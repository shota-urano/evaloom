package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func runnerHeartbeat(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			BootID              string   `json:"bootId"`
			RunningExecutionIDs []string `json:"runningExecutionIds"`
		}
		err := DecodeJSON(r, &input)
		if err == nil && strings.TrimSpace(input.BootID) == "" {
			err = invalid("bootId", "required")
		}
		if err == nil && input.RunningExecutionIDs == nil {
			err = invalid("runningExecutionIds", "required")
		}
		for _, id := range input.RunningExecutionIDs {
			if err != nil {
				break
			}
			err = ValidateID("runningExecutionIds", id, ULID)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		ids, err := store.Heartbeat(r.Context(), db, runner, input.BootID, input.RunningExecutionIDs)
		if errors.Is(err, store.ErrHeartbeatExecutionOwner) {
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot report this Execution")
			return
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			CancelExecutionIDs []string `json:"cancelExecutionIds"`
		}{ids})
	}
}
