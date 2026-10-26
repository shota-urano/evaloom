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
		before := model.Harness{ID: id, HarnessSettings: model.HarnessSettings{
			Specialty: "developer", SupportedTasks: []string{"development"}, ContractVersion: "1",
			SupportedModels: []string{}, AvailableTools: []string{}, ToolPermissions: map[string]any{},
		}}
		if !create {
			var err error
			before, err = store.GetHarness(r.Context(), db, id)
			if err != nil {
				WriteError(w, logger, harnessError(err))
				return
			}
		}
		settings, err := decodeSettings(before.HarnessSettings, patch, create)
		if err == nil {
			err = validateHarnessSettings(settings, adapters)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		roots, err := store.RunnerWorktreeRoots(r.Context(), db)
		if err == nil && len(roots) == 0 {
			err = &Error{Code: PreconditionFailed, Message: "No Runner Worktree Root registered", Details: []Detail{{Field: "worktreeRoot", Reason: "not_found"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// Reuse the existing deadline-bound Git checks outside the transaction.
		// Harnesses have no Base Branch and are shared across registered Runners.
		for _, root := range roots {
			settings.RepoPath, err = registry.ValidateRepository(r.Context(), registry.RepositoryInput{
				Path: settings.RepoPath, URL: settings.RepoURL, WorktreeRoot: root,
			})
			if err != nil {
				WriteError(w, logger, err)
				return
			}
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
		currentRoots, err := store.RunnerWorktreeRoots(r.Context(), tx)
		if err == nil && !reflect.DeepEqual(currentRoots, roots) {
			err = &Error{Code: Conflict, Message: "Runner Worktree Roots changed during repository validation; retry the request"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
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
		now := time.Now().UTC().Format(time.RFC3339Nano)
		after := model.Harness{ID: id, HarnessSettings: settings, CreatedAt: before.CreatedAt, UpdatedAt: now}
		action, status := "harness.update", http.StatusOK
		var old any = auditHarnessSettings(before.HarnessSettings)
		if create {
			after.CreatedAt = now
			action, status, old = "harness.register", http.StatusCreated, nil
		}
		err = store.SaveHarness(r.Context(), tx, after, create)
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

func validateHarnessSettings(h model.HarnessSettings, adapters *adapter.Registry) error {
	for _, field := range []struct{ name, value string }{
		{"name", h.Name}, {"adapter", h.Adapter}, {"repoPath", h.RepoPath}, {"repoUrl", h.RepoURL},
	} {
		if strings.TrimSpace(field.value) == "" {
			return invalid(field.name, "required")
		}
	}
	if _, err := adapters.Lookup(h.Adapter); err != nil {
		return invalid("adapter", "invalid")
	}
	if h.Specialty != "developer" {
		return invalid("specialty", "invalid")
	}
	if len(h.SupportedTasks) != 1 || h.SupportedTasks[0] != "development" {
		return invalid("supportedTasks", "invalid")
	}
	if h.ContractVersion != "1" {
		return invalid("contractVersion", "invalid")
	}
	for _, field := range []struct {
		name   string
		values []string
	}{{"supportedModels", h.SupportedModels}, {"availableTools", h.AvailableTools}} {
		for _, value := range field.values {
			if strings.TrimSpace(value) == "" {
				return invalid(field.name, "invalid_format")
			}
		}
	}
	return nil
}

func auditHarnessSettings(h model.HarnessSettings) model.HarnessSettings {
	h.RepoURL = auditRepositoryURL(h.RepoURL)
	return h
}
