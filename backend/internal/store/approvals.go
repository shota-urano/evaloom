package store

import (
	"context"
	"database/sql"
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
