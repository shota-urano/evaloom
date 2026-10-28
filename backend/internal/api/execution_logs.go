package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

type executionLogsView struct {
	Harness string `json:"harness"`
	Setup   string `json:"setup"`
	Test    string `json:"test"`
	Runner  string `json:"runner"`
}

func executionLogs(db *sql.DB, logger *slog.Logger, config ArtifactConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		err := store.CheckExecutionEventCursor(r.Context(), db, id, nil)
		if errors.Is(err, store.ErrExecutionNotFound) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		artifacts, err := store.ListLogArtifacts(r.Context(), db, id)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		var harness, setup, test strings.Builder
		for _, artifact := range artifacts {
			if config.Store == nil {
				WriteError(w, logger, fmt.Errorf("artifact storage must be configured"))
				return
			}
			_, file, err := config.Store.Open(r.Context(), id, artifact.ID)
			if err != nil {
				WriteError(w, logger, err)
				return
			}
			destination := map[string]*strings.Builder{
				"harness_log": &harness, "setup_log": &setup, "test_log": &test,
			}[artifact.Kind]
			_, readErr := io.Copy(destination, file)
			if err := errors.Join(readErr, file.Close()); err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		runner, err := store.RunnerLog(r.Context(), db, id)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// Commit the response only after all files and events were read, so a
		// storage failure uses the common JSON error rather than partial logs.
		writeJSON(w, http.StatusOK, executionLogsView{
			Harness: harness.String(), Setup: setup.String(), Test: test.String(), Runner: runner,
		})
	}
}
