package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func authorizeExecution(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := ValidateID("id", r.PathValue("id"), ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		var input contract.AuthorizeRequest
		if err := DecodeJSON(r, &input); err != nil {
			WriteError(w, logger, err)
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			writeAuthorizeFailure(w, logger, err)
			return
		}
		defer tx.Rollback()
		runnerID, _ := r.Context().Value(runnerIdentityKey{}).(string)
		reply, err := store.AuthorizeExecutionTx(r.Context(), tx, r.PathValue("id"), runnerID, input, time.Now().UTC())
		if errors.Is(err, store.ErrAuthorizeRunner) {
			tx.Rollback() // release the connection before persisting rejection audit
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot authorize this Execution")
			return
		}
		if errors.Is(err, store.ErrExecutionNotFound) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		} else if errors.Is(err, store.ErrAuthorizeState) {
			err = &Error{Code: Conflict, Message: "Execution is not publishing"}
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			var public *Error
			if errors.As(err, &public) {
				WriteError(w, logger, err)
			} else {
				writeAuthorizeFailure(w, logger, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, reply)
	}
}

// Spec 01 requires an explicit denial on DB failure. Spec 17 does not assign
// an API reason to this case (unavailable belongs to Runner), so keep the
// common internal-error envelope and omit reason rather than inventing one.
func writeAuthorizeFailure(w http.ResponseWriter, logger *slog.Logger, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("API authorize failed", "error", err)
	writeJSON(w, http.StatusInternalServerError, struct {
		Allowed bool   `json:"allowed"`
		Error   *Error `json:"error"`
	}{Error: &Error{Code: Internal, Message: "internal server error", Details: []Detail{}}})
}
