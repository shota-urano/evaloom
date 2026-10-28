package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// StageID identifies a step in spec 05, section 3.3. These are local handler
// keys, not API event types or additional resume_point values.
type StageID string

const (
	StageValidateProject StageID = "validate_project"
	StageCheckPreviousPR StageID = "check_previous_pr"
	StagePrepareWorktree StageID = "prepare_worktree"
	StagePinContext      StageID = "pin_context"
	StageReceiveContext  StageID = "receive_context"
	StageSetup           StageID = "setup"
	StageTestBaseline    StageID = "test_baseline"
	StageHarness         StageID = "harness"
	StageHarnessHead     StageID = "harness_head"
	StageGate1           StageID = "gate1"
	StageGate2           StageID = "gate2"
	StageGate3           StageID = "gate3"
	StagePublish         StageID = "publish"
	StageComplete        StageID = "complete"
)

// Stage describes the fixed order and Execution state in spec 05. Status is
// metadata for stage implementations/reporting, not a local state transition.
type Stage struct {
	Number int
	ID     StageID
	Status model.ExecutionStatus
}

// ExecutionStages returns a fresh table so callers cannot change the order
// used by an executor. Conditional work (for example checking a previous PR)
// belongs to its handler; the table itself is never reordered or shortened.
func ExecutionStages() []Stage {
	return []Stage{
		{1, StageValidateProject, model.ExecutionPreparing},
		{2, StageCheckPreviousPR, model.ExecutionPreparing},
		{3, StagePrepareWorktree, model.ExecutionPreparing},
		{4, StagePinContext, model.ExecutionPreparing},
		{5, StageReceiveContext, model.ExecutionPreparing},
		{6, StageSetup, model.ExecutionPreparing},
		{7, StageTestBaseline, model.ExecutionPreparing},
		{8, StageHarness, model.ExecutionRunning},
		{9, StageHarnessHead, model.ExecutionVerifying},
		{10, StageGate1, model.ExecutionVerifying},
		{11, StageGate2, model.ExecutionVerifying},
		{12, StageGate3, model.ExecutionVerifying},
		{13, StagePublish, model.ExecutionPublishing},
		{14, StageComplete, model.ExecutionSucceeded},
	}
}

// Execution is allocated once per Execute call and shared by its stages.
// Stage-specific worktree/context/result data can be added here as those
// implementations are introduced, without sharing mutable state between jobs.
type Execution struct {
	Claim Claim
	// Set by the worktree stage once its existing worktrees row is available.
	// The next progress/terminal report associates it with this Execution.
	WorktreeID string
	control    *ExecutionControl
	status     model.ExecutionStatus
}

// StageFunc performs one stage. A failure or block must return an error; the
// executor preserves that error for the caller's result reporting. Handlers
// own their conditional work and must honor ctx while they run.
type StageFunc func(ctx context.Context, execution *Execution) error

type executionStage struct {
	Stage
	run StageFunc
}

// Executor sequences stages. NewExecutorWithControl additionally enables
// stage deadlines, process supervision and progress/stop reports. Neither
// constructor deletes worktrees or retries stage handlers.
// It may be reused by concurrent Runner.Run callbacks if handlers are safe
// for concurrent calls and keep per-execution data in Execution.
type Executor struct {
	stages  []executionStage
	control *ExecutionControl
}

// NewExecutor requires every stage, including stages skipped when resuming.
// Missing implementations must never silently become successful no-ops.
// Copy handlers so later map changes cannot alter an in-flight execution.
func NewExecutor(handlers map[StageID]StageFunc) (*Executor, error) {
	table := ExecutionStages()
	stages := make([]executionStage, 0, len(table))
	for _, stage := range table {
		handler := handlers[stage.ID]
		if handler == nil {
			return nil, fmt.Errorf("runner: stage %d (%s) handler is required", stage.Number, stage.ID)
		}
		stages = append(stages, executionStage{stage, handler})
	}
	if len(handlers) != len(table) {
		return nil, fmt.Errorf("runner: unknown stage handler")
	}
	return &Executor{stages: stages}, nil
}

// StageError identifies the failing stage while retaining the original error
// (including a stage's block/error classification) through errors.Is/As.
// Error omits the handler's free text because it may contain secrets.
type StageError struct {
	Stage Stage
	Err   error
}

func (err *StageError) Error() string {
	return fmt.Sprintf("runner: stage %d (%s) failed", err.Stage.Number, err.Stage.ID)
}

func (err *StageError) Unwrap() error { return err.Err }

// Execute accepts the same claim and context as Runner.Run's callback. The
// callback must handle the returned error and finish result reporting before
// returning to Run, which then releases the concurrency slot.
// A controlled executor reports progress, success, timeout and cancellation;
// other stage errors retain their classification for caller-owned reporting.
func (executor *Executor) Execute(ctx context.Context, claim Claim) error {
	if executor == nil || len(executor.stages) == 0 {
		return fmt.Errorf("runner: executor must be constructed with stage handlers")
	}
	start := StageValidateProject
	if claim.Execution.ResumePoint != nil {
		switch *claim.Execution.ResumePoint {
		case "gate2":
			start = StageGate2
		case "gate3":
			start = StageGate3
		case "publish":
			start = StagePublish
		default:
			return fmt.Errorf("runner: invalid resume point")
		}
	}
	execution := &Execution{Claim: claim, control: executor.control, status: model.ExecutionClaimed}
	if executor.control != nil {
		if err := executor.control.validateTimeouts(claim.Project.Timeouts); err != nil {
			return err
		}
	}
	started := false
	for _, stage := range executor.stages {
		started = started || stage.ID == start
		if !started {
			continue
		}
		if err := ctx.Err(); err != nil {
			return execution.stageError(stage.Stage, err)
		}
		if executor.control != nil && stage.ID == StagePublish {
			checkErr := executor.control.CheckCancellation(ctx)
			if err := ctx.Err(); err != nil {
				return execution.stageError(stage.Stage, err)
			}
			if checkErr != nil {
				return &StageError{stage.Stage, checkErr}
			}
		}
		if executor.control != nil && stage.ID != StageComplete {
			if err := execution.report(stage.Status, ""); err != nil {
				return &StageError{stage.Stage, err}
			}
		}
		stageCtx, cancel := execution.stageContext(ctx, stage.ID)
		err := stageCtx.Err()
		if err == nil {
			err = stage.run(stageCtx, execution)
		}
		if executor.control != nil && stageCtx.Err() != nil {
			err = errors.Join(stageCtx.Err(), err)
		}
		cancel()
		if err != nil {
			return execution.stageError(stage.Stage, err)
		}
		if executor.control != nil && stage.ID == StageComplete {
			if err := execution.report(stage.Status, ""); err != nil {
				return &StageError{stage.Stage, err}
			}
		}
	}
	return nil
}
