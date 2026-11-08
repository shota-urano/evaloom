package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
)

// invalidateOpenApprovalsTx marks the unused (pending / approved) approvals of
// an Execution invalidated with reason (spec 17 3.6, 3.9) and records each
// invalidation in audit_logs with actor system (spec 17 3.9). The caller owns
// the transaction and must roll back on any error.
func invalidateOpenApprovalsTx(ctx context.Context, tx *sql.Tx, executionID, reason string) error {
	return invalidateApprovalsWhereTx(ctx, tx, reason, `execution_id = ?`, executionID)
}

// invalidateApprovalsWhereTx invalidates the unused approvals matching where
// (a fixed SQL fragment; values stay parameters) and audits each one.
func invalidateApprovalsWhereTx(ctx context.Context, tx *sql.Tx, reason, where string, args ...any) error {
	rows, err := tx.QueryContext(ctx, `UPDATE approvals SET status = 'invalidated', invalidated_reason = ?
		WHERE status IN ('pending', 'approved') AND `+where+` RETURNING id, task_id, execution_id, operation`,
		append([]any{reason}, args...)...)
	if err != nil {
		return err
	}
	type invalidated struct{ id, taskID, executionID, operation string }
	var changed []invalidated
	for rows.Next() {
		var row invalidated
		if err := rows.Scan(&row.id, &row.taskID, &row.executionID, &row.operation); err != nil {
			rows.Close()
			return err
		}
		changed = append(changed, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, row := range changed {
		if _, err := RecordAuditLog(ctx, tx, AuditLog{
			Actor: "system", Action: "approval.invalidate", TargetType: "approval", TargetID: row.id,
			Detail: map[string]any{"taskId": row.taskID, "executionId": row.executionID, "operation": row.operation, "reason": reason},
		}); err != nil {
			return err
		}
	}
	return nil
}

// MergeMethod is the PR merge method (spec 17 3.4, Q56: merge commit). It is
// part of params_hash and of the consume response.
const MergeMethod = "merge"

// MergeTarget is approvals.target for merge_pull_request (spec 17 3.4).
type MergeTarget struct {
	Repository string `json:"repository"`
	PRNumber   int    `json:"prNumber"`
	BaseBranch string `json:"baseBranch"`
	HeadBranch string `json:"headBranch"`
}

// MergeParamsHash is params_hash (spec 17 3.4): the SHA-256 of the
// normalized JSON of operation, target, content_version and merge method.
// encoding/json writes map keys in sorted order and struct fields in a fixed
// order, so equal values always produce the same bytes.
func MergeParamsHash(target MergeTarget, contentVersion string) (string, error) {
	encoded, err := json.Marshal(map[string]any{
		"operation": "merge_pull_request", "target": target,
		"contentVersion": contentVersion, "mergeMethod": MergeMethod,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ErrApprovalPrecondition means the Execution lacks what a merge approval
// binds to (spec 17 3.4): a GitHub PR URL matching pr_number, the pushed
// SHA and the branch. No approval is created (Fail Closed).
var ErrApprovalPrecondition = errors.New("store: PR approval requires the PR URL, pushed SHA and branch")

var githubPRURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/([1-9][0-9]*)$`)

// RequiresMergeApprovalTx reports whether a published Execution must wait
// for approval (spec 17 3.3): it has a PR and its Project's approval policy
// is not direct_push. Any other stored value counts as the default,
// require_approval (3.2), so an unexpected value never skips the approval.
func RequiresMergeApprovalTx(ctx context.Context, tx *sql.Tx, executionID string) (bool, error) {
	var required bool
	err := tx.QueryRowContext(ctx, `SELECT e.pr_number IS NOT NULL AND
		(CASE WHEN json_valid(p.approval_policy) THEN json_extract(p.approval_policy, '$.mergePullRequest') END) IS NOT 'direct_push'
		FROM executions e JOIN tasks t ON t.id = e.task_id JOIN projects p ON p.id = t.project_id
		WHERE e.id = ?`, executionID).Scan(&required)
	return required, err
}

// OpenMergeApprovalTx performs steps 1 and 2 of spec 17 3.3 for a PR the
// Execution created or updated: it invalidates every unused approval of the
// same PR (superseded, audited with actor system) and inserts a pending
// approval bound to the PR's head SHA (3.4). The caller moves the Execution
// to waiting_approval in the same transaction (step 3), which also records
// the notification (step 4). It returns the new approval ID.
func OpenMergeApprovalTx(ctx context.Context, tx *sql.Tx, executionID string) (string, error) {
	var taskID, prURL, pushed, head, base string
	var number sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT e.task_id, e.pr_number, coalesce(e.pr_url, ''),
		coalesce(e.pushed_sha, ''), coalesce(e.branch_name, ''), p.base_branch
		FROM executions e JOIN tasks t ON t.id = e.task_id JOIN projects p ON p.id = t.project_id
		WHERE e.id = ?`, executionID).Scan(&taskID, &number, &prURL, &pushed, &head, &base); err != nil {
		return "", err
	}
	match := githubPRURL.FindStringSubmatch(prURL)
	if !number.Valid || match == nil || match[2] != strconv.FormatInt(number.Int64, 10) || pushed == "" || head == "" {
		return "", ErrApprovalPrecondition
	}
	target := MergeTarget{Repository: match[1], PRNumber: int(number.Int64), BaseBranch: base, HeadBranch: head}
	hash, err := MergeParamsHash(target, pushed)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return "", err
	}
	// GitHub owner and repository names are case-insensitive.
	if err := invalidateApprovalsWhereTx(ctx, tx, "superseded", `operation = 'merge_pull_request'
		AND lower(json_extract(target, '$.repository')) = lower(?) AND json_extract(target, '$.prNumber') = ?`,
		target.Repository, target.PRNumber); err != nil {
		return "", err
	}
	id := ulid.Make().String()
	_, err = tx.ExecContext(ctx, `INSERT INTO approvals (id, task_id, execution_id, operation, target,
		content_version, params_hash, status, max_uses, use_count, created_at)
		VALUES (?, ?, ?, 'merge_pull_request', ?, ?, ?, 'pending', 1, 0, ?)`,
		id, taskID, executionID, string(encoded), pushed, hash, time.Now().UTC().Format(time.RFC3339Nano))
	return id, err
}
