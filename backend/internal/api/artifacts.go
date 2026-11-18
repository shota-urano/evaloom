package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// ArtifactConfig has no implicit storage location or size policy. Both belong
// to the Control Plane configuration, never to the Runner's request.
type ArtifactConfig struct {
	Store    *store.ArtifactStore
	MaxBytes int64
}

func getArtifact(logger *slog.Logger, config ArtifactConfig) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		for _, field := range []string{"id", "artifactId"} {
			if err := ValidateID(field, r.PathValue(field), ULID); err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		if config.Store == nil {
			WriteError(w, logger, fmt.Errorf("artifact storage must be configured"))
			return
		}
		artifact, file, err := config.Store.Open(r.Context(), r.PathValue("id"), r.PathValue("artifactId"))
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Artifact not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.SizeBytes, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		if _, err := io.Copy(w, file); err != nil {
			// Headers are committed: do not append a JSON error to the file.
			logger.Error("Artifact response failed", "artifactId", artifact.ID, "error", err)
		}
	}
}

// getPinnedRecord serves GET /api/executions/{id}/records/{artifactId} (spec 07
// 5, Q208). Only the owning Runner may read, and only records pinned for this
// Execution are served; any other artifact, including another Harness's
// record, is 404. A pinned record whose file is gone is also 404, so the
// Runner skips that record and continues (spec 07 6).
func getPinnedRecord(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, config ArtifactConfig) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		for _, field := range []string{"id", "artifactId"} {
			if err := ValidateID(field, r.PathValue(field), ULID); err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		var owner sql.NullString
		err := db.QueryRowContext(r.Context(), `SELECT runner_id FROM executions WHERE id = ?`, r.PathValue("id")).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		if runner == "" || !owner.Valid || owner.String != runner {
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot read this Execution's records")
			return
		}
		if config.Store == nil {
			WriteError(w, logger, fmt.Errorf("artifact storage must be configured"))
			return
		}
		artifact, file, err := config.Store.OpenPinnedRecord(r.Context(), r.PathValue("id"), r.PathValue("artifactId"))
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Record not found"}
		} else if errors.Is(err, fs.ErrNotExist) {
			logger.Warn("Pinned record file is missing", "executionId", r.PathValue("id"), "artifactId", r.PathValue("artifactId"))
			err = &Error{Code: NotFound, Message: "Record file not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.SizeBytes, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, file); err != nil {
			logger.Error("Record response failed", "artifactId", artifact.ID, "error", err)
		}
	}
}

func artifactKind(kind string) bool {
	switch kind {
	case "log", "out_file", "request_json", "result_json", "work_summary", "question", "proposal_result":
		return true
	}
	return false
}

func registerArtifact(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, config ArtifactConfig, isProposal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		for _, field := range []string{"artifactId", "stage", "kind", "name", "redacted"} {
			if len(query[field]) != 1 {
				WriteError(w, logger, invalid(field, "required_once"))
				return
			}
		}
		if err := ValidateID("artifactId", query.Get("artifactId"), ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		switch query.Get("stage") {
		case "prepare", "work", "check", "publish", "runner", "proposal":
		default:
			WriteError(w, logger, invalid("stage", "invalid"))
			return
		}
		if query.Get("name") == "" {
			WriteError(w, logger, invalid("name", "required"))
			return
		}
		if !artifactKind(query.Get("kind")) {
			WriteError(w, logger, invalid("kind", "invalid"))
			return
		}
		if query.Get("redacted") != "0" && query.Get("redacted") != "1" {
			WriteError(w, logger, invalid("redacted", "invalid"))
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/octet-stream" {
			WriteError(w, logger, invalid("Content-Type", "invalid"))
			return
		}
		// As for events, even a retry of a finished Execution needs ownership.
		if isProposal && (query.Get("stage") != "proposal" || (query.Get("kind") != "log" && query.Get("kind") != "proposal_result")) {
			WriteError(w, logger, invalid("kind", "invalid_proposal_artifact"))
			return
		}
		tx := reportTransaction(w, r, db, logger, auth, isProposal)
		if tx == nil {
			return
		}
		defer tx.Rollback()
		if config.Store == nil || config.MaxBytes <= 0 {
			WriteError(w, logger, fmt.Errorf("artifact storage and body limit must be configured"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, config.MaxBytes)
		artifact := store.Artifact{ID: query.Get("artifactId"), ExecutionID: r.PathValue("id"), Stage: query.Get("stage"), Kind: query.Get("kind"), Name: query.Get("name"), Redacted: query.Get("redacted") == "1"}
		if isProposal {
			artifact.RouteProposalID, artifact.ExecutionID = artifact.ExecutionID, ""
		}
		_, cleanup, err := config.Store.RegisterTx(r.Context(), tx, artifact, r.Body)
		if err == nil {
			err = tx.Commit()
			if err != nil {
				err = errors.Join(err, cleanup())
			}
		}
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			err = invalid("body", "too_large")
		} else if errors.Is(err, store.ErrArtifactConflict) {
			err = &Error{Code: Conflict, Message: "Artifact ID already used for another Execution or content"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
