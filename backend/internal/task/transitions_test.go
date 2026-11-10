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
	// Every cell, including reserved states and self-transitions, is checked
	// against the specification, independently of the production table.
	// waiting_approval follows spec 17 3.3, 3.5.2 and 3.6 (Q57, Q58).
	allowed := map[model.TaskStatus][]model.TaskStatus{
		"received":         {"queued"},
		"queued":           {"running", "waiting_approval", "cancelled"},
		"running":          {"failed", "timed_out", "cancelled", "blocked", "completed", "waiting_approval"},
		"routing":          {"queued", "cancelled"},
		"waiting_approval": {"queued", "completed", "failed", "cancelled"},
		"blocked":          {"queued", "cancelled"},
		"failed":           {"queued"},
		"timed_out":        {"queued"},
		"cancelled":        {"queued"},
		"completed":        {"queued"},
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

func TestExecutionTransitionTable(t *testing.T) {
	allowed := map[model.ExecutionStatus][]model.ExecutionStatus{
		"pending":    {"claimed", "waiting_approval", "cancelled"},
		"claimed":    {"preparing", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"preparing":  {"working", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"working":    {"checking", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"checking":   {"publishing", "failed", "interrupted", "timed_out", "cancelled", "blocked"},
		"publishing": {"succeeded", "failed", "interrupted", "timed_out", "cancelled", "blocked", "waiting_approval"},
		"blocked":    {"pending", "cancelled"},
		// Spec 17: approval starts the merge (3.5.1); merged, rejected (Q57),
		// merged or closed on GitHub (Q58) and cancelled end the wait (3.6).
		"waiting_approval": {"pending", "succeeded", "failed", "cancelled"},
		// Merged or failed (3.5.2, 3.5.3), cancelled, or back to the wait when
		// consume is refused (expired, head or target changed).
	}
	for _, resume := range []string{""} {
		for _, from := range executionStates {
			for _, to := range executionStates {
				t.Run(resume+"/"+string(from)+"->"+string(to), func(t *testing.T) {
					want := contains(allowed[from], to)
					err := task.ValidateExecutionTransition(from, to)
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
