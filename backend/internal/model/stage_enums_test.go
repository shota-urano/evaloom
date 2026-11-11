package model_test

import (
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestDataModelAdditionalEnums(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decode func([]byte) error
		values []string
	}{
		{"stage", decode[model.Stage], []string{"prepare", "work", "check", "publish"}},
		{"stage status", decode[model.StageStatus], []string{"running", "succeeded", "failed", "blocked", "needs_approval", "unknown", "question", "cancelled", "timed_out"}},
		{"execution mode", decode[model.ExecutionMode], []string{"continue", "fresh"}},
		{"task source", decode[model.TaskSource], []string{"manual", "linear"}},
		{"route kind", decode[model.RouteKind], []string{"project", "harness"}},
		{"proposal status", decode[model.RouteProposalStatus], []string{"queued", "running", "proposed", "failed", "decided", "superseded", "cancelled"}},
		{"decision kind", decode[model.DecisionKind], []string{"route", "approval", "blocked", "question"}},
		{"decision cause", decode[model.DecisionCause], []string{"check_blocked", "publish_unknown", "context_conflict", "workspace_missing", "question"}},
		{"intake cause", decode[model.IntakeBlockCause], []string{"missing_acceptance_criteria", "config_missing_profile"}},
		{"outbox action", decode[model.LinearOutboxAction], []string{"comment", "set_state", "add_label"}},
		{"outbox kind", decode[model.LinearOutboxKind], []string{"missing_acceptance_criteria", "decision_open", "succeeded", "failed", "cancelled", "started", "waiting_decision", "completed", "route_approved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range tc.values {
				if err := tc.decode([]byte(`"` + v + `"`)); err != nil {
					t.Error(err)
				}
			}
			for _, v := range []string{`null`, `0`, `true`, `[]`, `{}`, `""`, `"unknown_value"`, `"UPPER"`} {
				if err := tc.decode([]byte(v)); err == nil {
					t.Errorf("invalid value accepted: %s", v)
				}
			}
		})
	}
}

func TestOutboxKindActionPairs(t *testing.T) {
	for _, action := range []model.LinearOutboxAction{model.OutboxComment, model.OutboxSetState, model.OutboxAddLabel, "unknown"} {
		for _, kind := range []model.LinearOutboxKind{model.OutboxMissingAcceptanceCriteria, model.OutboxDecisionOpen, model.OutboxSucceeded, model.OutboxFailed, model.OutboxCancelled, model.OutboxStarted, model.OutboxWaitingDecision, model.OutboxCompleted, model.OutboxRouteApproved, "unknown"} {
			allowed := action == model.OutboxComment && (kind == model.OutboxMissingAcceptanceCriteria || kind == model.OutboxDecisionOpen || kind == model.OutboxSucceeded || kind == model.OutboxFailed || kind == model.OutboxCancelled) ||
				action == model.OutboxSetState && (kind == model.OutboxStarted || kind == model.OutboxWaitingDecision || kind == model.OutboxCompleted || kind == model.OutboxCancelled) ||
				action == model.OutboxAddLabel && kind == model.OutboxRouteApproved
			if err := kind.ValidateForAction(action); (err == nil) != allowed {
				t.Errorf("%s/%s: allowed=%v, error=%v", action, kind, allowed, err)
			}
		}
	}
}

func TestRetiredDataModelValuesRejected(t *testing.T) {
	for _, v := range []interface{ Validate() error }{
		model.TaskStatus("planning"), model.TaskStatus("verifying"), model.ExecutionStatus("merging"),
		model.ExecutionEventType("worktree_ready"), model.ExecutionEventType("gate_result"), model.ExecutionEventType("pushed"),
		model.ExecutionEventType("pr_created"), model.ExecutionEventType("harness_started"), model.ExecutionEventType("harness_finished"), model.ExecutionEventType("blocked"),
		model.ErrorClass("execution_failed"), model.ErrorClass("test_failed"),
		model.IntakeBlockCause("routing_multiple_labels"), model.IntakeBlockCause("routing_unknown_label"),
		model.IntakeBlockCause("routing_team_mismatch"), model.IntakeBlockCause("routing_no_label"), model.IntakeBlockCause("config_missing_assignment"),
	} {
		if err := v.Validate(); err == nil {
			t.Errorf("retired value accepted: %v", v)
		}
	}
}
