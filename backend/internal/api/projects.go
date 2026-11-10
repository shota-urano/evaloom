package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func projectError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Project not found"}
	}
	if errors.Is(err, store.ErrProjectConflict) {
		return &Error{Code: Conflict, Message: "Project ID already registered"}
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
		before := model.Project{ID: id, ProjectSettings: model.ProjectSettings{Params: json.RawMessage(`{}`), ApprovalWaivers: []string{}}}
		if !create {
			var err error
			before, err = store.GetProject(r.Context(), db, id)
			if err != nil {
				WriteError(w, logger, projectError(err))
				return
			}
		}
		settings, err := decodeSettings(before.ProjectSettings, patch, create)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		if err := validateProjectSettings(settings); err != nil {
			WriteError(w, logger, err)
			return
		}
		// Recheck the values the merge depended on inside the saving
		// transaction so a concurrent update cannot be overwritten with the
		// stale merged settings.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		if !create {
			current, err := store.GetProject(r.Context(), tx, id)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !reflect.DeepEqual(current, before)) {
				err = &Error{Code: Conflict, Message: "Project changed during the update; retry the request"}
			}
			if err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		// The Runner's existence is checked in the saving transaction.
		err = store.RunnerExists(r.Context(), tx, settings.RunnerID)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: PreconditionFailed, Message: "Runner not found", Details: []Detail{{Field: "runnerId", Reason: "not_found"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		if err := validateProjectProfile(r.Context(), tx, settings); err != nil {
			WriteError(w, logger, err)
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		after := model.Project{ID: id, ProjectSettings: settings, CreatedAt: before.CreatedAt, UpdatedAt: now}
		action, status := "project.update", http.StatusOK
		var old any = before.ProjectSettings
		if create {
			after.CreatedAt = now
			action, status, old = "project.register", http.StatusCreated, nil
		}
		err = store.SaveProject(r.Context(), tx, after, create)
		if err == nil && !create {
			err = markLinearRecheck(r.Context(), tx, before.ProjectSettings, after, now)
		}
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
				Actor: "owner", Action: action, TargetType: "project", TargetID: id,
				Detail: map[string]any{"before": old, "after": after.ProjectSettings},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, projectError(err))
			return
		}
		writeJSON(w, status, after)
	}
}

// markLinearRecheck keeps existing held tasks eligible for reassessment when
// their direct Profile pin changes. Routing is managed separately in routes.
func markLinearRecheck(ctx context.Context, tx *sql.Tx, before model.ProjectSettings, after model.Project, at string) error {
	if before.HarnessID == after.HarnessID && before.ProfileID == after.ProfileID {
		return nil
	}
	return store.MarkLinearRecheck(ctx, tx, store.LinearRecheck{ProjectID: after.ID}, at)
}

func validateProjectSettings(p model.ProjectSettings) error {
	for _, field := range []struct{ name, value string }{{"name", p.Name}, {"runnerId", p.RunnerID}, {"harnessId", p.HarnessID}, {"profileId", p.ProfileID}} {
		if strings.TrimSpace(field.value) == "" {
			return invalid(field.name, "required")
		}
	}
	if p.ApprovalWaivers == nil {
		return invalid("approvalWaivers", "invalid_format")
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(p.Params, &params) != nil || params == nil {
		return invalid("params", "invalid_format")
	}
	return nil
}

func validateProjectProfile(ctx context.Context, tx *sql.Tx, settings model.ProjectSettings) error {
	profile, err := store.GetHarnessProfileByID(ctx, tx, settings.ProfileID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && profile.HarnessID != settings.HarnessID) {
		return &Error{Code: PreconditionFailed, Message: "Profile does not belong to this Harness", Details: []Detail{{Field: "profileId", Reason: "invalid"}}}
	}
	if err != nil {
		return err
	}
	return validateProfileWaivers(profile.Definition, settings.ApprovalWaivers)
}

// Only waivable operations explicitly declared by the pinned Profile may be
// skipped. Full Profile/params schema validation belongs to the contract task.
func validateProfileWaivers(definition json.RawMessage, waivers []string) error {
	var def struct {
		Approvals []struct {
			Operation string `json:"operation"`
			Waivable  bool   `json:"waivable"`
		} `json:"approvals"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, a := range def.Approvals {
		allowed[a.Operation] = a.Waivable
	}
	seen := map[string]bool{}
	for _, w := range waivers {
		if !allowed[w] || seen[w] {
			return &Error{Code: PreconditionFailed, Message: "Operation is not waivable in this Profile", Details: []Detail{{Field: "approvalWaivers", Reason: "invalid"}}}
		}
		seen[w] = true
	}
	return nil
}
