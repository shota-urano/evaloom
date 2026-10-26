package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func assignProjectHarness(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, SlugID); err != nil {
			WriteError(w, logger, err)
			return
		}
		var input struct {
			Role      string `json:"role"`
			HarnessID string `json:"harnessId"`
			Version   string `json:"version"`
		}
		err := DecodeJSON(r, &input)
		if err == nil && strings.TrimSpace(input.Role) == "" {
			err = invalid("role", "required")
		}
		if err == nil {
			err = ValidateID("harnessId", input.HarnessID, SlugID)
		}
		if err == nil && !registry.ValidProfileVersion(input.Version) {
			err = &Error{Code: PreconditionFailed, Message: "Profile version must be SemVer", Details: []Detail{{Field: "version", Reason: "invalid_format"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// Resolve the registered version, read the old pin, and save its history
		// while holding the store's serialized transaction. No Git lookup is needed.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		if _, err := store.GetProject(r.Context(), tx, id); err != nil {
			WriteError(w, logger, projectError(err))
			return
		}
		profile, err := store.GetHarnessProfile(r.Context(), tx, input.HarnessID, input.Version)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: PreconditionFailed, Message: "Profile version is not registered for this Harness", Details: []Detail{{Field: "version", Reason: "not_found"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		before, err := store.GetProjectHarness(r.Context(), tx, id, input.Role)
		status := http.StatusOK
		var old any = before
		if errors.Is(err, sql.ErrNoRows) {
			status, old, err = http.StatusCreated, nil, nil
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		after := model.ProjectHarness{
			ProjectID: id, Role: input.Role, HarnessID: profile.HarnessID, ProfileID: profile.ID,
			AssignedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		err = store.SaveProjectHarness(r.Context(), tx, after)
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
				Actor: "owner", Action: "project.assign_harness", TargetType: "project", TargetID: id,
				Detail: map[string]any{"before": old, "after": after},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, status, after)
	}
}
