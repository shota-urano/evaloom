package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func harnessError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Harness not found"}
	}
	if errors.Is(err, store.ErrHarnessConflict) {
		return &Error{Code: Conflict, Message: "Harness ID or repository path already registered"}
	}
	return err
}

func getHarness(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, SlugID); err != nil {
			WriteError(w, logger, err)
			return
		}
		harness, err := store.GetHarness(r.Context(), db, id)
		if err != nil {
			WriteError(w, logger, harnessError(err))
			return
		}
		writeJSON(w, http.StatusOK, harness)
	}
}

func listHarnesses(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		page, err := ParsePage(query, "harnesses")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		harnesses, err := store.ListHarnesses(r.Context(), db, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, NewPage(harnesses, page.Limit, "harnesses", func(h model.Harness) string { return h.ID }))
	}
}

func writeHarness(db *sql.DB, logger *slog.Logger, adapters *adapter.Registry, create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]json.RawMessage
		if err := DecodeJSON(r, &patch); err != nil {
			WriteError(w, logger, err)
			return
		}
		id := r.PathValue("id")
		if create {
			if err := json.Unmarshal(patch["id"], &id); err != nil {
				WriteError(w, logger, invalid("id", "required"))
				return
			}
		}
		if err := ValidateID("id", id, SlugID); err != nil {
			WriteError(w, logger, err)
			return
		}
		before := model.Harness{ID: id, HarnessSettings: model.HarnessSettings{ApprovalWaivers: []string{}}}
		if !create {
			var err error
			before, err = store.GetHarness(r.Context(), db, id)
			if err != nil {
				WriteError(w, logger, harnessError(err))
				return
			}
		}
		settings, err := decodeSettings(before.HarnessSettings, patch, create, "defaultProfileId")
		if shapeErr := waiverListShape(patch); err != nil && shapeErr != nil {
			err = shapeErr
		}
		if err == nil {
			err = validateHarnessSettings(settings, adapters)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		settings.RepoPath, err = registry.ValidateRepository(r.Context(), registry.RepositoryInput{
			Path: settings.RepoPath, SkipRemote: true,
		})
		if err != nil {
			WriteError(w, logger, repoPathError(err))
			return
		}

		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		if !create {
			current, err := store.GetHarness(r.Context(), tx, id)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !reflect.DeepEqual(current, before)) {
				err = &Error{Code: Conflict, Message: "Harness changed during repository validation; retry the request"}
			}
			if err != nil {
				WriteError(w, logger, err)
				return
			}
		}

		paths, err := store.HarnessRepositoryPaths(r.Context(), tx, id)
		if err == nil {
			var duplicate bool
			duplicate, err = sameRepository(settings.RepoPath, paths)
			if duplicate {
				err = store.ErrHarnessConflict
			}
		}
		if err != nil {
			WriteError(w, logger, harnessError(err))
			return
		}
		if settings.DefaultProfileID != nil {
			profile, profileErr := store.GetHarnessProfileByID(r.Context(), tx, *settings.DefaultProfileID)
			if errors.Is(profileErr, sql.ErrNoRows) || (profileErr == nil && profile.HarnessID != id) {
				profileErr = &Error{Code: PreconditionFailed, Message: "Default Profile does not belong to this Harness", Details: []Detail{{Field: "defaultProfileId", Reason: "invalid"}}}
			}
			if profileErr == nil {
				profileErr = validateProfileWaivers(profile.Definition, settings.ApprovalWaivers)
			}
			if profileErr != nil {
				WriteError(w, logger, profileErr)
				return
			}
		} else if len(settings.ApprovalWaivers) != 0 {
			WriteError(w, logger, invalid("approvalWaivers", "invalid"))
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		after := model.Harness{ID: id, HarnessSettings: settings, CreatedAt: before.CreatedAt, UpdatedAt: now}
		action, status := "harness.update", http.StatusOK
		var old any = auditHarnessSettings(before.HarnessSettings)
		if create {
			after.CreatedAt = now
			action, status, old = "harness.register", http.StatusCreated, nil
		}
		err = store.SaveHarness(r.Context(), tx, after, create)
		if err == nil && !create && defaultProfileSet(before.DefaultProfileID, after.DefaultProfileID) {
			// spec 03 3.3 / 15 3.6.3: setting the default version asks for the
			// held direct-routed tasks to be decided again on the next poll.
			err = store.MarkLinearRecheck(r.Context(), tx, store.LinearRecheck{HarnessID: id}, now)
		}
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
				Actor: "owner", Action: action, TargetType: "harness", TargetID: id,
				Detail: map[string]any{"before": old, "after": auditHarnessSettings(after.HarnessSettings)},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, harnessError(err))
			return
		}
		writeJSON(w, status, after)
	}
}

func validateHarnessSettings(h model.HarnessSettings, _ *adapter.Registry) error {
	for _, field := range []struct{ name, value string }{{"name", h.Name}, {"repoPath", h.RepoPath}, {"repoUrl", h.RepoURL}} {
		if strings.TrimSpace(field.value) == "" {
			return invalid(field.name, "required")
		}
	}
	if utf8.RuneCountInString(h.Name) > maxProjectNameLength { // spec 03 3.3: same limit as Project
		return invalid("name", "too_long")
	}
	if !strings.HasPrefix(h.RepoURL, "https://") {
		return invalid("repoUrl", "invalid_format")
	}
	return validateWaiverList(h.ApprovalWaivers)
}

// defaultProfileSet reports a PATCH that sets defaultProfileId to a new
// version. Clearing it or keeping the same value releases no held task.
func defaultProfileSet(before, after *string) bool {
	return after != nil && (before == nil || *before != *after)
}

// repoPathError names the request field in the repository check's details.
func repoPathError(err error) error {
	var precondition *registry.PreconditionError
	if !errors.As(err, &precondition) {
		return err
	}
	public := &Error{Code: PreconditionFailed, Message: precondition.Message}
	for _, detail := range precondition.Details {
		public.Details = append(public.Details, Detail{Field: "repoPath", Reason: detail.Reason})
	}
	return public
}

func auditHarnessSettings(h model.HarnessSettings) model.HarnessSettings {
	h.RepoURL = auditRepositoryURL(h.RepoURL)
	return h
}
