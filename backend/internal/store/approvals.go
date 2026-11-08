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
	rows, err := tx.QueryContext(ctx, `UPDATE approvals SET status = 'invalidated', invalidated_reason = ?
		WHERE execution_id = ? AND status IN ('pending', 'approved') RETURNING id, task_id, operation`, reason, executionID)
	if err != nil {
		return err
	}
	type invalidated struct{ id, taskID, operation string }
	var changed []invalidated
	for rows.Next() {
		var row invalidated
		if err := rows.Scan(&row.id, &row.taskID, &row.operation); err != nil {
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
			Detail: map[string]any{"taskId": row.taskID, "executionId": executionID, "operation": row.operation, "reason": reason},
		}); err != nil {
			return err
		}
	}
	return nil
}
