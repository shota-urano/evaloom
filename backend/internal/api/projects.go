package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func projectError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Project not found"}
	}
	if errors.Is(err, store.ErrProjectConflict) {
		return &Error{Code: Conflict, Message: "Project ID or repository path already registered"}
	}
	return err
}

func listProjects(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		page, err := ParsePage(query, "projects")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		projects, err := store.ListProjects(r.Context(), db, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, NewPage(projects, page.Limit, "projects", func(p model.Project) string { return p.ID }))
	}
}

func getProject(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, SlugID); err != nil {
			WriteError(w, logger, err)
			return
		}
		project, err := store.GetProject(r.Context(), db, id)
		if err != nil {
			WriteError(w, logger, projectError(err))
			return
		}
		writeJSON(w, http.StatusOK, project)
	}
}

func writeProject(db *sql.DB, logger *slog.Logger, create bool) http.HandlerFunc {
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
		// Read the current configuration, validate the merged update, and write
		// the audit row under one transaction. Concurrent PATCH requests cannot
		// silently overwrite changes made after reading their initial values.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		before := model.Project{ID: id, ProjectSettings: model.ProjectSettings{
			ContextLocation: "AGENTS.md", CopyAllowlist: []string{}, TestFilePatterns: []string{},
			AllowedOperations: map[string]any{}, ApprovalPolicy: map[string]any{}, Timeouts: map[string]int{},
		}}
		if !create {
			before, err = store.GetProject(r.Context(), tx, id)
			if err != nil {
				WriteError(w, logger, projectError(err))
				return
			}
		}
		settings, err := decodeProjectSettings(before.ProjectSettings, patch, create)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		warnings, err := validateProjectSettings(settings)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		root, err := store.RunnerWorktreeRoot(r.Context(), tx, settings.RunnerID)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: PreconditionFailed, Message: "Runner not found", Details: []Detail{{Field: "runnerId", Reason: "not_found"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		settings.LocalRepoPath, err = registry.ValidateRepository(r.Context(), registry.RepositoryInput{
			Path: settings.LocalRepoPath, URL: settings.GitRemoteURL, BaseBranch: settings.BaseBranch, WorktreeRoot: root,
		})
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// Symlink resolution preserves spelling on case-insensitive volumes.
		// Also compare directory identities while holding the write transaction.
		paths, err := store.ProjectRepositoryPaths(r.Context(), tx, id)
		if err == nil {
			var info os.FileInfo
			info, err = os.Stat(settings.LocalRepoPath)
			if err == nil {
				for _, other := range paths {
					if otherInfo, statErr := os.Stat(other); statErr == nil && os.SameFile(info, otherInfo) {
						err = store.ErrProjectConflict
						break
					}
				}
			}
		}
		if err != nil {
			WriteError(w, logger, projectError(err))
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		after := model.Project{ID: id, ProjectSettings: settings, CreatedAt: before.CreatedAt, UpdatedAt: now}
		action, status := "project.update", http.StatusOK
		var old any = auditProjectSettings(before.ProjectSettings)
		if create {
			after.CreatedAt = now
			action, status, old = "project.register", http.StatusCreated, nil
		}
		err = store.SaveProject(r.Context(), tx, after, create)
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
				Actor: "owner", Action: action, TargetType: "project", TargetID: id,
				Detail: map[string]any{"before": old, "after": auditProjectSettings(after.ProjectSettings)},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, projectError(err))
			return
		}
		writeJSON(w, status, struct {
			model.Project
			Warnings []string `json:"warnings"`
		}{after, warnings})
	}
}

// PATCH replaces supplied top-level values (including objects and arrays).
// Nullable columns can be cleared with null; ID and timestamps are immutable.
func decodeProjectSettings(before model.ProjectSettings, patch map[string]json.RawMessage, create bool) (model.ProjectSettings, error) {
	encoded, err := json.Marshal(before)
	if err != nil {
		return before, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return before, err
	}
	for field, value := range patch {
		if field == "id" && create {
			continue
		}
		if _, known := merged[field]; !known {
			return before, invalid("body", "invalid_json")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			switch field {
			case "reviewRole", "testCommand", "testReportFormat", "testReportPath", "setupCommand":
			default:
				return before, invalid(field, "required")
			}
		}
		merged[field] = value
	}
	encoded, err = json.Marshal(merged)
	if err != nil {
		return before, err
	}
	var settings model.ProjectSettings
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&settings); err != nil {
		return before, invalid("body", "invalid_json")
	}
	return settings, nil
}

func validateProjectSettings(p model.ProjectSettings) ([]string, error) {
	for _, field := range []struct{ name, value string }{
		{"name", p.Name}, {"localRepoPath", p.LocalRepoPath}, {"gitRemoteUrl", p.GitRemoteURL},
		{"runnerId", p.RunnerID}, {"baseBranch", p.BaseBranch}, {"defaultRole", p.DefaultRole},
	} {
		if strings.TrimSpace(field.value) == "" {
			return nil, invalid(field.name, "required")
		}
	}
	if p.ContextLocation != "AGENTS.md" {
		return nil, invalid("contextLocation", "invalid_format")
	}
	if p.ReviewRole != nil {
		return nil, invalid("reviewRole", "invalid")
	}
	for _, seconds := range []string{"setupSec", "harnessSec", "testSec"} {
		if n, ok := p.Timeouts[seconds]; ok && n <= 0 {
			return nil, invalid("timeouts."+seconds, "out_of_range")
		}
	}
	for key := range p.Timeouts {
		if key != "setupSec" && key != "harnessSec" && key != "testSec" {
			return nil, invalid("timeouts", "invalid")
		}
	}
	warnings := []string{}
	for _, name := range p.CopyAllowlist {
		if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, "*?[]\\\x00") {
			return nil, invalid("copyAllowlist", "invalid_format")
		}
		for _, segment := range strings.Split(name, "/") {
			if segment == ".." || segment == "." || segment == "" {
				return nil, invalid("copyAllowlist", "invalid_format")
			}
		}
		base := path.Base(name)
		if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasPrefix(base, "id_") {
			warnings = append(warnings, name+" may contain secrets; use test-only dummy values")
		}
	}
	for _, pattern := range p.TestFilePatterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, invalid("testFilePatterns", "invalid_format")
		}
	}
	return warnings, nil
}

// Credentials in a remote URL are not configuration history. Retain the
// repository identity while omitting URL userinfo, query, and fragment.
func auditProjectSettings(p model.ProjectSettings) model.ProjectSettings {
	if remote, err := url.Parse(p.GitRemoteURL); err == nil && remote.Scheme != "" && remote.Host != "" {
		remote.User, remote.RawQuery, remote.Fragment = nil, "", ""
		p.GitRemoteURL = remote.String()
	}
	return p
}
