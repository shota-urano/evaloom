package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
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

// projectView is a Project as read by the owner, with each role's pinned
// Harness Profile (management UI: Projects list and detail).
type projectView struct {
	model.Project
	Harnesses []model.ProjectHarnessView `json:"harnesses"`
}

func withHarnesses(ctx context.Context, db *sql.DB, projects []model.Project) ([]projectView, error) {
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	assignments, err := store.ListProjectHarnesses(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	views := make([]projectView, len(projects))
	for i, p := range projects {
		views[i] = projectView{Project: p, Harnesses: []model.ProjectHarnessView{}}
		for _, a := range assignments {
			if a.ProjectID == p.ID {
				views[i].Harnesses = append(views[i].Harnesses, a)
			}
		}
	}
	return views, nil
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
		views, err := withHarnesses(r.Context(), db, projects)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, NewPage(views, page.Limit, "projects", func(p projectView) string { return p.ID }))
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
		views, err := withHarnesses(r.Context(), db, []model.Project{project})
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, views[0])
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
		before := model.Project{ID: id, ProjectSettings: model.ProjectSettings{
			ContextLocation: "AGENTS.md", CopyAllowlist: []string{},
			AllowedOperations: map[string]any{}, ApprovalPolicy: map[string]any{}, Timeouts: map[string]int{},
		}}
		if !create {
			var err error
			before, err = store.GetProject(r.Context(), db, id)
			if err != nil {
				WriteError(w, logger, projectError(err))
				return
			}
		}
		settings, err := decodeSettings(before.ProjectSettings, patch, create, "reviewRole", "testCommand", "testReportFormat", "testReportPath", "setupCommand")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		warnings, err := validateProjectSettings(settings)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		root, err := store.RunnerWorktreeRoot(r.Context(), db, settings.RunnerID)
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
		// Remote Git validation must not hold the store's only DB connection.
		// Recheck the values it depended on inside the saving transaction so a
		// concurrent update cannot be overwritten with the stale merged settings.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		if !create {
			current, err := store.GetProject(r.Context(), tx, id)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !reflect.DeepEqual(current, before)) {
				err = &Error{Code: Conflict, Message: "Project changed during repository validation; retry the request"}
			}
			if err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		currentRoot, err := store.RunnerWorktreeRoot(r.Context(), tx, settings.RunnerID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && currentRoot != root) {
			err = &Error{Code: Conflict, Message: "Runner changed during repository validation; retry the request"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// Symlink resolution preserves spelling on case-insensitive volumes.
		// Also compare directory identities while holding the write transaction.
		paths, err := store.ProjectRepositoryPaths(r.Context(), tx, id)
		if err == nil {
			var duplicate bool
			duplicate, err = sameRepository(settings.LocalRepoPath, paths)
			if duplicate {
				err = store.ErrProjectConflict
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
		if name == "" || filepath.IsAbs(name) || strings.Contains(name, "..") || strings.ContainsAny(name, "*?[]\\\x00") {
			return nil, invalid("copyAllowlist", "invalid_format")
		}
		for _, segment := range strings.Split(name, "/") {
			if segment == "." || segment == "" {
				return nil, invalid("copyAllowlist", "invalid_format")
			}
		}
		base := path.Base(name)
		if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasPrefix(base, "id_") {
			warnings = append(warnings, name+" may contain secrets; use test-only dummy values")
		}
	}
	return warnings, nil
}

// Credentials in a remote URL are not configuration history. Retain the
// repository identity while omitting URL userinfo, query, and fragment.
func auditProjectSettings(p model.ProjectSettings) model.ProjectSettings {
	p.GitRemoteURL = auditRepositoryURL(p.GitRemoteURL)
	return p
}
