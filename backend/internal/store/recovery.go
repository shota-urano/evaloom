package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// StartupHeartbeatGrace allows a live Runner to reconnect after the HTTP
// listener starts, even when the Control Plane was unavailable for a long time.
// Thirty seconds tolerates local scheduling/reconnection delays without leaving
// abandoned Executions active indefinitely. This is not a runtime watchdog.
const StartupHeartbeatGrace = 30 * time.Second

// RecoverStaleRunners performs startup-only recovery after the grace period.
// Any Runner that heartbeats since startedAt keeps its Executions. A recent
// pre-start heartbeat is also protected until it is at least the grace age.
// Time comparisons use parsed timestamps, not variable-width RFC3339 text.
// Callers may retry database failures; all transitions and records are atomic.
func RecoverStaleRunners(ctx context.Context, db *sql.DB, startedAt, now time.Time) error {
	if now.Before(startedAt.Add(StartupHeartbeatGrace)) {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, coalesce(last_heartbeat_at, '') FROM runners r
		WHERE EXISTS (SELECT 1 FROM executions e WHERE e.runner_id = r.id AND e.status IN `+activeExecutionStatuses+`)
		OR EXISTS (SELECT 1 FROM route_proposals rp WHERE rp.runner_id = r.id AND rp.status = 'running')
		ORDER BY id`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id, heartbeat string
		if err := rows.Scan(&id, &heartbeat); err != nil {
			rows.Close()
			return err
		}
		if heartbeat != "" {
			last, err := time.Parse(time.RFC3339Nano, heartbeat)
			if err != nil {
				rows.Close()
				return fmt.Errorf("store: invalid Runner heartbeat timestamp for %s: %w", id, err)
			}
			if !last.Before(startedAt) || now.Sub(last) < StartupHeartbeatGrace {
				continue
			}
		}
		stale = append(stale, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range stale {
		if err := interruptRunnerTx(ctx, tx, id, "runner_heartbeat_lost"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runners SET status = 'offline' WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Both recovery paths use the same lifecycle and recording code. The caller's
// BEGIN IMMEDIATE serializes heartbeat, result, claim and owner operations.
// Spec 04 3.9: active Executions become interrupted (task failed), their
// running stage_runs failed (interrupted) and their workspace retained; the
// Runner's running route proposals become failed (interrupted) with a route
// decision while the task is still routing (Q133). Nothing is re-executed.
func interruptRunnerTx(ctx context.Context, tx *sql.Tx, runnerID, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.task_id, e.status, t.status
		FROM executions e JOIN tasks t ON t.id = e.task_id
		WHERE e.runner_id = ? AND e.status IN `+activeExecutionStatuses+` ORDER BY e.id`, runnerID)
	if err != nil {
		return err
	}
	var changes []task.Transition
	for rows.Next() {
		var change task.Transition
		if err := rows.Scan(&change.ExecutionID, &change.TaskID, &change.FromExecution, &change.FromTask); err != nil {
			rows.Close()
			return err
		}
		change.ToExecution = model.ExecutionInterrupted
		changes = append(changes, change)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, change := range changes {
		// Classify first: the transition writes the Linear comment body from
		// the classification in this transaction (spec 16 3.3).
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = 'interrupted', error_message = ? WHERE id = ?`, reason, change.ExecutionID); err != nil {
			return err
		}
		if err := task.TransitionExecutionTx(ctx, tx, change); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `UPDATE stage_runs SET status = 'failed', error_class = 'interrupted', finished_at = ?
			WHERE execution_id = ? AND status = 'running'`, now, change.ExecutionID); err != nil {
			return err
		}
		// Never delete the workspace (spec 04 3.9 step 4); a cleaned row stays cleaned.
		if _, err := tx.ExecContext(ctx, `UPDATE task_workspaces SET status = 'retained'
			WHERE id = (SELECT workspace_id FROM executions WHERE id = ?) AND status = 'active'`, change.ExecutionID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"from": string(change.FromExecution), "to": "interrupted", "reason": reason})
		if _, err := AppendExecutionEvent(ctx, tx, change.ExecutionID, model.EventStatusChanged, payload); err != nil {
			return err
		}
		if _, err := RecordAuditLog(ctx, tx, AuditLog{
			Actor: "system", Action: "execution.interrupt", TargetType: "execution", TargetID: change.ExecutionID,
			Detail: map[string]any{"taskId": change.TaskID, "runnerId": runnerID, "from": string(change.FromExecution), "to": "interrupted", "reason": reason},
		}); err != nil {
			return err
		}
	}
	return interruptRouteProposalsTx(ctx, tx, runnerID)
}

func interruptRouteProposalsTx(ctx context.Context, tx *sql.Tx, runnerID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT rp.id, rp.task_id, t.status FROM route_proposals rp JOIN tasks t ON t.id = rp.task_id
		WHERE rp.runner_id = ? AND rp.status = 'running' ORDER BY rp.id`, runnerID)
	if err != nil {
		return err
	}
	type proposal struct{ id, taskID, taskStatus string }
	var proposals []proposal
	for rows.Next() {
		var p proposal
		if err := rows.Scan(&p.id, &p.taskID, &p.taskStatus); err != nil {
			rows.Close()
			return err
		}
		proposals = append(proposals, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, p := range proposals {
		result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = 'failed', error_class = 'interrupted', finished_at = ?
			WHERE id = ? AND status = 'running'`, now, p.id)
		if err := expectOneRow(result, err); err != nil {
			return err
		}
		if model.TaskStatus(p.taskStatus) != model.TaskRouting {
			continue
		}
		if err := OpenRouteDecisionTx(ctx, tx, p.taskID, p.id); err != nil {
			return err
		}
	}
	return nil
}
