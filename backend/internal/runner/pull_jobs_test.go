package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPullSharesSlotsAndDispatchesJobKinds(t *testing.T) {
	for _, kinds := range [][2]string{
		{"execution", "route_proposal"},
		{"route_proposal", "execution"},
		{"route_proposal", "route_proposal"},
	} {
		t.Run(strings.Join(kinds[:], "/"), func(t *testing.T) {
			var claims, active, peak atomic.Int32
			heartbeats := make(chan heartbeatInput, 128)
			process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/heartbeat") {
					heartbeats <- readHeartbeat(t, r)
					io.WriteString(w, `{"cancelExecutionIds":[],"cancelRouteProposalIds":[]}`)
					return
				}
				n := claims.Add(1)
				if kinds[(n-1)%2] == "execution" {
					fmt.Fprintf(w, `{"job":{"kind":"execution","execution":{"id":"job%d","taskId":"task"},"profile":{"definition":{"kept":true}}}}`, n)
				} else {
					fmt.Fprintf(w, `{"job":{"kind":"route_proposal","proposal":{"id":"job%d","taskId":"task","trigger":"no_label","task":{"title":"title","body":"body"},"candidates":[{"label":"route:project","kind":"project","id":"project","description":"description"}],"proposer":{"microHarness":"codex","model":"model"},"timeoutSec":300}}}`, n)
				}
			})
			type work struct {
				id, kind string
				release  chan struct{}
			}
			started := make(chan work, 8)
			handle := func(ctx context.Context, kind, id string) {
				current := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); current > old; old = peak.Load() {
					if peak.CompareAndSwap(old, current) {
						break
					}
				}
				release := make(chan struct{})
				started <- work{id, kind, release}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			startJobs(t, process, JobHandlers{
				Execution: func(ctx context.Context, claim Claim) {
					if claim.Kind != "execution" || claim.Execution.TaskID != "task" || string(claim.Profile.Definition) != `{"kept":true}` {
						t.Error("execution payload was lost or sent to the wrong handler")
					}
					handle(ctx, "execution", claim.Execution.ID)
				},
				RouteProposal: func(ctx context.Context, claim ProposalClaim) {
					if claim.TaskID != "task" || claim.Trigger != "no_label" || claim.Task.Title != "title" || claim.Task.Body != "body" || claim.TimeoutSec != 300 ||
						string(claim.Candidates) != `[{"label":"route:project","kind":"project","id":"project","description":"description"}]` ||
						string(claim.Proposer) != `{"microHarness":"codex","model":"model"}` {
						t.Errorf("proposal payload was lost: %+v", claim)
					}
					handle(ctx, "route_proposal", claim.ID)
				},
			}, func(err error) { t.Error(err) })
			first, second := await(t, started), await(t, started)
			if first.id > second.id {
				first, second = second, first
			}
			if first.id != "job1" || first.kind != kinds[0] || second.id != "job2" || second.kind != kinds[1] {
				t.Fatalf("incorrect dispatch: first=%+v second=%+v", first, second)
			}
			for full := 0; full < 5; {
				hb := await(t, heartbeats)
				if len(hb.RunningExecutionIDs)+len(hb.RunningRouteProposalIDs) != 2 {
					continue
				}
				for _, job := range []work{first, second} {
					ids := hb.RunningExecutionIDs
					if job.kind == "route_proposal" {
						ids = hb.RunningRouteProposalIDs
					}
					if !strings.Contains(strings.Join(ids, ","), job.id) {
						t.Fatalf("job missing from heartbeat: %+v", hb)
					}
				}
				full++
			}
			if claims.Load() != 2 || active.Load() != 2 || peak.Load() != 2 {
				t.Fatalf("claims=%d active=%d peak=%d; want 2 each", claims.Load(), active.Load(), peak.Load())
			}
			if err := process.RegisterExecution("external", func() {}); err == nil {
				t.Fatal("execution registration exceeded the shared limit")
			}
			close(first.release)
			third := await(t, started)
			if third.id != "job3" || third.kind != kinds[0] || claims.Load() != 3 || peak.Load() != 2 {
				t.Fatalf("slot not reused correctly: third=%+v claims=%d peak=%d", third, claims.Load(), peak.Load())
			}
		})
	}
}

