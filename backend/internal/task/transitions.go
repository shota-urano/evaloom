// Package task owns the Phase 1 Task and Execution lifecycle (spec 04).
package task

import (
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ErrConflict identifies invalid transitions and stale state expectations.
var ErrConflict = errors.New("conflict")

// ConflictError supplies the code and HTTP status required by spec 04, section 6.
type ConflictError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ConflictError) Error() string { return e.Message }
func (*ConflictError) Unwrap() error   { return ErrConflict }
func (*ConflictError) StatusCode() int { return 409 }

func conflict(format string, args ...any) error {
	return &ConflictError{Code: "conflict", Message: fmt.Sprintf(format, args...)}
}

// taskTransitions is the table in spec 04, section 3.2. Rows whose task state
// follows the latest Execution also include the task changes implied by the
// Execution table of spec 02, section 5.2 (via the mapping below):
// running -> queued (publish with every approval waived) and blocked ->
// waiting_approval (owner continues a check_blocked with approval requests).
// 02 5.2 lists the "main" transitions; the confirmed text adds, for a queued
// task: claim-time failed (config_error) and blocked (workspace_missing,
// spec 04 3.3 steps 4-5), and waiting_approval (claim-time expiry Q166 and
// renew, spec 17 3.6).
// blocked covers both the intake hold (no Execution: -> queued / routing /
// cancelled) and the owner decision (-> queued / failed / cancelled).
// A self-transition is not a task transition.
var taskTransitions = map[model.TaskStatus][]model.TaskStatus{
	model.TaskReceived:        {model.TaskQueued, model.TaskRouting, model.TaskBlocked, model.TaskCancelled},
	model.TaskRouting:         {model.TaskQueued, model.TaskCancelled},
	model.TaskQueued:          {model.TaskRunning, model.TaskCancelled, model.TaskFailed, model.TaskBlocked, model.TaskWaitingApproval},
	model.TaskRunning:         {model.TaskWaitingApproval, model.TaskBlocked, model.TaskCompleted, model.TaskFailed, model.TaskTimedOut, model.TaskCancelled, model.TaskQueued},
	model.TaskWaitingApproval: {model.TaskQueued, model.TaskFailed, model.TaskCancelled},
	model.TaskBlocked:         {model.TaskQueued, model.TaskRouting, model.TaskWaitingApproval, model.TaskFailed, model.TaskCancelled},
	model.TaskFailed:          {model.TaskQueued},
	model.TaskTimedOut:        {model.TaskQueued},
	model.TaskCancelled:       {model.TaskQueued},
	model.TaskCompleted:       {model.TaskQueued},
}

func ValidateTaskTransition(from, to model.TaskStatus) error {
	for _, allowed := range taskTransitions[from] {
		if to == allowed {
			return nil
		}
	}
	return conflict("task transition %q -> %q is not allowed", from, to)
}

// executionTasks is the task state column of spec 02, section 5.2.
// Unknown values fail closed.
var executionTasks = map[model.ExecutionStatus]model.TaskStatus{
	model.ExecutionPending:         model.TaskQueued,
	model.ExecutionClaimed:         model.TaskRunning,
	model.ExecutionPreparing:       model.TaskRunning,
	model.ExecutionRunning:         model.TaskRunning,
	model.ExecutionVerifying:       model.TaskRunning,
	model.ExecutionPublishing:      model.TaskRunning,
	model.ExecutionWaitingApproval: model.TaskWaitingApproval,
	model.ExecutionSucceeded:       model.TaskCompleted,
	model.ExecutionBlocked:         model.TaskBlocked,
	model.ExecutionFailed:          model.TaskFailed,
	model.ExecutionInterrupted:     model.TaskFailed,
	model.ExecutionCancelled:       model.TaskCancelled,
	model.ExecutionTimedOut:        model.TaskTimedOut,
}

func TaskStatusForExecution(status model.ExecutionStatus) (model.TaskStatus, error) {
	if taskStatus, ok := executionTasks[status]; ok {
		return taskStatus, nil
	}
	return "", conflict("unknown execution status %q", status)
}

