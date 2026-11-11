package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

var (
	ErrAuthorizeRunner = errors.New("Runner does not own Execution")
	ErrAuthorizeState  = errors.New("Execution is not publishing")
)

// AuthorizeExecutionTx checks and consumes an approval under BEGIN IMMEDIATE.
// The caller MUST commit successfully before exposing an allowed response.
func AuthorizeExecutionTx(ctx context.Context, tx *sql.Tx, executionID, runnerID string, input contract.AuthorizeRequest, now time.Time) (contract.AuthorizeResponse, error) {
	deny := contract.AuthorizeResponse{Reason: "not_approved"}
	if err := input.Validate(); err != nil {
		return deny, err
	}
	var owner, routeKind, projectID, harnessID, profileID, cancelled sql.NullString
	var status string
	err := tx.QueryRowContext(ctx, `SELECT runner_id,status,route_kind,project_id,harness_id,profile_id,cancel_requested_at
		FROM executions WHERE id=?`, executionID).Scan(&owner, &status, &routeKind, &projectID, &harnessID, &profileID, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return deny, ErrExecutionNotFound
	}
	if err != nil {
		return deny, err
	}
	// A different Runner must never mutate even a cancelled Execution.
	if runnerID == "" || !owner.Valid || owner.String != runnerID {
		return deny, ErrAuthorizeRunner
	}
	reply := deny
	if cancelled.Valid {
		reply.Reason = "cancelled"
		err = invalidateOpenApprovalsTx(ctx, tx, executionID, "cancelled")
	} else if status != "publishing" {
		return deny, ErrAuthorizeState
	} else if input.Operation == "publish" {
		reply = contract.AuthorizeResponse{Allowed: true}
	} else {
		var definition string
		err = tx.QueryRowContext(ctx, `SELECT definition FROM harness_profiles WHERE id=?`, profileID).Scan(&definition)
		if err == nil {
			// Registration validates the full definition. Read only approval
			// declarations here, as other stage configuration is unrelated.
			var profile struct {
				Approvals []contract.ApprovalDeclaration `json:"approvals"`
			}
			err = json.Unmarshal([]byte(definition), &profile)
			if err == nil {
				for _, declaration := range profile.Approvals {
					if declaration.Operation != input.Operation {
						continue
					}
					if input.ApprovalID != nil {
						reply, err = consumeApprovalTx(ctx, tx, executionID, input, now)
					} else if declaration.Waivable {
						var waiversJSON string
						switch routeKind.String {
						case "project":
							err = tx.QueryRowContext(ctx, `SELECT approval_waivers FROM projects WHERE id=?`, projectID).Scan(&waiversJSON)
						case "harness":
							err = tx.QueryRowContext(ctx, `SELECT approval_waivers FROM harnesses WHERE id=?`, harnessID).Scan(&waiversJSON)
						default:
							err = fmt.Errorf("store: missing pinned route")
						}
						var waivers []string
						if err == nil {
							err = json.Unmarshal([]byte(waiversJSON), &waivers)
						}
						if err == nil && slices.Contains(waivers, input.Operation) {
							reply = contract.AuthorizeResponse{Allowed: true}
						}
					}
					break
				}
			}
		}
	}
	if err != nil {
		return deny, err
	}
	detail := map[string]any{"approvalId": input.ApprovalID, "operation": input.Operation, "allowed": reply.Allowed}
	if reply.Reason != "" {
		detail["reason"] = reply.Reason
	}
	payload, err := json.Marshal(detail)
	if err == nil {
		_, err = AppendExecutionEvent(ctx, tx, executionID, model.EventApprovalConsumed, payload)
	}
	if err == nil {
		_, err = RecordAuditLog(ctx, tx, AuditLog{Actor: "runner:" + runnerID, Action: "approval.consume",
			TargetType: "execution", TargetID: executionID, Detail: detail})
	}
	if err != nil {
		return deny, err
	}
	return reply, nil
}

func consumeApprovalTx(ctx context.Context, tx *sql.Tx, executionID string, input contract.AuthorizeRequest, now time.Time) (contract.AuthorizeResponse, error) {
	deny := contract.AuthorizeResponse{Reason: "not_approved"}
	a, err := GetApprovalTx(ctx, tx, *input.ApprovalID)
	if errors.Is(err, sql.ErrNoRows) {
		return deny, nil
	}
	if err != nil {
		return deny, err
	}
	if a.ExecutionID != executionID || a.Operation != input.Operation {
		return deny, nil
	}
	if a.Status == "consumed" || a.UseCount >= a.MaxUses {
		return contract.AuthorizeResponse{Reason: "already_used"}, nil
	}
	if a.Status != "approved" {
		return deny, nil
	}
	if a.ExpiresAt == nil {
		return deny, fmt.Errorf("store: approved approval has no expiration")
	}
	expires, err := time.Parse(time.RFC3339Nano, *a.ExpiresAt)
	if err != nil {
		return deny, err
	}
	if !expires.After(now) {
		_, err = tx.ExecContext(ctx, `UPDATE approvals SET status='expired' WHERE id=? AND status='approved'`, a.ID)
		if err == nil {
			_, err = RecordAuditLog(ctx, tx, AuditLog{Actor: "system", Action: "approval.expire", TargetType: "approval", TargetID: a.ID,
				Detail: map[string]any{"executionId": executionID}})
		}
		return contract.AuthorizeResponse{Reason: "expired"}, err
	}
	if input.ContentID != a.ContentVersion {
		err = invalidateApprovalsWhereTx(ctx, tx, "content_changed", `id=?`, a.ID)
		return contract.AuthorizeResponse{Reason: "content_changed"}, err
	}
	// Compare the original timestamp verbatim after parsing it above. RFC3339
	// strings can contain differing fractional precision or UTC offset syntax.
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='consumed',use_count=use_count+1,consumed_at=?
		WHERE id=? AND execution_id=? AND operation=? AND status='approved'
		AND use_count < max_uses AND expires_at=? AND content_id=?`, now.UTC().Format(time.RFC3339Nano),
		a.ID, executionID, input.Operation, *a.ExpiresAt, input.ContentID)
	if err != nil {
		return deny, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return deny, err
	}
	return contract.AuthorizeResponse{Allowed: true}, nil
}
