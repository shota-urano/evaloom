package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestProposalArtifactRetryStops(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		for _, kind := range []string{"log", "proposal_result"} {
			for _, stop := range []string{"job_cancel", "runner_shutdown"} {
				t.Run(name+"/"+kind+"/"+stop, func(t *testing.T) {
					claim := proposalClaim(ulid.Make().String(), 30)
					claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"m"}`, name))
					attempts := make(chan string, 32)
					var claimed atomic.Bool
					var results atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						switch {
						case strings.HasSuffix(req.URL.Path, "/heartbeat"):
							_, _ = io.WriteString(w, `{"cancelExecutionIds":[],"cancelRouteProposalIds":[]}`)
						case strings.HasSuffix(req.URL.Path, "/claim"):
							if claimed.Swap(true) {
								_, _ = io.WriteString(w, `{"job":null}`)
							} else {
								_ = json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"kind": "route_proposal", "proposal": claim}})
							}
						case strings.HasSuffix(req.URL.Path, "/artifacts") && req.URL.Query().Get("kind") == kind:
							attempts <- req.URL.Query().Get("artifactId")
							w.WriteHeader(http.StatusServiceUnavailable)
						case strings.HasSuffix(req.URL.Path, "/result"):
							results.Add(1)
						default:
							w.WriteHeader(http.StatusCreated)
						}
					}))
					defer server.Close()
					config, _ := proposalRuntimeConfig(t, testConfig(t, server.URL), "success")
					r, err := New(config)
					if err != nil {
						t.Fatal(err)
					}
					r.pollInterval = 10 * time.Millisecond
					services := ProposalServicesFromConfig(config)
					services.Adapters = proposalAdapters
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() {
						if stop == "job_cancel" {
							done <- r.ExecuteProposal(ctx, claim, services)
						} else {
							done <- r.RunJobs(ctx, JobHandlers{
								Execution: func(context.Context, Claim) { t.Error("unexpected execution") },
								RouteProposal: func(ctx context.Context, claim ProposalClaim) {
									if err := r.ExecuteProposal(ctx, claim, services); err != nil {
										t.Errorf("proposal: %v", err)
									}
								},
							}, func(err error) { t.Errorf("pull loop: %v", err) })
						}
					}()
					id := await(t, attempts)
					if retry := await(t, attempts); retry != id {
						t.Fatal("retry changed artifact identity")
					}
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("artifact retry prevented cancellation/shutdown")
					}
					if results.Load() != 0 {
						t.Fatal("reported success before artifact delivery")
					}
					directory := "logs"
					if kind == "proposal_result" {
						directory = "artifacts"
					}
					filename := id
					if kind == "log" {
						filename += ".log"
					}
					data, err := os.ReadFile(filepath.Join(config.DataDir, directory, filename))
					if err != nil || len(data) == 0 {
						t.Fatalf("saved artifact lost: %v", err)
					}
					if stop == "runner_shutdown" {
						r.mu.Lock()
						active := len(r.proposals)
						r.mu.Unlock()
						if active != 0 {
							t.Fatal("Runner did not release proposal slot")
						}
					}
				})
			}
		}
	}
}
