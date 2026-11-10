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
			"received", "routing", "queued", "running", "waiting_approval",
			"completed", "failed", "cancelled", "timed_out", "blocked",
		}},
		{"execution", decode[model.ExecutionStatus], []string{
			"pending", "claimed", "preparing", "working", "checking", "publishing",
			"waiting_approval", "succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out",
		}},
		{"event", decode[model.ExecutionEventType], []string{
			"status_changed", "log", "context_pinned", "stage_started", "stage_finished", "check_result", "approval_requested", "approval_waived", "approval_consumed", "decision_opened", "decision_answered", "published", "notification_sent",
			"notification_failed",
		}},
		{"error", decode[model.ErrorClass], []string{
			"config_error", "work_failed", "check_failed", "timeout", "cancelled",
			"interrupted", "publish_failed", "sandbox_error",
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
		model.ErrorClass("unknown"), model.ResultStatus("unknown"),
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
