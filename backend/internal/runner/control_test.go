package runner

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type reportFunc func(context.Context, string, ExecutionResult) error

func (report reportFunc) ReportResult(ctx context.Context, id string, result ExecutionResult) error {
	return report(ctx, id, result)
}

func testExecutionControl() ExecutionControl {
	return ExecutionControl{
		Timeouts:    Timeouts{Setup: time.Second, Harness: time.Second, Test: time.Second},
		CancelGrace: time.Millisecond, ReportContext: context.Background(),
		Reporter:          reportFunc(func(context.Context, string, ExecutionResult) error { return nil }),
		CheckCancellation: func(context.Context) error { return nil },
	}
}

func TestControlledExecutorRequiresInjectedPolicy(t *testing.T) {
	for _, mutate := range []func(*ExecutionControl){
		func(c *ExecutionControl) { c.Timeouts.Setup = 0 },
		func(c *ExecutionControl) { c.Timeouts.Harness = -1 },
		func(c *ExecutionControl) { c.Timeouts.Test = 0 },
		func(c *ExecutionControl) { c.CancelGrace = 0 },
		func(c *ExecutionControl) { c.ReportContext = nil },
		func(c *ExecutionControl) { c.Reporter = nil },
		func(c *ExecutionControl) { c.CheckCancellation = nil },
	} {
		control := testExecutionControl()
		mutate(&control)
		if _, err := NewExecutorWithControl(stubStageHandlers(func(StageID, context.Context, *Execution) error {
			return nil
		}), control); err == nil {
			t.Fatal("missing/invalid policy accepted")
		}
	}
}

func TestStageTimeoutsProjectOverridesAndIndependentDeadlines(t *testing.T) {
	control := testExecutionControl()
	control.Timeouts = Timeouts{Setup: 4 * time.Second, Harness: 5 * time.Second, Test: 6 * time.Second}
	for _, project := range []map[string]int{nil, {"setupSec": 1, "harnessSec": 2, "testSec": 3}, {"harnessSec": 2}} {
		execution := &Execution{control: &control}
		execution.Claim.Project.Timeouts = project
		for _, tc := range []struct {
			stage StageID
			key   string
			limit time.Duration
		}{
			{StageSetup, "setupSec", control.Timeouts.Setup},
			{StageHarness, "harnessSec", control.Timeouts.Harness},
			{StageTestBaseline, "testSec", control.Timeouts.Test},
			{StageGate2, "testSec", control.Timeouts.Test},
		} {
			limit := tc.limit
			if seconds, exists := project[tc.key]; exists {
				limit = time.Duration(seconds) * time.Second
			}
			before := time.Now()
			ctx, cancel := execution.stageContext(context.Background(), tc.stage)
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Before(before.Add(limit)) || deadline.After(time.Now().Add(limit)) {
				t.Fatalf("%s deadline=%v; want fresh %v limit", tc.stage, deadline, limit)
			}
			cancel()
		}
		ctx, cancel := execution.stageContext(context.Background(), StageGate1)
		if _, limited := ctx.Deadline(); limited || ctx.Err() != nil {
			t.Fatal("an unconfigured stage inherited a prior stage's deadline")
		}
		cancel()
	}
	for _, project := range []map[string]int{{"testSec": 0}, {"harnessSec": -1}, {"unknown": 1}, {"setupSec": int(^uint(0) >> 1)}} {
		if err := control.validateTimeouts(project); err == nil {
			t.Fatalf("invalid/overflowing project timeouts accepted: %v", project)
		}
	}
}

func TestControlledExecutorReportsCancellationEvenWhenStageReturnsNil(t *testing.T) {
	for _, stage := range []StageID{StageValidateProject, StageSetup, StageHarness, StageGate2, StageComplete} {
		t.Run(string(stage), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := testExecutionControl()
			var reports []ExecutionResult
			control.Reporter = reportFunc(func(ctx context.Context, _ string, result ExecutionResult) error {
				if ctx.Err() != nil {
					t.Fatal("result reused the cancelled execution context")
				}
				reports = append(reports, result)
				return nil
			})
			var last StageID
			executor, err := NewExecutorWithControl(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
				last = id
				if id == stage {
					cancel()
				}
				return nil
			}), control)
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.Execute(ctx, Claim{}); !errors.Is(err, context.Canceled) || last != stage {
				t.Fatalf("continued after cancellation: last=%s error=%v", last, err)
			}
			if result := reports[len(reports)-1]; result.Status != model.ExecutionCancelled || result.ErrorClass != model.ErrorCancelled {
				t.Fatalf("wrong final report: %+v", result)
			}
		})
	}
}

