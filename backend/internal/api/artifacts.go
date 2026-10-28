package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
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

func artifactKind(kind string) bool {
	switch kind {
	case "harness_log", "test_log", "setup_log", "diff", "test_report", "test_ids", "harness_output":
		return true
	}
	return false
}

func registerArtifact(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, config ArtifactConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		for _, field := range []string{"artifactId", "kind", "redacted"} {
			if len(query[field]) != 1 {
				WriteError(w, logger, invalid(field, "required_once"))
				return
			}
		}
		if err := ValidateID("artifactId", query.Get("artifactId"), ULID); err != nil {
			WriteError(w, logger, err)
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
		tx := logTransaction(w, r, db, logger, auth)
		if tx == nil {
			return
		}
		defer tx.Rollback()
		if config.Store == nil || config.MaxBytes <= 0 {
			WriteError(w, logger, fmt.Errorf("artifact storage and body limit must be configured"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, config.MaxBytes)
		_, cleanup, err := config.Store.RegisterTx(r.Context(), tx, query.Get("artifactId"), r.PathValue("id"), query.Get("kind"), query.Get("redacted") == "1", r.Body)
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
