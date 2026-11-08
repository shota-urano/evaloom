package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// approvalConsumeInput is the PR state the Git Publisher read from GitHub
// just before the operation (spec 17 3.5.2 step 1): head SHA, base branch,
// whether it is open and whether it is merged.
type approvalConsumeInput struct {
	HeadSHA    string `json:"headSha"`
	BaseBranch string `json:"baseBranch"`
	Open       *bool  `json:"open"`
	Merged     *bool  `json:"merged"`
}

// approvalConsumeResponse is the 5章 consume response.
type approvalConsumeResponse struct {
	Allowed        bool   `json:"allowed"`
	ContentVersion string `json:"contentVersion,omitempty"`
	MergeMethod    string `json:"mergeMethod,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// Refusal reasons of consume. content_changed, target_changed and cancelled
// are the invalidated_reason values of the 3.5.2 table; the others name the
// rows that leave the approval as it is (or mark it expired).
const (
	consumeContentChanged = "content_changed"
	consumeTargetChanged  = "target_changed"
	consumeCancelled      = "cancelled"
	consumeExpired        = "expired"
	consumeAlreadyUsed    = "already_used"
	consumeNotApproved    = "not_approved"
)

func validateConsumeInput(input approvalConsumeInput) error {
	switch {
	case input.HeadSHA == "":
		return invalid("headSha", "required")
	case !fullCommitSHA(input.HeadSHA):
		return invalid("headSha", "invalid_format")
	case strings.TrimSpace(input.BaseBranch) == "":
		return invalid("baseBranch", "required")
	case input.Open == nil:
		return invalid("open", "required")
	case input.Merged == nil:
		return invalid("merged", "required")
	}
	return nil
}

// consumeApproval is POST /api/approvals/{id}/consume (spec 17 3.5.2, 5章).
func consumeApproval(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := ValidateID("id", id, ULID)
		var input approvalConsumeInput
		if err == nil {
			err = DecodeJSON(r, &input)
		}
		if err == nil {
			err = validateConsumeInput(input)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		response, err := saveApprovalConsume(r.Context(), db, id, runner, input)
		if errors.Is(err, errConsumeRunnerMismatch) {
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot consume this approval")
			return
		}
		if err != nil {
			WriteError(w, logger, approvalError(err))
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

var errConsumeRunnerMismatch = errors.New("api: approval belongs to another Runner's Execution")

// saveApprovalConsume judges the 3.5.2 table inside one write transaction
// (BEGIN IMMEDIATE serializes it with decisions, cancellations and other
// consumes) and consumes the approval with a conditional update. Every
// call is audited with actor runner:<id>; invalidations are audited with
// actor system (3.9).
func saveApprovalConsume(ctx context.Context, db *sql.DB, id, runner string, input approvalConsumeInput) (approvalConsumeResponse, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return approvalConsumeResponse{}, err
	}
	defer tx.Rollback()
	approval, execution, err := readApprovalForUpdate(ctx, tx, id)
	if err != nil {
		return approvalConsumeResponse{}, err
	}
	if runner == "" || execution.RunnerID != runner {
		return approvalConsumeResponse{}, errConsumeRunnerMismatch
	}
	now := time.Now().UTC()
	reason, err := judgeApprovalConsume(ctx, tx, approval, execution, input, now)
	if err != nil {
		return approvalConsumeResponse{}, err
	}
	response := approvalConsumeResponse{Reason: reason}
	if reason == "" {
		consumed, err := store.ConsumeApprovalTx(ctx, tx, id, now.Format(time.RFC3339Nano))
		if err != nil {
			return response, err
		}
		if consumed {
			response = approvalConsumeResponse{Allowed: true, ContentVersion: approval.ContentVersion, MergeMethod: store.MergeMethod}
		} else {
			response.Reason = consumeAlreadyUsed
		}
	}
	detail := map[string]any{"taskId": approval.TaskID, "executionId": approval.ExecutionID, "operation": approval.Operation,
		"contentVersion": approval.ContentVersion, "headSha": input.HeadSHA, "allowed": response.Allowed}
	if response.Reason != "" {
		detail["reason"] = response.Reason
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{
		Actor: "runner:" + runner, Action: "approval.consume", TargetType: "approval", TargetID: id, Detail: detail,
	}); err != nil {
		return response, err
	}
	return response, tx.Commit()
}

// judgeApprovalConsume returns the refusal reason, or "" when the approval
// may be consumed, applying the approval and Execution changes of a refusal.
func judgeApprovalConsume(ctx context.Context, tx *sql.Tx, approval store.Approval, execution store.ContextExecution,
	input approvalConsumeInput, now time.Time) (string, error) {
	// Rows that leave the approval unchanged: used, or never usable.
	switch {
	case approval.Status == "consumed" || approval.UseCount >= approval.MaxUses:
		return consumeAlreadyUsed, nil
	case approval.Status == "expired":
		return consumeExpired, nil
	case approval.Status == "invalidated" && approval.InvalidatedReason != nil:
		return *approval.InvalidatedReason, nil
	case approval.Status != "approved":
		return consumeNotApproved, nil
	}
	if !execution.Latest || execution.Status != model.ExecutionMerging || execution.TaskStatus != model.TaskWaitingApproval {
		return "", &Error{Code: Conflict, Message: "Consume requires the latest Execution waiting for the merge"}
	}
	stamp := now.Format(time.RFC3339Nano)
	cancelled, err := store.ApprovalCancelRequested(ctx, tx, execution.ID)
	if err != nil {
		return "", err
	}
	if cancelled {
		return consumeCancelled, cancelMergingExecution(ctx, tx, approval, execution)
	}
	if store.ApprovalExpired(approval, now) {
		if err := store.ExpireApprovalTx(ctx, tx, approval.ID, stamp); err != nil {
			return "", err
		}
		return consumeExpired, returnToApprovalWait(ctx, tx, execution, "approval_expired")
	}
	target, targetErr := store.MergeTargetOf(approval)
	if targetErr == nil && (*input.Merged || !*input.Open || input.BaseBranch != target.BaseBranch) {
		return consumeTargetChanged, invalidateAndWait(ctx, tx, approval, execution, consumeTargetChanged)
	}
	if input.HeadSHA != approval.ContentVersion {
		return consumeContentChanged, invalidateAndWait(ctx, tx, approval, execution, consumeContentChanged)
	}
	// params_hash binds operation, target, content_version and merge method
	// (3.4). A row whose stored values no longer produce it is not used.
	if targetErr != nil || approval.Operation != "merge_pull_request" {
		return consumeContentChanged, invalidateAndWait(ctx, tx, approval, execution, consumeContentChanged)
	}
	hash, err := store.MergeParamsHash(target, approval.ContentVersion)
	if err != nil {
		return "", err
	}
	if hash != approval.ParamsHash {
		return consumeContentChanged, invalidateAndWait(ctx, tx, approval, execution, consumeContentChanged)
	}
	return "", nil
}

// invalidateAndWait invalidates the approval and returns the Execution from
// merging to waiting_approval (spec 17 3.5.2, task transition table).
func invalidateAndWait(ctx context.Context, tx *sql.Tx, approval store.Approval, execution store.ContextExecution, reason string) error {
	if err := store.InvalidateApprovalTx(ctx, tx, approval.ID, reason); err != nil {
		return err
	}
	return returnToApprovalWait(ctx, tx, execution, "approval_"+reason)
}

func returnToApprovalWait(ctx context.Context, tx *sql.Tx, execution store.ContextExecution, reason string) error {
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: execution.TaskID, ExecutionID: execution.ID, FromTask: model.TaskWaitingApproval,
		FromExecution: model.ExecutionMerging, ToExecution: model.ExecutionWaitingApproval,
	}); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"from": string(model.ExecutionMerging),
		"to": string(model.ExecutionWaitingApproval), "reason": reason})
	_, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload)
	return err
}

// cancelMergingExecution ends a merging Execution whose task has a pending
// cancellation (spec 17 3.5.2, 3.6): the approval is invalidated
// (cancelled) and the Execution and task become cancelled with the same
// metadata as the owner's cancellation of a waiting Execution (spec 04 3.5).
func cancelMergingExecution(ctx context.Context, tx *sql.Tx, approval store.Approval, execution store.ContextExecution) error {
	if err := store.InvalidateApprovalTx(ctx, tx, approval.ID, consumeCancelled); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ? WHERE id = ?`, model.ErrorCancelled, execution.ID); err != nil {
		return err
	}
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: execution.TaskID, ExecutionID: execution.ID, FromTask: model.TaskWaitingApproval,
		FromExecution: model.ExecutionMerging, ToExecution: model.ExecutionCancelled,
	}); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"from": string(model.ExecutionMerging),
		"to": string(model.ExecutionCancelled), "reason": "task_cancelled"})
	_, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload)
	return err
}