func TestControlledExecutorChecksCancellationBeforePublishAndFailsClosed(t *testing.T) {
	for _, outcome := range []string{"cancelled", "offline", "allowed"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := testExecutionControl()
			checkErr := errors.New("control plane unavailable")
			var checked, published bool
			var reports []ExecutionResult
			control.Reporter = reportFunc(func(_ context.Context, _ string, result ExecutionResult) error {
				reports = append(reports, result)
				return nil
			})
			control.CheckCancellation = func(context.Context) error {
				checked = true
				switch outcome {
				case "cancelled":
					cancel()
				case "offline":
					return checkErr
				}
				return nil
			}
			executor, err := NewExecutorWithControl(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
				if id == StagePublish {
					if !checked {
						t.Fatal("push started before cancellation check")
					}
					published = true
				}
				return nil
			}), control)
			if err != nil {
				t.Fatal(err)
			}
			claim := Claim{}
			claim.Execution.ResumePoint = new("publish")
			err = executor.Execute(ctx, claim)
			if !checked || published != (outcome == "allowed") {
				t.Fatalf("checked=%v published=%v", checked, published)
			}
			switch outcome {
			case "cancelled":
				if !errors.Is(err, context.Canceled) || reports[0].Status != model.ExecutionCancelled {
					t.Fatalf("cancelled push: error=%v reports=%v", err, reports)
				}
			case "offline":
				if !errors.Is(err, checkErr) || len(reports) != 0 {
					t.Fatalf("offline push: error=%v reports=%v", err, reports)
				}
			case "allowed":
				want := []ExecutionResult{
					{FromStatus: model.ExecutionClaimed, Status: model.ExecutionPublishing},
					{FromStatus: model.ExecutionPublishing, Status: model.ExecutionSucceeded},
				}
				if err != nil || !reflect.DeepEqual(reports, want) {
					t.Fatalf("successful push: error=%v reports=%v", err, reports)
				}
			}
		})
	}
}

type blockedCancel struct{ release <-chan struct{} }

func (cancel blockedCancel) Cancel(adapter.SessionID) error { <-cancel.release; return nil }

type controlledSandbox struct {
	done, stopped, killed chan struct{}
}

func (p controlledSandbox) Wait() error { <-p.done; return nil }
func (p controlledSandbox) Stop() error {
	close(p.stopped)
	return errors.New("sensitive stop failure")
}
func (p controlledSandbox) Kill() error { close(p.killed); return errors.New("sensitive kill failure") }

func TestStuckAdapterAndFailedSignalsStillWaitForSandboxExit(t *testing.T) {
	control := testExecutionControl()
	execution := &Execution{control: &control}
	process := controlledSandbox{done: make(chan struct{}), stopped: make(chan struct{}), killed: make(chan struct{})}
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- execution.WaitHarness(ctx, process, blockedCancel{release}, "session") }()
	await(t, process.stopped)
	await(t, process.killed)
	select {
	case err := <-done:
		t.Fatalf("released execution before sandbox stopped: %v", err)
	default:
	}
	close(process.done)
	if err := await(t, done); !errors.Is(err, context.Canceled) || err.Error() != "context canceled\nrunner: sandbox cancellation request failed" {
		t.Fatalf("lost cancellation or exposed external error: %v", err)
	}
}

func TestNaturalSandboxExitDoesNotSendStopOrKill(t *testing.T) {
	control := testExecutionControl()
	execution := &Execution{control: &control}
	process := controlledSandbox{done: make(chan struct{}), stopped: make(chan struct{}), killed: make(chan struct{})}
	close(process.done)
	if err := execution.WaitProcess(context.Background(), process); err != nil {
		t.Fatal(err)
	}
	for _, requested := range []chan struct{}{process.stopped, process.killed} {
		select {
		case <-requested:
			t.Fatal("sent a stop/kill request to a sandbox that exited normally")
		default:
		}
	}
}
