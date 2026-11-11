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

// taskTransitions is the Phase 1 table in spec 04, section 3.2, plus the
// approval paths of spec 17 (3.3 PR created -> waiting_approval; 3.5.2 merged,
// 3.6 rejected (Q57), closed or merged on GitHub (Q58) and cancelled).
// A self-transition is not a task transition; Execution substeps are handled below.
var taskTransitions = map[model.TaskStatus][]model.TaskStatus{
	model.TaskReceived:        {model.TaskQueued},
	model.TaskRouting:         {model.TaskQueued, model.TaskCancelled},
	model.TaskQueued:          {model.TaskRunning, model.TaskWaitingApproval, model.TaskCancelled},
	model.TaskRunning:         {model.TaskCompleted, model.TaskFailed, model.TaskTimedOut, model.TaskCancelled, model.TaskBlocked, model.TaskWaitingApproval},
	model.TaskWaitingApproval: {model.TaskQueued, model.TaskCompleted, model.TaskFailed, model.TaskCancelled},
	model.TaskBlocked:         {model.TaskQueued, model.TaskCancelled},
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

// executionTasks is spec 02, section 5.2 and spec 17, section 3.9.
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

// executionTransitions follows the steps in spec 05, section 3.3 and the failure,
// interruption, cancellation and resume paths in spec 04, and the approval
// paths in spec 17: a created PR waits for approval (3.3); approval starts the
// merge (3.5.1); the merge succeeds or fails (3.5.2, 3.5.3); rejection (Q57),
// a PR merged or closed on GitHub (Q58) and cancellation end the wait (3.6); a
// consume that is refused for expiry or a changed head/target returns to the
// wait so the owner can approve again (3.5.2, 3.6). Retries create another
// Execution; they must never reopen a terminal Execution.
var executionTransitions = map[model.ExecutionStatus][]model.ExecutionStatus{
	model.ExecutionPending:         {model.ExecutionClaimed, model.ExecutionWaitingApproval, model.ExecutionCancelled},
	model.ExecutionClaimed:         {model.ExecutionPreparing, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionPreparing:       {model.ExecutionRunning, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionRunning:         {model.ExecutionVerifying, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionVerifying:       {model.ExecutionPublishing, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionPublishing:      {model.ExecutionSucceeded, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked, model.ExecutionWaitingApproval},
	model.ExecutionWaitingApproval: {model.ExecutionPending, model.ExecutionSucceeded, model.ExecutionFailed, model.ExecutionCancelled},
	model.ExecutionBlocked:         {model.ExecutionPending, model.ExecutionCancelled},
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
