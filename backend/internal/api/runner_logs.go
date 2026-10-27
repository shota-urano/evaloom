package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
			payload, _ := json.Marshal(struct {
				Stream string `json:"stream"`
				Text   string `json:"text"`
			}{event.Stream, event.Text})
			hash := sha256.Sum256(payload)
			digest := hex.EncodeToString(hash[:])
			var previousExecution, previousHash string
			err := tx.QueryRowContext(r.Context(), `SELECT execution_id, payload_hash FROM runner_event_receipts WHERE source_id = ?`, event.ID).Scan(&previousExecution, &previousHash)
			if err == nil {
				if previousExecution != executionID || previousHash != digest {
					WriteError(w, logger, &Error{Code: Conflict, Message: "Event ID already used for another payload"})
					return
				}
				continue
			}
			if errors.Is(err, sql.ErrNoRows) {
				_, err = store.AppendExecutionEvent(r.Context(), tx, executionID, model.EventLog, payload)
				if err == nil {
					_, err = tx.ExecContext(r.Context(), `INSERT INTO runner_event_receipts (source_id, execution_id, payload_hash) VALUES (?, ?, ?)`, event.ID, executionID, digest)
				}
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

// Raw upload avoids loading a finished log into memory or exposing Runner-local
// filesystem paths. The Control Plane chooses storage paths and computes hashes.
func uploadLogArtifact(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, artifacts *store.ArtifactStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		artifactID := r.PathValue("artifactID")
		if err := ValidateID("artifactID", artifactID, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		query, err := parseLogArtifactQuery(r)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		tx := logTransaction(w, r, db, logger, auth)
		if tx == nil {
			return
		}
		defer tx.Rollback()
		if artifacts == nil {
			WriteError(w, logger, &Error{Code: PreconditionFailed, Message: "Artifact storage is not configured"})
			return
		}
		executionID, kind := r.PathValue("id"), query.stream+"_log"
		var previousExecution, previousKind, previousHash string
		var previousSize int64
		var previousRedacted bool
		err = tx.QueryRowContext(r.Context(), `SELECT execution_id, kind, sha256, size_bytes, redacted FROM artifacts WHERE id = ?`, artifactID).
			Scan(&previousExecution, &previousKind, &previousHash, &previousSize, &previousRedacted)
		if err == nil {
			hash := sha256.New()
			size, copyErr := io.Copy(hash, r.Body)
			if copyErr != nil {
				WriteError(w, logger, invalid("body", "read_failed"))
				return
			}
			if previousExecution != executionID || previousKind != kind || previousRedacted != query.redacted || previousSize != size || previousHash != hex.EncodeToString(hash.Sum(nil)) {
				WriteError(w, logger, &Error{Code: Conflict, Message: "Artifact ID already used for another payload"})
				return
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			_, err = artifacts.SaveTx(r.Context(), tx, artifactID, executionID, kind, query.redacted, r.Body)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type logArtifactQuery struct {
	stream   string
	redacted bool
}

func parseLogArtifactQuery(r *http.Request) (logArtifactQuery, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return logArtifactQuery{}, invalid("query", "invalid")
	}
	stream, redacted := query.Get("stream"), query.Get("redacted")
	if len(query) != 2 || len(query["stream"]) != 1 || len(query["redacted"]) != 1 ||
		!logStream(stream) || stream == "runner" || (redacted != "true" && redacted != "false") {
		return logArtifactQuery{}, invalid("query", "invalid")
	}
	return logArtifactQuery{stream, redacted == "true"}, nil
}