// executionTransitions is the table in spec 02, section 5.2. "Active" states
// (claimed / preparing / working / checking / publishing) may also end as
// failed, timed_out, cancelled or interrupted. pending -> cancelled is the
// cancellation of a queued task (spec 04, 3.2 and 3.8); pending -> failed
// and blocked are claim-time config_error and workspace_missing (spec 04,
// 3.3 steps 4-5); pending -> waiting_approval is an approval expired before
// publish (spec 17 Q166, 3.6 renew). Which stage a claimed
// Execution may start depends on resume_stage; TransitionExecutionTx checks it.
// Retries create another Execution; a terminal Execution never reopens.
var executionTransitions = map[model.ExecutionStatus][]model.ExecutionStatus{
	model.ExecutionPending:         {model.ExecutionClaimed, model.ExecutionCancelled, model.ExecutionFailed, model.ExecutionBlocked, model.ExecutionWaitingApproval},
	model.ExecutionClaimed:         activeExit(model.ExecutionPreparing, model.ExecutionRunning, model.ExecutionPublishing),
	model.ExecutionPreparing:       activeExit(model.ExecutionRunning),
	model.ExecutionRunning:         activeExit(model.ExecutionVerifying, model.ExecutionBlocked),
	model.ExecutionVerifying:       activeExit(model.ExecutionPublishing, model.ExecutionWaitingApproval, model.ExecutionBlocked),
	model.ExecutionPublishing:      activeExit(model.ExecutionWaitingApproval, model.ExecutionPending, model.ExecutionBlocked, model.ExecutionSucceeded),
	model.ExecutionWaitingApproval: {model.ExecutionPending, model.ExecutionFailed, model.ExecutionCancelled},
	model.ExecutionBlocked:         {model.ExecutionWaitingApproval, model.ExecutionPending, model.ExecutionCancelled, model.ExecutionFailed},
}

func activeExit(next ...model.ExecutionStatus) []model.ExecutionStatus {
	return append(next, model.ExecutionFailed, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionInterrupted)
}

// resumeStages lists, per source state, the resume_stage values that an
// Execution returning to pending may carry (spec 02, 5.2 and 5.6).
var resumeStages = map[model.ExecutionStatus][]model.Stage{
	model.ExecutionPublishing:      {model.StagePublish},
	model.ExecutionWaitingApproval: {model.StagePublish},
	model.ExecutionBlocked:         {model.StageWork, model.StagePublish},
}

// ValidateResume checks the resume_stage written when an Execution returns to
// pending. Only from -> pending transitions of spec 02, 5.2 accept a stage.
func ValidateResume(from model.ExecutionStatus, stage model.Stage) error {
	for _, allowed := range resumeStages[from] {
		if stage == allowed {
			return nil
		}
	}
	return conflict("execution %q cannot resume at stage %q", from, stage)
}

// ValidateStageStart checks which stage a claimed Execution may start: the
// resume_stage if it has one, otherwise preparing or working (spec 02, 5.2).
// Whether the Profile declares prepare is checked by the result handler.
func ValidateStageStart(resume model.Stage, to model.ExecutionStatus) error {
	var want []model.ExecutionStatus
	switch resume {
	case "":
		want = []model.ExecutionStatus{model.ExecutionPreparing, model.ExecutionRunning}
	case model.StageWork:
		want = []model.ExecutionStatus{model.ExecutionRunning}
	case model.StagePublish:
		want = []model.ExecutionStatus{model.ExecutionPublishing}
	}
	for _, allowed := range want {
		if to == allowed {
			return nil
		}
	}
	return conflict("claimed execution with resume stage %q cannot start %q", resume, to)
}

// ValidateExecutionTransition checks one Execution state change against the
// table above.
func ValidateExecutionTransition(from, to model.ExecutionStatus) error {
	for _, allowed := range executionTransitions[from] {
		if to == allowed {
			return nil
		}
	}
	return conflict("execution transition %q -> %q is not allowed", from, to)
}
