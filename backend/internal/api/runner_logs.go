package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type runnerLogEvent struct {
	ID     string `json:"id"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

func logStream(stream string) bool {
	return stream == "harness" || stream == "setup" || stream == "test" || stream == "runner"
}

// Check ownership under the same transaction that will persist the report.
// Finished/older Executions still accept buffered logs from their owning Runner.
func logTransaction(w http.ResponseWriter, r *http.Request, db *sql.DB, logger *slog.Logger, auth *OwnerAuth) *sql.Tx {
	if err := ValidateID("id", r.PathValue("id"), ULID); err != nil {
		WriteError(w, logger, err)
		return nil
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		WriteError(w, logger, err)
		return nil
	}
	var owner sql.NullString
	err = tx.QueryRowContext(r.Context(), `SELECT runner_id FROM executions WHERE id = ?`, r.PathValue("id")).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		err = &Error{Code: NotFound, Message: "Execution not found"}
	}
	if err != nil {
		tx.Rollback()
		WriteError(w, logger, err)
		return nil
	}
	runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
	if runner == "" || !owner.Valid || owner.String != runner {
		tx.Rollback()
		auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot report this Execution")
		return nil
	}
	return tx
}

func reportLogEvents(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Events []runnerLogEvent `json:"events"`
		}
		if err := DecodeJSON(r, &input); err != nil {
			WriteError(w, logger, err)
			return
		}
		if len(input.Events) == 0 || len(input.Events) > 100 {
			WriteError(w, logger, invalid("events", "out_of_range"))
			return
		}
		for _, event := range input.Events {
			if event.ID == "" || len(event.ID) > 100 || !logStream(event.Stream) || strings.Contains(event.Text, "\n") {
				WriteError(w, logger, invalid("events", "invalid"))
				return
			}
		}
		tx := logTransaction(w, r, db, logger, auth)
		if tx == nil {
			return
		}
		defer tx.Rollback()
		executionID := r.PathValue("id")
		for _, event := range input.Events {
			// The stable source ID lives in the existing JSON payload. No
			// receipt table or schema extension is needed for retry detection.
			payload, _ := json.Marshal(struct {
				SourceID string `json:"sourceId"`
				Stream   string `json:"stream"`
				Text     string `json:"text"`
			}{event.ID, event.Stream, event.Text})
			var previous string
			err := tx.QueryRowContext(r.Context(), `SELECT payload FROM execution_events
				WHERE execution_id = ? AND type = 'log' AND json_extract(payload, '$.sourceId') = ?`,
				executionID, event.ID).Scan(&previous)
			if err == nil {
				if previous != string(payload) {
					WriteError(w, logger, &Error{Code: Conflict, Message: "Event ID already used for another payload"})
					return
				}
				continue
			}
			if errors.Is(err, sql.ErrNoRows) {
				_, err = store.AppendExecutionEvent(r.Context(), tx, executionID, model.EventLog, payload)
			}
			if err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			WriteError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
