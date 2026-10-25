package task_test

import (
	"errors"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

var taskStates = []model.TaskStatus{
	"received", "planning", "queued", "running", "verifying", "waiting_approval",
	"completed", "failed", "cancelled", "timed_out", "blocked", "", "unknown",
}

var executionStates = []model.ExecutionStatus{
	"pending", "claimed", "preparing", "running", "verifying", "publishing",
	"succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out", "", "unknown",
}

func TestTaskTransitionsMatchSpec04Section32(t *testing.T) {
	// Every cell, including reserved Phase 2 states and self-transitions, is
	// checked against the specification, independently of the production table.
	allowed := map[model.TaskStatus][]model.TaskStatus{
		"received":  {"queued"},
		"queued":    {"running", "cancelled"},
		"running":   {"verifying", "failed", "timed_out", "cancelled", "blocked"},
		"verifying": {"failed", "timed_out", "cancelled", "blocked", "completed"},
		"blocked":   {"queued", "cancelled"},
		"failed":    {"queued"},
		"timed_out": {"queued"},
		"cancelled": {"queued"},
		"completed": {"queued"},
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
		{"running", "running"}, {"verifying", "verifying"}, {"publishing", "verifying"},
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

func TestExecutionTransitionTable(t *testing.T) {
	allowed := map[model.ExecutionStatus][]model.ExecutionStatus{
		"pending":    {"claimed", "cancelled"},
		"claimed":    {"preparing", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"preparing":  {"running", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"running":    {"verifying", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"verifying":  {"publishing", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"publishing": {"succeeded", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"blocked":    {"pending", "cancelled"},
	}
	for _, resume := range []string{"", "gate2", "gate3", "publish"} {
		for _, from := range executionStates {
			for _, to := range executionStates {
				t.Run(resume+"/"+string(from)+"->"+string(to), func(t *testing.T) {
					want := contains(allowed[from], to)
					if from == "claimed" || from == "preparing" {
						if resume == "gate2" || resume == "gate3" {
							want = want || to == "verifying"
						}
						if resume == "publish" {
							want = want || to == "publishing"
						}
					}
					err := task.ValidateExecutionTransition(from, to, resume)
					if want {
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
