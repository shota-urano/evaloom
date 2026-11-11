package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// claimExecution is POST /api/runners/{id}/claim (spec 04 3.3, 3.3.1).
func claimExecution(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// protectRunner has already matched this identity to the route's Runner.
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		if runner == "" {
			WriteError(w, logger, &Error{Code: Forbidden, Message: "Runner identity required"})
			return
		}
		var input struct {
			WorkspaceRoot *string `json:"workspaceRoot"`
		}
		err := DecodeJSON(r, &input)
		if err == nil && input.WorkspaceRoot == nil {
			err = invalid("workspaceRoot", "required")
		}
		if err == nil && !store.ValidWorkspaceRoot(*input.WorkspaceRoot) {
			err = invalid("workspaceRoot", "invalid_format")
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil) // store.Open configures BEGIN IMMEDIATE.
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		response, err := store.Claim(ctx, tx, store.ClaimInput{
			RunnerID: runner, WorkspaceRoot: *input.WorkspaceRoot,
		})
		if errors.Is(err, task.ErrConflict) {
			err = &Error{Code: Conflict, Message: err.Error()}
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}
