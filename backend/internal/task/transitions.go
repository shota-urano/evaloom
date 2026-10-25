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

// taskTransitions is exactly the Phase 1 table in spec 04, section 3.2.
// A self-transition is not a task transition; Execution substeps are handled below.
var taskTransitions = map[model.TaskStatus][]model.TaskStatus{
	model.TaskReceived:  {model.TaskQueued},
	model.TaskQueued:    {model.TaskRunning, model.TaskCancelled},
	model.TaskRunning:   {model.TaskVerifying, model.TaskFailed, model.TaskTimedOut, model.TaskCancelled, model.TaskBlocked},
	model.TaskVerifying: {model.TaskCompleted, model.TaskFailed, model.TaskTimedOut, model.TaskCancelled, model.TaskBlocked},
	model.TaskBlocked:   {model.TaskQueued, model.TaskCancelled},
	model.TaskFailed:    {model.TaskQueued},
	model.TaskTimedOut:  {model.TaskQueued},
	model.TaskCancelled: {model.TaskQueued},
	model.TaskCompleted: {model.TaskQueued},
}

func ValidateTaskTransition(from, to model.TaskStatus) error {
	for _, allowed := range taskTransitions[from] {
		if to == allowed {
			return nil
		}
	}
	return conflict("task transition %q -> %q is not allowed", from, to)
}

// executionTasks is spec 02, section 5.2. Unknown values fail closed.
var executionTasks = map[model.ExecutionStatus]model.TaskStatus{
	model.ExecutionPending:     model.TaskQueued,
	model.ExecutionClaimed:     model.TaskRunning,
	model.ExecutionPreparing:   model.TaskRunning,
	model.ExecutionRunning:     model.TaskRunning,
	model.ExecutionVerifying:   model.TaskVerifying,
	model.ExecutionPublishing:  model.TaskVerifying,
	model.ExecutionSucceeded:   model.TaskCompleted,
	model.ExecutionBlocked:     model.TaskBlocked,
	model.ExecutionFailed:      model.TaskFailed,
	model.ExecutionInterrupted: model.TaskFailed,
	model.ExecutionCancelled:   model.TaskCancelled,
	model.ExecutionTimedOut:    model.TaskTimedOut,
}

func TaskStatusForExecution(status model.ExecutionStatus) (model.TaskStatus, error) {
	if taskStatus, ok := executionTasks[status]; ok {
		return taskStatus, nil
	}
	return "", conflict("unknown execution status %q", status)
}

// executionTransitions follows the steps in spec 05, section 3.3 and the failure,
// interruption, cancellation and resume paths in spec 04. Retries create another
// Execution; they must never reopen a terminal Execution.
var executionTransitions = map[model.ExecutionStatus][]model.ExecutionStatus{
	model.ExecutionPending:    {model.ExecutionClaimed, model.ExecutionCancelled},
	model.ExecutionClaimed:    {model.ExecutionPreparing, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionPreparing:  {model.ExecutionRunning, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionRunning:    {model.ExecutionVerifying, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionVerifying:  {model.ExecutionPublishing, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionPublishing: {model.ExecutionSucceeded, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut, model.ExecutionCancelled, model.ExecutionBlocked},
	model.ExecutionBlocked:    {model.ExecutionPending, model.ExecutionCancelled},
}

// ValidateExecutionTransition also accepts starting directly at an approved
// resume point after claim (spec 04, 3.8; spec 05, 3.3). The resume API must check
// the block decision and SHA before setting resume_point; this is not authorization.
func ValidateExecutionTransition(from, to model.ExecutionStatus, resumePoint string) error {
	if from == model.ExecutionClaimed || from == model.ExecutionPreparing {
		if (resumePoint == "gate2" || resumePoint == "gate3") && to == model.ExecutionVerifying ||
			resumePoint == "publish" && to == model.ExecutionPublishing {
			return nil
		}
	}
	for _, allowed := range executionTransitions[from] {
		if to == allowed {
			return nil
		}
	}
	return conflict("execution transition %q -> %q is not allowed", from, to)
}
