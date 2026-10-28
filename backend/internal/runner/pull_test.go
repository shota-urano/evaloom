package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for pull loop")
		var zero T
		return zero
	}
}

func newPullRunner(t *testing.T, handler http.HandlerFunc) *Runner {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	process, err := New(testConfig(t, server.URL+"/control"))
	if err != nil {
		t.Fatal(err)
	}
	process.pollInterval = time.Millisecond
	return process
}

func startPull(t *testing.T, process *Runner, execute func(context.Context, Claim), onError func(error)) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- process.Run(ctx, execute, onError) }()
	t.Cleanup(func() {
		cancel()
		if err := await(t, done); err != nil {
			t.Error(err)
		}
		process.mu.Lock()
		defer process.mu.Unlock()
		if len(process.executions) != 0 || process.claimPending || process.loopRunning {
			t.Error("shutdown left executions, a claim reservation or a running loop")
		}
	})
	return ctx
}

func TestPullLimitsExecutionsEvenWhenServerAlwaysOffersWork(t *testing.T) {
	var claims, active, peak atomic.Int32
	heartbeats := make(chan heartbeatInput, 128)
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/api/runners/mac-mini/heartbeat":
			heartbeats <- readHeartbeat(t, r)
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
		case "/control/api/runners/mac-mini/claim":
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Content-Type") != "application/json" {
				t.Error("claim method or credentials incorrect")
			}
			n := claims.Add(1)
			fmt.Fprintf(w, `{"execution":{"id":"e%d","taskId":"t%d","seq":1,"mode":"new"},"profile":{"definition":{"kept":true}}}`, n, n)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	type work struct {
		id      string
		release chan struct{}
	}
	started := make(chan work, 8)
	startPull(t, process, func(ctx context.Context, claim Claim) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		if string(claim.Profile.Definition) != `{"kept":true}` {
			t.Error("claim input lost profile definition")
		}
		release := make(chan struct{})
		started <- work{claim.Execution.ID, release}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}, func(err error) { t.Error(err) })
	first, second := await(t, started), await(t, started)
	if first.id == second.id {
		t.Fatal("same execution started twice")
	}
	// Observe multiple completed polls with both callbacks blocked. A server
	// with no admission limit must still receive only two claim requests.
	for full := 0; full < 5; {
		hb := await(t, heartbeats)
		if len(hb.RunningExecutionIDs) == 2 {
			full++
		}
	}
	if claims.Load() != 2 || active.Load() != 2 || peak.Load() != 2 {
		t.Fatalf("claims=%d active=%d peak=%d; want 2 for each", claims.Load(), active.Load(), peak.Load())
	}
	close(first.release)
	third := await(t, started)
	if third.id == first.id || third.id == second.id || claims.Load() != 3 || peak.Load() != 2 {
		t.Fatalf("slot not correctly reused: third=%s claims=%d peak=%d", third.id, claims.Load(), peak.Load())
	}
	for {
		hb := await(t, heartbeats)
		if len(hb.RunningExecutionIDs) == 2 && !strings.Contains(strings.Join(hb.RunningExecutionIDs, ","), first.id) {
			break
		}
	}
}

func TestPullCancellationRetainsSlotUntilHandlerReturns(t *testing.T) {
	var claims atomic.Int32
	var cancelWork atomic.Bool
	heartbeats := make(chan heartbeatInput, 128)
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			heartbeats <- readHeartbeat(t, r)
			if cancelWork.Load() {
				io.WriteString(w, `{"cancelExecutionIds":["e1"]}`)
			} else {
				io.WriteString(w, `{"cancelExecutionIds":[]}`)
			}
			return
		}
		n := claims.Add(1)
		fmt.Fprintf(w, `{"execution":{"id":"e%d","taskId":"t%d"}}`, n, n)
	})
	started, canceled := make(chan string, 8), make(chan struct{}, 1)
	release := make(chan struct{})
	// A separate shutdown context releases delayed cancellation cleanup even
	// if an assertion fails before the test releases the execution.
	shutdown, stop := context.WithCancel(context.Background())
	startPull(t, process, func(ctx context.Context, claim Claim) {
		started <- claim.Execution.ID
		<-ctx.Done()
		if claim.Execution.ID == "e1" {
			canceled <- struct{}{}
			select {
			case <-release:
			case <-shutdown.Done():
			}
		}
	}, func(err error) { t.Error(err) })
	// Ensure the handler's cleanup escape is canceled before startPull waits.
	t.Cleanup(stop)
	await(t, started)
	await(t, started)
	cancelWork.Store(true)
	await(t, canceled)
	for full := 0; full < 5; {
		if len(await(t, heartbeats).RunningExecutionIDs) == 2 {
			full++
		}
	}
	if claims.Load() != 2 {
		t.Fatalf("cancellation freed an occupied slot: claims=%d", claims.Load())
	}
	close(release)
	if id := await(t, started); id != "e3" {
		t.Fatalf("expected replacement execution, got %s", id)
	}
}

