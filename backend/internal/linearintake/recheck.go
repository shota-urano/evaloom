package linearintake

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

var _ HeldRechecker = (*TaskStarter)(nil)

// RecheckHeldTask redoes the 3.6 decision for the Issue's held task (spec 15
// 3.6.3) with the same Route as task creation, so creation and recheck never
// disagree. When it passes, the same task gets its Project and acceptance
// criteria, its first Execution and the queued status; no task is created.
// When the cause is unchanged nothing is written (no second comment for the
// same cause, spec 16 3.2). A different cause is written to the task and
// stays held. An Issue without a held task is left alone.
func (s *TaskStarter) RecheckHeldTask(ctx context.Context, tx *sql.Tx, issue linear.Issue) (Decision, error) {
	held, err := store.ListHeldLinearTasks(ctx, tx, issue.ID)
	if err != nil || len(held) == 0 {
		return "", err
	}
	// At most one task of an Issue is non-terminal (spec 15 3.5 rule 3).
	h := held[len(held)-1]
	r, err := s.Route(ctx, tx, issue)
	if err != nil {
		return "", err
	}
	comparisonCause := string(r.Cause)
	if strings.HasPrefix(comparisonCause, "routing_") {
		comparisonCause = "routing"
	}
	if comparisonCause == h.Cause {
		return "", nil
	}
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339)
	detail := map[string]any{"source": "linear", "linearIssueId": issue.ID,
		"issueIdentifier": issue.Identifier, "fromIntakeBlockCause": h.Cause}
	if r.ProjectID != "" {
		detail["projectId"] = r.ProjectID
	}
	if err := store.UpdateHeldLinearTask(ctx, tx, h.TaskID, r.ProjectID, r.Criteria, string(r.Cause), stamp); err != nil {
		return "", err
	}
	decision := DecisionHeld
	if r.Cause != "" {
		detail["intakeBlockCause"] = string(r.Cause)
		if err := enqueueHold(ctx, tx, h.TaskID, r.Cause, s.routingLabelPrefix(ctx)); err != nil {
			return "", err
		}
	} else {
		decision = DecisionStarted
		executionID, err := newID(now)
		if err != nil {
			return "", err
		}
		detail["executionId"], detail["from"], detail["to"] = executionID, model.TaskBlocked, model.TaskQueued
		if err := store.InsertInitialExecution(ctx, tx, executionID, h.TaskID, stamp); err != nil {
			return "", fmt.Errorf("linearintake: insert execution: %w", err)
		}
		if err := task.QueueHeldTaskTx(ctx, tx, h.TaskID, executionID); err != nil {
			return "", err
		}
		if _, err := store.AppendExecutionEvent(ctx, tx, executionID, model.EventStatusChanged,
			json.RawMessage(`{"from":"blocked","to":"queued","reason":"intake_recheck"}`)); err != nil {
			return "", err
		}
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{Actor: "system", Action: "task.intake_recheck",
		TargetType: "task", TargetID: h.TaskID, Detail: detail}); err != nil {
		return "", err
	}
	return decision, nil
}
