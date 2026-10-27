package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

func cancelTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, TaskID); err != nil {
			WriteError(w, logger, err)
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil) // Same BEGIN IMMEDIATE reservation as claim/result.
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		var status model.TaskStatus
		var executionID sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT t.status,
			(SELECT id FROM executions WHERE task_id = t.id ORDER BY seq DESC LIMIT 1)
			FROM tasks t WHERE t.id = ?`, id).Scan(&status, &executionID)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Task not found"}
		}
		if err == nil {
			err = task.ValidateTaskTransition(status, model.TaskCancelled)
		}
		if err == nil && !executionID.Valid {
			err = &Error{Code: Conflict, Message: "Task has no latest Execution"}
		}
		var execution store.ContextExecution
		if err == nil {
			execution, err = store.GetContextExecution(ctx, tx, executionID.String)
		}
		if err == nil {
			expected, mappingErr := task.TaskStatusForExecution(execution.Status)
			if mappingErr != nil {
				err = mappingErr
			} else if expected != status {
				err = &Error{Code: Conflict, Message: "Task and latest Execution states do not match"}
			}
		}
		requested := status == model.TaskRunning || status == model.TaskVerifying
		changed := true
		if err == nil && requested {
			// Preserve the first request on retries. State and latest-seq guards
			// prevent a delayed cancellation from modifying another attempt.
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE executions SET cancel_requested_at = ?
				WHERE id = ? AND status = ? AND cancel_requested_at IS NULL
				AND seq = (SELECT max(seq) FROM executions WHERE task_id = ?)
				AND EXISTS (SELECT 1 FROM tasks WHERE id = ? AND status = ?)`,
				time.Now().UTC().Format(time.RFC3339Nano), execution.ID, execution.Status, id, id, status)
			if err == nil {
				var count int64
				count, err = result.RowsAffected()
				changed = count == 1
				if err == nil && !changed {
					var alreadyRequested bool
					err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM executions e JOIN tasks t ON t.id = e.task_id
						WHERE e.id = ? AND e.status = ? AND t.status = ? AND e.cancel_requested_at IS NOT NULL
						AND e.seq = (SELECT max(seq) FROM executions WHERE task_id = t.id))`,
						execution.ID, execution.Status, status).Scan(&alreadyRequested)
					if err == nil && !alreadyRequested {
						err = &Error{Code: Conflict, Message: "State changed before cancellation could be requested"}
					}
				}
			}
		} else if err == nil {
			// Reuse result metadata and transition effects: error classification,
			// finish time, retained worktree and cancellation's notification exclusion.
			err = saveResult(r, tx, execution, resultInput{Status: string(model.ExecutionCancelled)})
			if err == nil {
				err = task.TransitionExecutionTx(ctx, tx, task.Transition{
					TaskID: id, ExecutionID: execution.ID, FromTask: status,
					FromExecution: execution.Status, ToExecution: model.ExecutionCancelled,
				})
			}
			if err == nil {
				payload, _ := json.Marshal(map[string]string{
					"from": string(execution.Status), "to": "cancelled", "reason": "task_cancelled",
				})
				_, err = store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload)
			}
		}
		if err == nil && changed {
			to := model.TaskCancelled
			if requested {
				to = status // A request is not yet a state transition.
			}
			_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
				Actor: "owner", Action: "task.cancel", TargetType: "task", TargetID: id,
				Detail: map[string]any{"executionId": execution.ID, "from": status, "to": to, "cancelRequested": requested},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if errors.Is(err, task.ErrConflict) {
			err = &Error{Code: Conflict, Message: err.Error()}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