func TestPullRetriesCommunicationWithoutStoppingWork(t *testing.T) {
	var heartbeats, claims atomic.Int32
	var boot atomic.Value
	var stage atomic.Int32
	started := make(chan context.Context, 2)
	errors := make(chan error, 16)
	recovered := make(chan struct{}, 1)
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			input := readHeartbeat(t, r)
			if previous := boot.Load(); previous != nil && previous != input.BootID {
				t.Error("boot identity changed on retry")
			}
			boot.Store(input.BootID)
			if heartbeats.Add(1) == 1 || stage.Load() == 1 {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
			return
		}
		n := claims.Add(1)
		if heartbeats.Load() < 2 {
			t.Error("claim ran before successful boot reconciliation")
		}
		if n == 1 {
			io.WriteString(w, `{"execution":{"id":"active","taskId":"task"}}`)
			return
		}
		if n == 2 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
		if n >= 4 && stage.Load() == 2 {
			select {
			case recovered <- struct{}{}:
			default:
			}
		}
	})
	startPull(t, process, func(ctx context.Context, _ Claim) { started <- ctx; <-ctx.Done() }, func(err error) {
		select {
		case errors <- err:
		default:
		}
	})
	if err := await(t, errors); !strings.Contains(err.Error(), "heartbeat returned HTTP 503") {
		t.Fatal(err)
	}
	workCtx := await(t, started)
	if err := await(t, errors); !strings.Contains(err.Error(), "claim returned HTTP 503") {
		t.Fatal(err)
	}
	stage.Store(1)
	await(t, errors)
	if workCtx.Err() != nil {
		t.Fatal("network failure stopped active execution")
	}
	stage.Store(2)
	await(t, recovered)
}

func TestClaimRejectsInvalidResponsesAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"unauthorized", 401, testToken},
		{"malformed", 200, testToken},
		{"null", 200, `null`},
		{"empty", 200, `{}`},
		{"missing task", 200, `{"execution":{"id":"e"}}`},
		{"trailing value", 200, `{"execution":{"id":"e","taskId":"t"}} {}`},
		{"oversize", 200, strings.Repeat(" ", 1<<20+1)},
		{"redirect", 307, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Bool
			process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					redirected.Store(true)
				}
				w.Header().Set("Location", "/target")
				w.WriteHeader(tc.code)
				io.WriteString(w, tc.body)
			})
			claim, err := process.claim(context.Background())
			if err == nil || claim != nil || strings.Contains(err.Error(), testToken) || redirected.Load() {
				t.Fatalf("invalid claim accepted, followed redirect or exposed token: %v", err)
			}
		})
	}
}

func TestPullShutdownInterruptsClaim(t *testing.T) {
	claimStarted := make(chan struct{})
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		close(claimStarted)
		<-r.Context().Done()
	})
	startPull(t, process, func(context.Context, Claim) { t.Error("execution started from interrupted claim") }, func(err error) { t.Error(err) })
	await(t, claimStarted)
}

func TestPullReservesCapacityWhileClaimIsInFlight(t *testing.T) {
	claimStarted, reply := make(chan struct{}), make(chan struct{})
	var claims atomic.Int32
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
			return
		}
		if claims.Add(1) == 1 {
			io.WriteString(w, `{"execution":{"id":"first","taskId":"task"}}`)
			return
		}
		close(claimStarted)
		select {
		case <-reply:
			io.WriteString(w, `{"execution":{"id":"second","taskId":"task"}}`)
		case <-r.Context().Done():
		}
	})
	started := make(chan struct{}, 1)
	startPull(t, process, func(ctx context.Context, claim Claim) {
		if claim.Execution.ID == "second" {
			started <- struct{}{}
		}
		<-ctx.Done()
	}, func(err error) { t.Error(err) })
	await(t, claimStarted)
	if err := process.RegisterExecution("third", func() {}); err == nil {
		t.Fatal("registration stole a slot reserved by an in-flight claim")
	}
	if err := process.Run(context.Background(), func(context.Context, Claim) {}, nil); err == nil {
		t.Fatal("second pull loop was allowed on the same Runner")
	}
	close(reply)
	await(t, started)
	if err := process.RegisterExecution("third", func() {}); err == nil {
		t.Fatal("third registration exceeded the execution limit")
	}
}
