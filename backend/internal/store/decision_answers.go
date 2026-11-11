package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ApplyBlockedOrQuestionAnswerTx performs the lifecycle change of spec 04 3.6
// for kind blocked and question. The caller has checked the decision is open,
// the action is allowed for its kind and cause, and the basis matches; it
// owns the transaction and writes the answer and its audit record.
func ApplyBlockedOrQuestionAnswerTx(ctx context.Context, tx *sql.Tx, d OwnerDecision, action string) error {
	if d.ExecutionID == nil || d.Cause == nil {
		return fmt.Errorf("store: decision %s has no Execution or cause", d.ID)
	}
	e, err := readReportExecution(ctx, tx, *d.ExecutionID)
	if err != nil {
		return err
	}
	if e.taskID != d.TaskID {
		return fmt.Errorf("store: decision %s does not belong to its Execution", d.ID)
	}
	if !e.latest || e.status != model.ExecutionBlocked {
		return conflictf("Decision requires the latest Execution to be blocked")
	}
	if err := e.checkTask(); err != nil {
		return err
	}
	cause := model.DecisionCause(*d.Cause)
	switch {
	case d.Kind == string(model.DecisionQuestion) && action == "answer":
		return e.move(ctx, tx, model.ExecutionPending, model.StageWork, "question_answered")
	case d.Kind == string(model.DecisionBlocked) && cause == model.CausePublishUnknown && action == "continue":
		return e.move(ctx, tx, model.ExecutionPending, model.StagePublish, "owner_continue")
	case d.Kind == string(model.DecisionBlocked) && cause == model.CausePublishUnknown && action == "abort":
		return failExecution(ctx, tx, e, model.ErrorPublishUnknown, nil)
	case d.Kind == string(model.DecisionBlocked) && cause == model.CauseCheckBlocked && action == "continue":
		return continueCheckBlocked(ctx, tx, e, d)
	case action == "abort":
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ? WHERE id = ?`, model.ErrorCancelled, e.id); err != nil {
			return err
		}
		return e.move(ctx, tx, model.ExecutionCancelled, "", "owner_abort")
	}
	return fmt.Errorf("store: no lifecycle change for decision kind %q cause %q action %q", d.Kind, *d.Cause, action)
}

// continueCheckBlocked applies the check result's approval requests the same
// way a check needs_approval does (spec 17 3.3): waivable operations are
// waived, the rest wait for approval. With none left, publish starts.
func continueCheckBlocked(ctx context.Context, tx *sql.Tx, e *reportExecution, d OwnerDecision) error {
	var detail struct {
		ApprovalRequests []contract.ApprovalRequest `json:"approvalRequests"`
	}
	if err := json.Unmarshal(d.Detail, &detail); err != nil {
		return fmt.Errorf("store: decision %s detail cannot be read", d.ID)
	}
	profile, err := readReportProfile(ctx, tx, e)
	if err != nil {
		return err
	}
	waivers, err := routeWaivers(ctx, tx, e)
	if err != nil {
		return err
	}
	var requested []contract.ApprovalRequest
	for _, a := range detail.ApprovalRequests {
		waivable := false
		for _, declared := range profile.Approvals {
			waivable = waivable || (declared.Operation == a.Operation && declared.Waivable)
		}
		if waivable && slices.Contains(waivers, a.Operation) {
			if err := appendEvent(ctx, tx, e.id, model.EventApprovalWaived, map[string]any{"operation": a.Operation, "contentId": a.ContentID}); err != nil {
				return err
			}
			continue
		}
		requested = append(requested, a)
	}
	if len(requested) == 0 {
		return e.move(ctx, tx, model.ExecutionPending, model.StagePublish, "owner_continue")
	}
	for _, a := range requested {
		if err := requestApproval(ctx, tx, e, model.StageCheck, a); err != nil {
			return err
		}
	}
	return e.move(ctx, tx, model.ExecutionWaitingApproval, "", "approval_required")
}

// RejectApprovalSiblingsTx implements the reject row of spec 17 3.6: the other
// unused approvals of the Execution become invalidated (cancelled) and their
// open decisions withdrawn (cancelled), with audit records.
func RejectApprovalSiblingsTx(ctx context.Context, tx *sql.Tx, executionID string) error {
	return invalidateOpenApprovalsTx(ctx, tx, executionID, "cancelled")
}

// PendingApprovalCount counts the approvals of an Execution still waiting for
// the owner (spec 04 3.6: the Execution resumes once none is pending).
func PendingApprovalCount(ctx context.Context, tx *sql.Tx, executionID string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM approvals WHERE execution_id = ? AND status = 'pending'`, executionID).Scan(&n)
	return n, err
}
