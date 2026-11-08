package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// ConsumeApprovalTx consumes an approved approval (spec 17 3.5.2). The update
// is conditional on the expected current values (approved, unused, not past
// expires_at), so a concurrent consume, decision or invalidation leaves it
// unchanged and ConsumeApprovalTx returns false: the caller must not operate.
func ConsumeApprovalTx(ctx context.Context, tx *sql.Tx, id, now string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status = 'consumed', use_count = use_count + 1, consumed_at = ?
		WHERE id = ? AND status = 'approved' AND use_count < max_uses
		AND expires_at IS NOT NULL AND julianday(expires_at) > julianday(?)`, now, id, now)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ExpireApprovalTx marks an approved, unused approval past expires_at
// expired (spec 17 3.5.2). A row in any other state yields ErrApprovalStale.
func ExpireApprovalTx(ctx context.Context, tx *sql.Tx, id, now string) error {
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status = 'expired'
		WHERE id = ? AND status = 'approved' AND use_count = 0 AND julianday(expires_at) <= julianday(?)`, id, now)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrApprovalStale
	}
	return nil
}

// InvalidateApprovalTx marks one unused approval invalidated with reason and
// audits it with actor system (spec 17 3.5.2, 3.9). A row that is no longer
// pending or approved yields ErrApprovalStale.
func InvalidateApprovalTx(ctx context.Context, tx *sql.Tx, id, reason string) error {
	var open bool
	if err := tx.QueryRowContext(ctx, `SELECT status IN ('pending', 'approved') FROM approvals WHERE id = ?`, id).Scan(&open); err != nil {
		return err
	}
	if !open {
		return ErrApprovalStale
	}
	return invalidateApprovalsWhereTx(ctx, tx, reason, `id = ?`, id)
}

// ApprovalCancelRequested reports whether the approval's Execution has a
// pending cancellation request (spec 04 3.5, spec 17 3.5.2).
func ApprovalCancelRequested(ctx context.Context, tx *sql.Tx, executionID string) (bool, error) {
	var requested bool
	err := tx.QueryRowContext(ctx, `SELECT cancel_requested_at IS NOT NULL FROM executions WHERE id = ?`, executionID).Scan(&requested)
	return requested, err
}

// MergeTargetOf decodes approvals.target of a merge_pull_request approval.
func MergeTargetOf(a Approval) (MergeTarget, error) {
	var target MergeTarget
	err := json.Unmarshal(a.Target, &target)
	return target, err
}