func TestPullRepeatedNoWorkStartsNoHandlers(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var claims atomic.Int32
			polled := make(chan struct{}, 32)
			process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/heartbeat") {
					io.WriteString(w, `{"cancelExecutionIds":[],"cancelRouteProposalIds":[]}`)
					return
				}
				claims.Add(1)
				w.WriteHeader(code)
				if code == http.StatusOK {
					io.WriteString(w, `{"job":null}`)
				}
				polled <- struct{}{}
			})
			var started atomic.Int32
			startJobs(t, process, JobHandlers{
				Execution:     func(context.Context, Claim) { started.Add(1) },
				RouteProposal: func(context.Context, ProposalClaim) { started.Add(1) },
			}, func(err error) { t.Error(err) })
			for range 6 {
				await(t, polled)
			}
			if claims.Load() < 6 || started.Load() != 0 {
				t.Fatalf("claims=%d started=%d", claims.Load(), started.Load())
			}
		})
	}
}

func TestPullCancelsProposalWithoutReleasingItsSlotOrCancellingExecution(t *testing.T) {
	var claims atomic.Int32
	var cancelProposal atomic.Bool
	full := make(chan heartbeatInput, 128)
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			input := readHeartbeat(t, r)
			if cancelProposal.Load() {
				full <- input
				io.WriteString(w, `{"cancelExecutionIds":[],"cancelRouteProposalIds":["shared-id","unknown"]}`)
			} else {
				io.WriteString(w, `{"cancelExecutionIds":[],"cancelRouteProposalIds":[]}`)
			}
			return
		}
		switch claims.Add(1) {
		case 1:
			io.WriteString(w, `{"job":{"kind":"execution","execution":{"id":"shared-id","taskId":"task"}}}`)
		case 2:
			io.WriteString(w, `{"job":{"kind":"route_proposal","proposal":{"id":"shared-id","taskId":"task"}}}`)
		default:
			io.WriteString(w, `{"job":{"kind":"route_proposal","proposal":{"id":"replacement","taskId":"task"}}}`)
		}
	})
	executionStarted := make(chan context.Context, 1)
	proposalStarted, canceled := make(chan string, 8), make(chan struct{}, 1)
	release := make(chan struct{})
	shutdown, stop := context.WithCancel(context.Background())
	startJobs(t, process, JobHandlers{
		Execution: func(ctx context.Context, _ Claim) {
			executionStarted <- ctx
			<-ctx.Done()
		},
		RouteProposal: func(ctx context.Context, claim ProposalClaim) {
			proposalStarted <- claim.ID
			if claim.ID == "shared-id" {
				<-ctx.Done()
				canceled <- struct{}{}
				select {
				case <-release:
				case <-shutdown.Done():
				}
			} else {
				<-ctx.Done()
			}
		},
	}, func(err error) { t.Error(err) })
	t.Cleanup(stop)
	executionCtx := await(t, executionStarted)
	await(t, proposalStarted)
	cancelProposal.Store(true)
	await(t, canceled)
	for range 5 {
		hb := await(t, full)
		if !reflect.DeepEqual(hb.RunningExecutionIDs, []string{"shared-id"}) || !reflect.DeepEqual(hb.RunningRouteProposalIDs, []string{"shared-id"}) {
			t.Fatalf("cancelled proposal disappeared before it stopped: %+v", hb)
		}
	}
	if executionCtx.Err() != nil || claims.Load() != 2 {
		t.Fatalf("proposal cancellation affected execution or freed slot: execution=%v claims=%d", executionCtx.Err(), claims.Load())
	}
	close(release)
	if id := await(t, proposalStarted); id != "replacement" {
		t.Fatalf("expected replacement proposal, got %s", id)
	}
	for {
		hb := await(t, full)
		if reflect.DeepEqual(hb.RunningRouteProposalIDs, []string{"replacement"}) {
			break
		}
	}
}

func TestRunJobsRequiresBothHandlersBeforeClaiming(t *testing.T) {
	process := newPullRunner(t, func(http.ResponseWriter, *http.Request) {
		t.Error("contacted Control Plane without both handlers")
	})
	for _, handlers := range []JobHandlers{
		{},
		{Execution: func(context.Context, Claim) {}},
		{RouteProposal: func(context.Context, ProposalClaim) {}},
	} {
		if err := process.RunJobs(context.Background(), handlers, nil); err == nil {
			t.Fatal("accepted incomplete job handlers")
		}
	}
}
