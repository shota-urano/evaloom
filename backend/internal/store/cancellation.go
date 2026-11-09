package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// ErrTaskNotFound is returned by CancelTaskTx for an unknown task ID.
var ErrTaskNotFound = errors.New("store: task not found")

// TaskCancellation is one request to stop a task (spec 04 3.5). The owner's
// POST /api/tasks/{id}/cancel and the Linear-caused cancellation (spec 15
// 3.7) share it, so both take the same path.
type TaskCancellation struct {
	TaskID string
	Actor  string // audit_logs.actor: "owner" or "system"
	// Reason is written to the audit detail when not empty (spec 15 3.7:
	// linear_label_removed and the like). The owner's request has none.
	Reason string
}

// CancelTaskTx applies spec 04 3.5 inside tx: a queued, blocked or
// waiting_approval (spec 17 3.6) task and its latest Execution become
// cancelled; for a running or verifying task, or a merging Execution, the
// Execution's cancel_requested_at is recorded and the Runner stops it on its
// next heartbeat (a merging one at consume, spec 17 3.5.2). A task held at Linear intake (blocked without an
// Execution, spec 15 3.6.3) becomes cancelled directly. Any other state is a
// task.ErrConflict. A repeated request for a running task keeps the first
// timestamp and writes nothing. The caller must roll back on any error.
func CancelTaskTx(ctx context.Context, tx *sql.Tx, c TaskCancellation) error {
	var status model.TaskStatus
	var executionID, heldCause sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT t.status, t.intake_block_cause,
		(SELECT id FROM executions WHERE task_id = t.id ORDER BY seq DESC LIMIT 1)
		FROM tasks t WHERE t.id = ?`, c.TaskID).Scan(&status, &heldCause, &executionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskNotFound
	}
	if err != nil {
		return err
	}
	if err := task.ValidateTaskTransition(status, model.TaskCancelled); err != nil {
		return err
	}
	audit := func(detail map[string]any) error {
		if c.Reason != "" {
			detail["reason"] = c.Reason
		}
		_, err := RecordAuditLog(ctx, tx, AuditLog{
			Actor: c.Actor, Action: "task.cancel", TargetType: "task", TargetID: c.TaskID, Detail: detail,
		})
		return err
	}
	if !executionID.Valid {
		if status != model.TaskBlocked || !heldCause.Valid {
			return &task.ConflictError{Code: "conflict", Message: "Task has no latest Execution"}
		}
		// Held at intake: there is no Execution or worktree to stop.
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ?
			WHERE id = ? AND status = ? AND NOT EXISTS (SELECT 1 FROM executions WHERE task_id = ?)`,
			model.TaskCancelled, time.Now().UTC().Format(time.RFC3339), c.TaskID, status, c.TaskID)
		if err := expectOneRow(result, err); err != nil {
			return err
		}
		// No Execution to key on: the task itself is the dedupe target
		// (spec 16 3.2, cancellation of a Linear task).
		if err := linearoutbox.EnqueueTx(ctx, tx, linearoutbox.Trigger{
			TaskID: c.TaskID, Kind: linearoutbox.KindCancelled, TargetID: c.TaskID,
		}); err != nil {
			return err
		}
		if c.Actor == "owner" && c.Reason == "" {
			if err := linearoutbox.EnqueueStateTx(ctx, tx, linearoutbox.Trigger{
				TaskID: c.TaskID, Kind: linearoutbox.KindCancelled, TargetID: c.TaskID,
			}); err != nil {
				return err
			}
		}
		return audit(map[string]any{"from": status, "to": model.TaskCancelled, "cancelRequested": false})
	}
	execution, err := GetContextExecution(ctx, tx, executionID.String)
	if err != nil {
		return err
	}
	expected, err := task.TaskStatusForExecution(execution.Status)
	if err != nil {
		return err
	}
	if expected != status {
		return &task.ConflictError{Code: "conflict", Message: "Task and latest Execution states do not match"}
	}
	// A merging Execution may already hold a consumed approval, so the Runner
	// decides at consume time (spec 17 3.5.2: cancel request -> invalidated).
	if status == model.TaskRunning || status == model.TaskVerifying || execution.Status == model.ExecutionMerging {
		// Preserve the first request on retries. State and latest-seq guards
		// prevent a delayed cancellation from modifying another attempt.
		result, err := tx.ExecContext(ctx, `UPDATE executions SET cancel_requested_at = ?
			WHERE id = ? AND status = ? AND cancel_requested_at IS NULL
			AND seq = (SELECT max(seq) FROM executions WHERE task_id = ?)
			AND EXISTS (SELECT 1 FROM tasks WHERE id = ? AND status = ?)`,
			time.Now().UTC().Format(time.RFC3339Nano), execution.ID, execution.Status, c.TaskID, c.TaskID, status)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 1 {
			// A request is not yet a state transition.
			return audit(map[string]any{"executionId": execution.ID, "from": status, "to": status, "cancelRequested": true})
		}
		var alreadyRequested bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM executions e JOIN tasks t ON t.id = e.task_id
			WHERE e.id = ? AND e.status = ? AND t.status = ? AND e.cancel_requested_at IS NOT NULL
			AND e.seq = (SELECT max(seq) FROM executions WHERE task_id = t.id))`,
			execution.ID, execution.Status, status).Scan(&alreadyRequested); err != nil {
			return err
		}
		if !alreadyRequested {
			return &task.ConflictError{Code: "conflict", Message: "State changed before cancellation could be requested"}
		}
		return nil
	}
	// Spec 17 3.6: cancelling a task waiting for approval invalidates its
	// unused approvals in the same transaction.
	if execution.Status == model.ExecutionWaitingApproval {
		if err := invalidateOpenApprovalsTx(ctx, tx, execution.ID, "cancelled"); err != nil {
			return err
		}
	}
	// Same result metadata and transition effects as a reported cancellation:
	// error classification, finish time, retained worktree and cancellation's
	// notification exclusion.
	if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ? WHERE id = ?`,
		model.ErrorCancelled, execution.ID); err != nil {
		return err
	}
	// Record the requester before the shared transition writes its Linear
	// state row. Both the audit and transition roll back together on error.
	if err := audit(map[string]any{"executionId": execution.ID, "from": status, "to": model.TaskCancelled, "cancelRequested": false}); err != nil {
		return err
	}
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: c.TaskID, ExecutionID: execution.ID, FromTask: status,
		FromExecution: execution.Status, ToExecution: model.ExecutionCancelled,
	}); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"from": string(execution.Status), "to": "cancelled", "reason": "task_cancelled",
	})
	if _, err := AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload); err != nil {
		return err
	}
	return nil
}

func expectOneRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return &task.ConflictError{Code: "conflict", Message: "state changed before the transition could be applied"}
	}
	return nil
}
