package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Timeouts are injected global limits. Q15 has no agreed defaults. Project
// values (seconds) override these limits; baseline and Gate 2 share Test.
type Timeouts struct {
	Setup, Harness, Test time.Duration
}

// ExecutionControl supplies policy and reporting independently of the stage's
// cancellation context. ReportContext must outlive cancelled executions; the
// caller owns its shutdown and any unfinished delivery after it is cancelled.
// ReportResult must not acknowledge success until the report is accepted.
type ExecutionControl struct {
	Timeouts      Timeouts
	CancelGrace   time.Duration
	Reporter      ResultReporter
	ReportContext context.Context
	// Typically Runner.Heartbeat. A failed check prevents entering publishing;
	// a successful check dispatches any cancellation before the push handler.
	CheckCancellation func(context.Context) error
}

func NewExecutorWithControl(handlers map[StageID]StageFunc, control ExecutionControl) (*Executor, error) {
	if control.Timeouts.Setup <= 0 || control.Timeouts.Harness <= 0 || control.Timeouts.Test <= 0 ||
		control.CancelGrace <= 0 || control.Reporter == nil || control.ReportContext == nil || control.CheckCancellation == nil {
		return nil, fmt.Errorf("runner: stage limits, cancellation grace, reporter, report context and cancellation check are required")
	}
	executor, err := NewExecutor(handlers)
	if err != nil {
		return nil, err
	}
	executor.control = &control
	return executor, nil
}

func (control *ExecutionControl) validateTimeouts(project map[string]int) error {
	for key, seconds := range project {
		if (key != "setupSec" && key != "harnessSec" && key != "testSec") ||
			seconds <= 0 || uint64(seconds) > uint64((1<<63-1)/time.Second) {
			return fmt.Errorf("runner: invalid project timeout")
		}
	}
	return nil
}

func (execution *Execution) stageContext(ctx context.Context, stage StageID) (context.Context, context.CancelFunc) {
	if execution.control == nil {
		return ctx, func() {}
	}
	var key string
	var limit time.Duration
	switch stage {
	case StageSetup:
		key, limit = "setupSec", execution.control.Timeouts.Setup
	case StageHarness:
		key, limit = "harnessSec", execution.control.Timeouts.Harness
	case StageTestBaseline, StageGate2:
		key, limit = "testSec", execution.control.Timeouts.Test
	default:
		return ctx, func() {}
	}
	if seconds, ok := execution.Claim.Project.Timeouts[key]; ok {
		limit = time.Duration(seconds) * time.Second
	}
	return context.WithTimeout(ctx, limit)
}

func (execution *Execution) report(status model.ExecutionStatus, class model.ErrorClass) error {
	if status == execution.status {
		return nil
	}
	err := execution.control.Reporter.ReportResult(execution.control.ReportContext, execution.Claim.Execution.ID,
		ExecutionResult{FromStatus: execution.status, Status: status, ErrorClass: class, WorktreeID: execution.WorktreeID})
	if err == nil {
		execution.status = status
	}
	return err
}

func (execution *Execution) stageError(stage Stage, err error) error {
	if execution.control != nil {
		var status model.ExecutionStatus
		var class model.ErrorClass
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status, class = model.ExecutionTimedOut, model.ErrorTimeout
		case errors.Is(err, context.Canceled):
			status, class = model.ExecutionCancelled, model.ErrorCancelled
		}
		if status != "" {
			err = errors.Join(err, execution.report(status, class))
		}
	}
	return &StageError{Stage: stage, Err: err}
}

// SandboxProcess is the process-lifecycle boundary for Harness/Test Sandboxes.
// Wait is called exactly once and returns only after ALL sandbox processes have
// stopped, including children. Stop and Kill must be safe after Wait, return
// promptly, and target the whole sandbox. Kill must not remove the worktree.
// Sandbox creation/isolation belongs to spec 09, not to this supervisor.
type SandboxProcess interface {
	Wait() error
	Stop() error
	Kill() error
}

// SessionCanceller is satisfied by adapter.Adapter without depending on any
// concrete harness. Cancel should return promptly; a stuck Cancel does not
// prevent the supervisor from escalating to SandboxProcess.Kill.
type SessionCanceller interface {
	Cancel(adapter.SessionID) error
}

// WaitHarness must be called by the harness stage immediately after Start has
// returned its session/process handles, even if ctx was cancelled during Start.
// The adapter's process must NOT be automatically killed by exec.CommandContext:
// this supervisor owns the graceful cancellation and escalation sequence.
func (execution *Execution) WaitHarness(ctx context.Context, process SandboxProcess, harness SessionCanceller, session adapter.SessionID) error {
	if harness == nil || session == "" {
		return fmt.Errorf("runner: harness cancellation handle is required")
	}
	return execution.waitProcess(ctx, process, func() error { return harness.Cancel(session) })
}

// WaitProcess applies the same stop policy to setup and test sandboxes.
func (execution *Execution) WaitProcess(ctx context.Context, process SandboxProcess) error {
	return execution.waitProcess(ctx, process, nil)
}

func (execution *Execution) waitProcess(ctx context.Context, process SandboxProcess, cancelAdapter func() error) error {
	if execution.control == nil || process == nil {
		return fmt.Errorf("runner: process supervision requires execution control and a sandbox process")
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
	}

	// Start the grace period before invoking external cancellation code. Both
	// requests may run concurrently: a stuck adapter must not delay Sandbox Stop.
	timer := time.NewTimer(execution.control.CancelGrace)
	defer timer.Stop()
	requests := make(chan error, 2)
	if cancelAdapter != nil {
		go func() { requests <- cancelAdapter() }()
	}
	go func() { requests <- process.Stop() }()
	var stopErr error
	select {
	case <-done:
	case <-timer.C:
		stopErr = process.Kill()
		// Never report a terminal state or release Runner capacity while the
		// sandbox is still alive, even if Kill failed. Wait owns confirmation.
		<-done
	}
	for {
		select {
		case err := <-requests:
			stopErr = errors.Join(stopErr, err)
		default:
			if stopErr != nil {
				// External error text may contain secrets; retain only the cause
				// used for terminal classification and a safe diagnostic.
				return errors.Join(ctx.Err(), fmt.Errorf("runner: sandbox cancellation request failed"))
			}
			return ctx.Err()
		}
	}
}
