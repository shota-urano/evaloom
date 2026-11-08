package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// cancelTask is the owner's cancellation (spec 04 3.5). The state changes
// live in store.CancelTaskTx, which the Linear-caused cancellation (spec 15
// 3.7) shares.
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
		err = store.CancelTaskTx(ctx, tx, store.TaskCancellation{TaskID: id, Actor: "owner"})
		if err == nil {
			err = tx.Commit()
		}
		if errors.Is(err, store.ErrTaskNotFound) {
			err = &Error{Code: NotFound, Message: "Task not found"}
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
