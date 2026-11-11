package task_test

import (
	"errors"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

var taskStates = []model.TaskStatus{
	"received", "routing", "queued", "running", "planning", "verifying", "waiting_approval",
	"completed", "failed", "cancelled", "timed_out", "blocked", "", "unknown",
}

var executionStates = []model.ExecutionStatus{
	"pending", "claimed", "preparing", "working", "checking", "publishing",
	"waiting_approval", "merging", "succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out", "", "unknown",
}

func TestTaskTransitionsMatchSpec04Section32(t *testing.T) {
	// Every cell, including obsolete states and self-transitions, is checked
	// against the specification, independently of the production table.
	allowed := map[model.TaskStatus][]model.TaskStatus{
		// 3.1/15 queued, 15/3.7 routing, intake hold, cancel (3.8).
		"received": {"queued", "routing", "blocked", "cancelled"},
		// 3.7 approve or label, reject, cancel.
		"routing": {"queued", "cancelled"},
		// claim, cancel; claim-time config_error / workspace_missing (04 3.3)
		// and approval expiry (17 Q166, 3.6).
		"queued": {"running", "cancelled", "failed", "blocked", "waiting_approval"},
		// 02 5.2 mapping of stage reports, including publishing -> pending
		// (every approval waived) which maps to queued.
		"running": {"waiting_approval", "blocked", "completed", "failed", "timed_out", "cancelled", "queued"},
		// resume, end by decision, cancel.
		"waiting_approval": {"queued", "failed", "cancelled"},
		// Intake hold release (queued / routing); decision resume, end, cancel;
		// 02 5.2 blocked -> waiting_approval (check_blocked continue).
		"blocked": {"queued", "routing", "waiting_approval", "failed", "cancelled"},
		// Retry (Q131).
		"failed": {"queued"}, "timed_out": {"queued"}, "cancelled": {"queued"}, "completed": {"queued"},
	}
	for _, from := range taskStates {
		for _, to := range taskStates {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				err := task.ValidateTaskTransition(from, to)
				if contains(allowed[from], to) {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					assertConflict(t, err)
				}
			})
		}
	}
}

func TestExecutionTaskMappingMatchesSpec02Section52(t *testing.T) {
	for _, tc := range []struct {
		execution model.ExecutionStatus
		task      model.TaskStatus
	}{
		{"pending", "queued"}, {"claimed", "running"}, {"preparing", "running"},
		{"working", "running"}, {"checking", "running"}, {"publishing", "running"},
		{"waiting_approval", "waiting_approval"},
		{"succeeded", "completed"}, {"blocked", "blocked"}, {"failed", "failed"},
		{"interrupted", "failed"}, {"cancelled", "cancelled"}, {"timed_out", "timed_out"},
	} {
		t.Run(string(tc.execution), func(t *testing.T) {
			got, err := task.TaskStatusForExecution(tc.execution)
			if err != nil || got != tc.task {
				t.Fatalf("mapped task = %q, err = %v; want %q", got, err, tc.task)
			}
		})
	}
	for _, invalid := range []model.ExecutionStatus{"", "unknown", "completed", "Running"} {
		_, err := task.TaskStatusForExecution(invalid)
		assertConflict(t, err)
	}
}

func TestExecutionTransitionsMatchSpec02Section52(t *testing.T) {
	active := []model.ExecutionStatus{"failed", "timed_out", "cancelled", "interrupted"}
	allowed := map[model.ExecutionStatus][]model.ExecutionStatus{
		// claim; cancellation of a queued task (04 3.2, 3.8); claim-time
		// config_error / workspace_missing (04 3.3); approval expiry (17 3.6).
		"pending": {"claimed", "cancelled", "failed", "blocked", "waiting_approval"},
		// prepare, work, or the resume_stage.
		"claimed":   append([]model.ExecutionStatus{"preparing", "working", "publishing"}, active...),
		"preparing": append([]model.ExecutionStatus{"working"}, active...),
		// check, or a question.
		"working": append([]model.ExecutionStatus{"checking", "blocked"}, active...),
		// publish, approval wait, check_blocked.
		"checking": append([]model.ExecutionStatus{"publishing", "waiting_approval", "blocked"}, active...),
		// approval wait, every approval waived (resume publish), unknown, succeeded.
		"publishing": append([]model.ExecutionStatus{"waiting_approval", "pending", "blocked", "succeeded"}, active...),
		// all approved, one rejected, abort.
		"waiting_approval": {"pending", "failed", "cancelled"},
		// continue with approvals, continue/answer, abort, publish_unknown abort.
		"blocked": {"waiting_approval", "pending", "cancelled", "failed"},
	}
	for _, from := range executionStates {
		for _, to := range executionStates {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				err := task.ValidateExecutionTransition(from, to)
				if contains(allowed[from], to) {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					assertConflict(t, err)
				}
			})
		}
	}
}

func TestResumeStagesMatchSpec02(t *testing.T) {
	allowed := map[model.ExecutionStatus][]model.Stage{
		"publishing": {"publish"}, "waiting_approval": {"publish"}, "blocked": {"work", "publish"},
	}
	for _, from := range executionStates {
		for _, stage := range []model.Stage{"", "prepare", "work", "check", "publish", "unknown"} {
			t.Run(string(from)+"/"+string(stage), func(t *testing.T) {
				err := task.ValidateResume(from, stage)
				if contains(allowed[from], stage) {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					assertConflict(t, err)
				}
			})
		}
	}
	starts := map[model.Stage][]model.ExecutionStatus{
		"": {"preparing", "working"}, "work": {"working"}, "publish": {"publishing"},
	}
	for _, resume := range []model.Stage{"", "work", "publish", "prepare", "check"} {
		for _, to := range executionStates {
			t.Run("start/"+string(resume)+"->"+string(to), func(t *testing.T) {
				err := task.ValidateStageStart(resume, to)
				if contains(starts[resume], to) {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					assertConflict(t, err)
				}
			})
		}
	}
}

func assertConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *task.ConflictError
	if !errors.Is(err, task.ErrConflict) || !errors.As(err, &conflict) {
		t.Fatalf("error = %v; want ConflictError wrapping ErrConflict", err)
	}
	if conflict.StatusCode() != 409 || conflict.Code != "conflict" {
		t.Fatalf("conflict response = %#v, status = %d", conflict, conflict.StatusCode())
	}
}

func contains[T comparable](values []T, value T) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
