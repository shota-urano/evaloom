package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// pullRequestClosed is the failure reason of a PR the owner closed on GitHub
// while it waited for approval (spec 17 3.6, Q58). It is stored in
// executions.error_message with an empty error_class, as approval_rejected
// is (Q57): 02 5.4 has no error_class for it.
const pullRequestClosed = "pull_request_closed"

// saveApprovalWaitResult records what the Runner's periodic check read from
// GitHub for a PR waiting for approval (spec 17 3.6, Q58): merged directly
// on GitHub completes the task (succeeded with the merge commit), closed
// fails it (pull_request_closed). In both cases the unused approvals become
// invalidated (target_changed). Only the latest waiting_approval Execution
// with a merge approval can be reported.
func saveApprovalWaitResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, runner string, input resultInput) error {
	if !execution.Latest || execution.TaskStatus != model.TaskWaitingApproval ||
		input.FromStatus != string(model.ExecutionWaitingApproval) ||
		(input.Status != string(model.ExecutionSucceeded) && input.Status != string(model.ExecutionFailed)) {
		return &Error{Code: Conflict, Message: "Result does not match an active, latest Execution transition"}
	}
	ctx := r.Context()
	if found, err := store.HasMergeApprovalTx(ctx, tx, execution.ID); err != nil {
		return err
	} else if !found {
		return &Error{Code: Conflict, Message: "Execution has no pull request waiting for approval"}
	}
	if err := validateApprovalWaitFields(input); err != nil {
		return err
	}
	if err := store.InvalidateUnusedApprovalsTx(ctx, tx, execution.ID, "target_changed"); err != nil {
		return err
	}
	to, reason := model.ExecutionStatus(input.Status), "pull_request_merged"
	detail := map[string]any{"taskId": execution.TaskID, "from": model.ExecutionWaitingApproval, "to": to}
	if to == model.ExecutionSucceeded {
		detail["mergeCommitSha"] = *input.MergeCommitSHA
	} else {
		reason = pullRequestClosed
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_message = ? WHERE id = ?`, pullRequestClosed, execution.ID); err != nil {
			return err
		}
	}
	detail["reason"] = reason
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: execution.TaskID, ExecutionID: execution.ID, FromTask: model.TaskWaitingApproval,
		FromExecution: model.ExecutionWaitingApproval, ToExecution: to,
	}); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"from": string(model.ExecutionWaitingApproval), "to": string(to), "reason": reason})
	if _, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload); err != nil {
		return err
	}
	_, err := store.RecordAuditLog(ctx, tx, store.AuditLog{
		Actor: "runner:" + runner, Action: "execution.result", TargetType: "execution", TargetID: execution.ID, Detail: detail,
	})
	return err
}

// validateApprovalWaitFields accepts only the PR outcome read on GitHub:
// succeeded with mergeCommitSha, or failed with errorMessage
// pull_request_closed and no errorClass.
func validateApprovalWaitFields(input resultInput) error {
	others := input.WorktreeID != nil || input.BranchName != nil || input.BaseSHA != nil || input.HarnessHeadSHA != nil ||
		input.VerifiedSHA != nil || input.PushedSHA != nil || input.PRNumber != nil || input.PRURL != nil ||
		input.HarnessSessionID != nil || input.Block != nil || input.Usage != nil || len(input.GateResults) > 0 ||
		input.Baseline != nil || len(input.TestRuns) > 0 || input.SetupStatus != nil || input.Worktree != nil ||
		input.PRCreated != nil || input.Audit != nil
	if others {
		return invalid("status", "only_pull_request_outcome")
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
	switch {
	case input.MergeCommitSHA != nil:
		return invalid("mergeCommitSha", "only_for_succeeded")
	case input.ErrorClass != "":
		return invalid("errorClass", "not_allowed")
	case input.ErrorMessage == nil:
		return invalid("errorMessage", "required")
	case *input.ErrorMessage != pullRequestClosed:
		return invalid("errorMessage", "invalid_value")
	}
	return nil
}
