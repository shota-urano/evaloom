package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestCancelledResultRetriesWhileHeartbeatKeepsExecutionRegistered(t *testing.T) {
	var claims, attempts atomic.Int32
	var running, allowDelivery atomic.Bool
	attempted := make(chan struct{}, 1)
	reported := make(chan ExecutionResult, 1)
	stillRegistered := make(chan struct{}, 1)
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			input := readHeartbeat(t, r)
			if running.Load() && len(input.RunningExecutionIDs) != 0 {
				if attempts.Load() > 0 {
					select {
					case stillRegistered <- struct{}{}:
					default:
					}
				}
				io.WriteString(w, `{"cancelExecutionIds":["`+logExecution+`"]}`)
			} else {
				io.WriteString(w, `{"cancelExecutionIds":[]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/claim"):
			if claims.Add(1) == 1 {
				io.WriteString(w, `{"execution":{"id":"`+logExecution+`","taskId":"task"}}`)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasSuffix(r.URL.Path, "/result"):
			if r.Header.Get("Authorization") != "Bearer "+testToken || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
				t.Error("result request method or headers incorrect")
			}
			var result ExecutionResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Error(err)
			}
			if result.Status == model.ExecutionCancelled {
				if attempts.Add(1) == 1 {
					attempted <- struct{}{}
				}
				if !allowDelivery.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				reported <- result
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	control := testExecutionControl()
	control.Reporter = process
	control.CheckCancellation = process.Heartbeat
	var harnessRuns atomic.Int32
	executor, err := NewExecutorWithControl(stubStageHandlers(func(id StageID, ctx context.Context, _ *Execution) error {
		if id == StageHarness {
			harnessRuns.Add(1)
			running.Store(true)
			<-ctx.Done()
			return ctx.Err()
		}
		if id == StagePublish {
			t.Error("publish ran after cancellation")
		}
		return nil
	}), control)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	startPull(t, process, func(ctx context.Context, claim Claim) {
		done <- executor.Execute(ctx, claim)
	}, func(err error) { t.Error(err) })
	// Ensure cleanup can finish delivery even if an assertion fails.
	t.Cleanup(func() { allowDelivery.Store(true) })
	await(t, attempted)
	await(t, stillRegistered)
	select {
	case <-done:
		t.Fatal("execution returned before pending result was delivered")
	default:
	}
	allowDelivery.Store(true)
	result := await(t, reported)
	if result.FromStatus != model.ExecutionRunning || result.Status != model.ExecutionCancelled || result.ErrorClass != model.ErrorCancelled {
		t.Fatalf("incorrect cancellation report: %+v", result)
	}
	if err := await(t, done); !errors.Is(err, context.Canceled) || attempts.Load() < 2 || harnessRuns.Load() != 1 {
		t.Fatalf("retry reran stage or lost error: attempts=%d stageRuns=%d error=%v", attempts.Load(), harnessRuns.Load(), err)
	}
}

func TestReportResultReturnsConflictInsteadOfAcceptingAmbiguousAcknowledgement(t *testing.T) {
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	err := process.ReportResult(context.Background(), logExecution, ExecutionResult{
		FromStatus: model.ExecutionRunning, Status: model.ExecutionTimedOut, ErrorClass: model.ErrorTimeout,
	})
	if err == nil || err.Error() != "runner: result returned HTTP 409" {
		t.Fatalf("ambiguous acknowledgement was accepted: %v", err)
	}
}
