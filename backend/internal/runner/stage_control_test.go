package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type supervisedProcess struct {
	done        chan struct{}
	once        sync.Once
	term, kill  time.Time
	cooperative bool
	blockStop   bool
}

func (p *supervisedProcess) Wait() error { <-p.done; return nil }
func (p *supervisedProcess) Stop() error {
	p.term = time.Now()
	if p.cooperative {
		p.once.Do(func() { close(p.done) })
	}
	if p.blockStop {
		<-p.done
	}
	return nil
}
func (p *supervisedProcess) Kill() error {
	p.kill = time.Now()
	p.once.Do(func() { close(p.done) })
	return nil
}
func (*supervisedProcess) ExitCode() (int, bool) { return 0, false }

func TestProgramTerminationGrace(t *testing.T) {
	for _, mode := range []string{"cooperative", "ignore-term", "blocked-term"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				process := &supervisedProcess{done: make(chan struct{}), cooperative: mode == "cooperative", blockStop: mode == "blocked-term"}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() { time.Sleep(time.Second); cancel() }()
				if err := waitProgram(ctx, process); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if process.term.IsZero() {
					t.Fatal("TERM was not requested")
				}
				if mode == "cooperative" {
					if !process.kill.IsZero() || time.Since(process.term) != 0 {
						t.Fatal("cooperative stop waited for KILL")
					}
				} else if process.kill.Sub(process.term) != 10*time.Second {
					t.Fatalf("KILL after %v; want exactly 10s", process.kill.Sub(process.term))
				}
			})
		})
	}
}

type cancellableAdapter struct {
	adapter.Adapter
	cancelled chan adapter.SessionID
	stopped   chan struct{}
	class     adapter.ErrorClass
}

func (a *cancellableAdapter) Cancel(id adapter.SessionID) error {
	a.cancelled <- id
	return nil
}
func (a *cancellableAdapter) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	<-a.stopped
	class := a.class
	if class == "" {
		class = adapter.ErrorCancelled
	}
	return adapter.HarnessResult{SessionID: id, Status: adapter.ResultFailed, ErrorClass: &class}, nil
}

type finishingAdapter struct {
	adapter.Adapter
	result    adapter.HarnessResult
	cancelled bool
}

func (a *finishingAdapter) Cancel(adapter.SessionID) error { a.cancelled = true; return nil }
func (a *finishingAdapter) Result(adapter.SessionID) (adapter.HarnessResult, error) {
	// The CLI has already finished; only authentication recovery remains.
	time.Sleep(1200 * time.Millisecond)
	return a.result, nil
}

func TestWorkRecoveryDoesNotChangeFinishedResult(t *testing.T) {
	for _, interruption := range []string{"timeout", "cancelled"} {
		for _, class := range []adapter.ErrorClass{"", adapter.ErrorExecutionFailed, adapter.ErrorConfig, adapter.ErrorTimeout, adapter.ErrorCancelled} {
			name := string(class)
			if name == "" {
				name = "succeeded"
			}
			t.Run(interruption+"/"+name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					result := adapter.HarnessResult{SessionID: "finished", Status: adapter.ResultSucceeded}
					if class != "" {
						result.Status, result.ErrorClass = adapter.ResultFailed, &class
					}
					a := &finishingAdapter{result: result}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					if interruption == "cancelled" {
						cancel()
						ctx, cancel = context.WithCancel(context.Background())
						go func() { time.Sleep(time.Second); cancel() }()
					}
					defer cancel()
					got, err := (StageInvocation{Stage: model.StageWork}).WaitWork(ctx, a, "finished")
					var wantErr error
					if class == adapter.ErrorTimeout {
						wantErr = context.DeadlineExceeded
					} else if class == adapter.ErrorCancelled {
						wantErr = context.Canceled
					}
					if !errors.Is(err, wantErr) || got.Status != result.Status || got.ErrorClass != result.ErrorClass || got.SessionID != result.SessionID {
						t.Fatalf("recovery changed finished result: %+v, err=%v", got, err)
					}
					if !a.cancelled || ctx.Err() == nil {
						t.Fatal("test did not interrupt authentication recovery")
					}
				})
			})
		}
	}
}

func TestWorkCancellationWaitsForAdapterStop(t *testing.T) {
	for _, timeout := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			a := &cancellableAdapter{cancelled: make(chan adapter.SessionID, 1), stopped: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			if timeout {
				a.class = adapter.ErrorTimeout
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			} else {
				go func() { time.Sleep(time.Second); cancel() }()
			}
			defer cancel()
			go func() {
				if id := <-a.cancelled; id != "session" {
					t.Errorf("wrong cancelled session: %s", id)
				}
				time.Sleep(StageCancelGrace)
				close(a.stopped)
			}()
			start := time.Now()
			result, err := (StageInvocation{Stage: model.StageWork}).WaitWork(ctx, a, "session")
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || result.SessionID != "session" || time.Since(start) != 11*time.Second {
				t.Fatalf("result=%+v err=%v elapsed=%v", result, err, time.Since(start))
			}
		})
	}
}

func TestWorkStageReportsTimeoutAndCancellation(t *testing.T) {
	for _, want := range []string{"timeout", "cancelled"} {
		t.Run(want, func(t *testing.T) {
			timeout := want == "timeout"
			r, claim, reports, services := stageFixture(t, false)
			claim.Limits.TimeoutSec["work"] = 1
			services.Reporter = liveStageReporter{reports}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := &cancellableAdapter{cancelled: make(chan adapter.SessionID, 1), stopped: make(chan struct{})}
			if timeout {
				a.class = adapter.ErrorTimeout
			}
			go func() { <-a.cancelled; close(a.stopped) }()
			services.Executor = stageExecutorFunc(func(stageCtx context.Context, in StageInvocation) StageOutcome {
				if in.Stage != model.StageWork {
					t.Fatalf("unexpected later stage: %s", in.Stage)
				}
				if !timeout {
					cancel()
				}
				result, err := in.WaitWork(stageCtx, a, "work-session")
				if err == nil || result.SessionID != "work-session" {
					t.Errorf("wrong adapter termination: %+v %v", result, err)
				}
				code := stageInterruption(err)
				return StageOutcome{RunnerError: &code, Work: &StageWork{SessionID: stagePtr(string(result.SessionID))}}
			})
			if err := r.ExecuteStages(ctx, claim, services); err != nil {
				t.Fatal(err)
			}
			if len(reports.outcomes) != 1 || reports.outcomes[0].RunnerError == nil || *reports.outcomes[0].RunnerError != want {
				t.Fatalf("wrong stage reports: %+v", reports.outcomes)
			}
			if data, err := os.ReadFile(filepath.Join(claim.Workspace.Path, "keep")); err != nil || string(data) != "workspace survives" {
				t.Fatalf("work stage did not retain workspace: %s %v", data, err)
			}
		})
	}
}
