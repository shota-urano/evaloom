package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/stage"
)

// StageCancelGrace is the fixed policy of spec 05 3.10, also supplied to
// Adapter Config.CancelGrace by work-stage wiring. It is not a setting.
const StageCancelGrace = sandbox.TerminationGrace

func stageInterruption(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancelled"
}

// The Control Plane still validates the result. Preserve a program's own
// cancel_check result instead of replacing it with an infrastructure error.
func cooperativeCancellation(ctx context.Context, stage model.Stage, outcome StageOutcome) bool {
	if ctx.Err() != context.Canceled || stage == model.StageWork ||
		(outcome.RunnerError != nil && *outcome.RunnerError != "cancelled") {
		return false
	}
	var result struct {
		Status     string `json:"status"`
		ErrorClass string `json:"errorClass"`
	}
	return json.Unmarshal(outcome.Result, &result) == nil && result.Status == "failed" && result.ErrorClass == "cancelled"
}

// RunProgram is the program executor's supervision boundary. launch must be
// Container.RunControlled (or a fake with the same ownership semantics), never
// a host-process fallback. Mounts, guest paths and CPU/memory come from wiring;
// the pinned Profile supplies the command and claim supplies the time limit.
// Collectors read result.json/out and finish logs after this returns.
func (in StageInvocation) RunProgram(ctx context.Context, launch func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error), spec sandbox.SandboxSpec) StageOutcome {
	code := "config_error"
	program, err := stage.Program(in.Profile.Definition, in.Stage)
	if err != nil || program == nil || launch == nil || len(program.Command) == 0 || in.timeout <= 0 {
		return StageOutcome{RunnerError: &code}
	}
	spec.Kind, spec.Limits.Time = sandbox.SandboxKind(in.Stage), in.timeout
	spec, err = in.SandboxSpec(spec)
	if err != nil {
		return StageOutcome{RunnerError: &code}
	}
	process, err := launch(ctx, spec, program.Command)
	if err != nil || process == nil {
		code = "sandbox_error"
		if ctx.Err() != nil {
			code = stageInterruption(ctx.Err())
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			code = stageInterruption(err)
		}
		outcome := StageOutcome{RunnerError: &code}
		if code == "sandbox_error" {
			outcome.Err = err
			if err == nil {
				outcome.Err = errors.New("sandbox: runtime returned no process")
			}
		}
		return outcome
	}
	err = waitProgram(ctx, process)
	outcome := StageOutcome{}
	if exit, confirmed := process.ExitCode(); confirmed {
		outcome.ExitCode = &exit
	} else if err == nil && ctx.Err() == nil {
		code = "sandbox_error"
		outcome.RunnerError = &code
		outcome.Err = errors.New("sandbox: workload exit status unconfirmed")
	}
	if ctx.Err() != nil {
		code = stageInterruption(ctx.Err())
		outcome.RunnerError = &code
	} else {
		var exit *sandbox.ExitError
		if err != nil && !errors.As(err, &exit) {
			code = "sandbox_error"
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				code = stageInterruption(err)
			} else {
				outcome.Err = err
			}
			outcome.RunnerError = &code
		}
	}
	return outcome
}

func waitProgram(ctx context.Context, process sandbox.Process) error {
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// Stop may spend time in a runtime CLI. Start the grace timer first and
	// do not let a stuck TERM request postpone forced removal.
	timer := time.NewTimer(StageCancelGrace)
	defer timer.Stop()
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = process.Stop() }()
	defer func() { <-stopped }()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = process.Kill()
		return <-done // Capacity and collectors remain owned until confirmed stop.
	}
}

// WaitWork cancels through the selected Adapter and waits for its final result,
// including Sandbox termination and authentication writeback. Work wiring must
// use StageCancelGrace for the Adapter's TERM/KILL policy and map returned
// interruption errors to StageOutcome.RunnerError. The Adapter's execution
// result, not the context after writeback, determines whether work was stopped.
func (in StageInvocation) WaitWork(ctx context.Context, harness adapter.Adapter, id adapter.SessionID) (adapter.HarnessResult, error) {
	if in.Stage != model.StageWork || harness == nil || id == "" {
		return adapter.HarnessResult{}, fmt.Errorf("runner: work session is required")
	}
	type reply struct {
		result adapter.HarnessResult
		err    error
	}
	done := make(chan reply, 1)
	go func() {
		result, err := harness.Result(id)
		done <- reply{result, err}
	}()
	var finished reply
	var cancelErr error
	select {
	case finished = <-done:
	case <-ctx.Done():
		cancelErr = harness.Cancel(id)
		finished = <-done
	}
	if in.measurement != nil {
		in.measurement.work = &finished.result.Usage
	}
	if finished.err != nil {
		return finished.result, errors.Join(finished.err, cancelErr)
	}
	// Cancel can race an already-finished CLI whose credential recovery is
	// still pending. A retrieved result remains authoritative even then.
	if finished.result.ErrorClass != nil {
		switch *finished.result.ErrorClass {
		case adapter.ErrorTimeout:
			return finished.result, context.DeadlineExceeded
		case adapter.ErrorCancelled:
			return finished.result, context.Canceled
		}
	}
	return finished.result, nil
}
