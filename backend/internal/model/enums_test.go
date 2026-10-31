package model_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestSpecEnumValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decode func([]byte) error
		values []string
	}{
		{"adapter", decode[model.AdapterName], []string{"codex", "claude_code"}},
		{"task", decode[model.TaskStatus], []string{
			"received", "planning", "queued", "running", "verifying", "waiting_approval",
			"completed", "failed", "cancelled", "timed_out", "blocked",
		}},
		{"execution", decode[model.ExecutionStatus], []string{
			"pending", "claimed", "preparing", "running", "verifying", "publishing",
			"succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out",
		}},
		{"event", decode[model.ExecutionEventType], []string{
			"status_changed", "log", "worktree_ready", "context_pinned", "harness_started",
			"harness_finished", "gate_result", "blocked", "pushed", "pr_created", "notification_sent",
			"notification_failed",
		}},
		{"error", decode[model.ErrorClass], []string{
			"config_error", "execution_failed", "test_failed", "timeout", "cancelled",
			"interrupted", "publish_failed", "sandbox_error",
		}},
		{"block", decode[model.BlockCause], []string{
			"gate2_missing_tests", "gate2_unverifiable", "gate3_mismatch", "context_conflict",
			"git_state",
		}},
		{"result", decode[model.ResultStatus], []string{"succeeded", "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, value := range tc.values {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := tc.decode(data); err != nil {
					t.Errorf("spec value %s rejected: %v", data, err)
				}
			}
			for _, input := range []string{`"unknown"`, `""`, `"SUCCEEDED"`, "null", "0", "true", "[]", "{}"} {
				if err := tc.decode([]byte(input)); err == nil {
					t.Errorf("invalid enum accepted: %s", input)
				}
			}
		})
	}
}

func TestEnumsRejectInvalidGoValues(t *testing.T) {
	for _, value := range []interface{ Validate() error }{
		model.AdapterName("unknown"), model.TaskStatus("unknown"),
		model.ExecutionStatus("unknown"), model.ExecutionEventType("unknown"),
		model.ErrorClass("unknown"), model.BlockCause("unknown"), model.ResultStatus("unknown"),
	} {
		if err := value.Validate(); err == nil {
			t.Errorf("invalid Go enum accepted: %v", value)
		}
	}
}

func decode[T ~string](data []byte) error {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, data) {
		return fmt.Errorf("round trip changed enum: %s to %s", data, encoded)
	}
	return nil
}
