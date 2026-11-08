package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// Merge failure reasons the Git Publisher reports with errorClass
// publish_failed (spec 17 3.5.2, 3.5.3, 6章). They are stored in
// executions.error_message, as approval_rejected is (Q57).
const (
	// GitHub refused the merge because the PR head is not content_version.
	mergeContentChanged = "content_changed"
	// GitHub refused the merge for another reason (not mergeable, etc.).
	mergeRejected = "merge_rejected"
	// The merge was requested but its result is not known (3.5.3).
	mergeUnknown = "merge_unknown"
)

// saveMergeResult records the result of an approved merge (spec 17 3.5.2
// step 4). Only the latest merging Execution with a consumed approval can be
// reported, and only as succeeded (with the merge commit SHA) or failed
// (publish_failed with a merge failure reason). On a content_changed failure
// the consumed approval becomes invalidated (6章).
func saveMergeResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, runner string, input resultInput) error {
	if !execution.Latest || input.FromStatus != string(model.ExecutionMerging) ||
		(input.Status != string(model.ExecutionSucceeded) && input.Status != string(model.ExecutionFailed)) {
		return &Error{Code: Conflict, Message: "Result does not match an active, latest Execution transition"}
	}
	ctx := r.Context()
	if consumed, err := store.HasConsumedMergeTx(ctx, tx, execution.ID); err != nil {
		return err
	} else if !consumed {
		return &Error{Code: Conflict, Message: "Merge result requires a consumed approval of this Execution"}
	}
	if err := validateMergeResultFields(input); err != nil {
		return err
	}
	result, invalidate := map[string]string{}, ""
	if input.Status == string(model.ExecutionSucceeded) {
		result["mergeCommitSha"] = *input.MergeCommitSHA
	} else {
		result["error"] = *input.ErrorMessage
		if *input.ErrorMessage == mergeContentChanged {
			invalidate = mergeContentChanged
		}
	}
	approvalID, err := store.RecordMergeResultTx(ctx, tx, execution.ID, result, invalidate)
	if errors.Is(err, store.ErrMergeNotConsumed) {
		return &Error{Code: Conflict, Message: "Merge result requires a consumed approval of this Execution"}
	}
	if err != nil {
		return err
	}
	if input.Status == string(model.ExecutionFailed) {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ?, error_message = ? WHERE id = ?`,
			model.ErrorPublishFailed, *input.ErrorMessage, execution.ID); err != nil {
			return err
		}
	}
	to := model.ExecutionStatus(input.Status)
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: execution.TaskID, ExecutionID: execution.ID, FromTask: execution.TaskStatus,
		FromExecution: model.ExecutionMerging, ToExecution: to,
	}); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"from": string(model.ExecutionMerging), "to": string(to), "reason": "runner_result"})
	if _, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload); err != nil {
		return err
	}
	detail := map[string]any{"taskId": execution.TaskID, "from": model.ExecutionMerging, "to": to, "approvalId": approvalID}
	for key, value := range result {
		detail[key] = value
	}
	_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
		Actor: "runner:" + runner, Action: "execution.result", TargetType: "execution", TargetID: execution.ID, Detail: detail,
	})
	return err
}

// validateMergeResultFields accepts only the merge outcome: no stage metadata,
// Gate results, usage or Block can be reported from merging.
func validateMergeResultFields(input resultInput) error {
	others := input.WorktreeID != nil || input.BranchName != nil || input.BaseSHA != nil || input.HarnessHeadSHA != nil ||
		input.VerifiedSHA != nil || input.PushedSHA != nil || input.PRNumber != nil || input.PRURL != nil ||
		input.HarnessSessionID != nil || input.Block != nil || input.Usage != nil || len(input.GateResults) > 0 ||
		input.Baseline != nil || len(input.TestRuns) > 0 || input.SetupStatus != nil || input.Worktree != nil ||
		input.PRCreated != nil || input.Audit != nil
	if others {
		return invalid("mergeCommitSha", "only_merge_result")
	}
	if input.Status == string(model.ExecutionSucceeded) {
		switch {
		case input.MergeCommitSHA == nil:
			return invalid("mergeCommitSha", "required")
		case !fullCommitSHA(*input.MergeCommitSHA):
			return invalid("mergeCommitSha", "invalid_format")
		case input.ErrorClass != "" || input.ErrorMessage != nil:
			return invalid("errorClass", "only_for_failure")
		}
		return nil
	}
	if input.MergeCommitSHA != nil {
		return invalid("mergeCommitSha", "only_for_succeeded")
	}
	if input.ErrorClass != model.ErrorPublishFailed {
		return invalid("errorClass", "publish_failed_required")
	}
	if input.ErrorMessage == nil {
		return invalid("errorMessage", "required")
	}
	switch *input.ErrorMessage {
	case mergeContentChanged, mergeRejected, mergeUnknown:
		return nil
	}
	return invalid("errorMessage", "invalid_value")
}
