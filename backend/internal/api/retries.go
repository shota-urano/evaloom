package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

func retryTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, TaskID); err != nil {
			WriteError(w, logger, err)
			return
		}
		var input struct {
			Mode string `json:"mode"`
		}
		err := DecodeJSON(r, &input)
		if err == nil {
			switch input.Mode {
			case "continue", "fresh":
			case "":
				err = invalid("mode", "required")
			default:
				err = invalid("mode", "invalid")
			}
		}
		if err == nil {
			err = queueTaskRetry(r.Context(), db, id, input.Mode)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// queueTaskRetry uses the same BEGIN IMMEDIATE transaction as create/cancel.
// The previous attempt stays intact; claim pins the new Profile and the Runner
// prepares or reuses the worktree after receiving the saved mode.
func queueTaskRetry(ctx context.Context, db *sql.DB, id, mode string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status model.TaskStatus
	var seq sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT t.status,
		(SELECT max(seq) FROM executions WHERE task_id = t.id)
		FROM tasks t WHERE t.id = ?`, id).Scan(&status, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Task not found"}
	}
	if err != nil {
		return err
	}
	switch status {
	case model.TaskFailed, model.TaskTimedOut, model.TaskCancelled, model.TaskCompleted, model.TaskBlocked:
	default:
		return &Error{Code: Conflict, Message: "Task state does not allow retry"}
	}
	if !seq.Valid {
		return &Error{Code: Conflict, Message: "Task has no latest Execution"}
	}
	now := time.Now().UTC()
	executionID, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES (?, ?, ?, ?, 'pending', ?)`, executionID.String(), id, seq.Int64+1, mode, now.Format(time.RFC3339))
	if err == nil {
		err = task.QueueExecutionTx(ctx, tx, id, executionID.String(), status)
	}
	if errors.Is(err, task.ErrConflict) {
		err = &Error{Code: Conflict, Message: err.Error()}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
