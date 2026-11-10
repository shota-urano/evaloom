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
		before := model.Project{ID: id, ProjectSettings: model.ProjectSettings{
			AllowedOperations: map[string]any{}, Timeouts: map[string]int{},
		}}
		if !create {
			var err error
			before, err = store.GetProject(r.Context(), db, id)
			if err != nil {
				WriteError(w, logger, projectError(err))
				return
			}
		}
		settings, err := decodeSettings(before.ProjectSettings, patch, create, "reviewRole", "linearTeamId", "linearRoutingLabel")
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
		_, err = store.RunnerWorktreeRoot(r.Context(), tx, settings.RunnerID)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: PreconditionFailed, Message: "Runner not found", Details: []Detail{{Field: "runnerId", Reason: "not_found"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// UNIQUE(linear_team_id, linear_routing_label) (spec 15 3.8): name the
		// field so the form can show which value collided.
		if settings.LinearTeamID != nil && settings.LinearRoutingLabel != nil {
			taken, err := store.LinearRouteTaken(r.Context(), tx, *settings.LinearTeamID, *settings.LinearRoutingLabel, id)
			if err == nil && taken {
				err = &Error{Code: Conflict, Message: "Linear Team and routing label already registered to another Project",
					Details: []Detail{{Field: "linearRoutingLabel", Reason: "duplicate"}}}
			}
			if err != nil {
				WriteError(w, logger, err)
				return
			}
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
		if err == nil {
			err = store.PruneLinearPollTeams(r.Context(), tx)
		}
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

// markLinearRecheck puts the recheck mark on the Issues of the held tasks a
// Project update may release (spec 15 3.6.3, the table of setting changes,
// Q81), in the update's transaction. An update that leaves linear_team_id,
// linear_routing_label and default_role unchanged marks nothing.
func markLinearRecheck(ctx context.Context, tx *sql.Tx, before model.ProjectSettings, after model.Project, at string) error {
	routing := !reflect.DeepEqual(before.LinearTeamID, after.LinearTeamID) ||
		!reflect.DeepEqual(before.LinearRoutingLabel, after.LinearRoutingLabel)
	if !routing && before.DefaultRole == after.DefaultRole {
		return nil
	}
	return store.MarkLinearRecheck(ctx, tx, store.LinearRecheck{ProjectID: after.ID, Routing: routing}, at)
}

func validateProjectSettings(p model.ProjectSettings) error {
	for _, field := range []struct{ name, value string }{
		{"name", p.Name}, {"runnerId", p.RunnerID}, {"defaultRole", p.DefaultRole},
	} {
		if strings.TrimSpace(field.value) == "" {
			return invalid(field.name, "required")
		}
	}
	if p.ReviewRole != nil {
		return invalid("reviewRole", "invalid")
	}
	// Clearing uses null; a blank value would never match a Linear Team or
	// label and is rejected rather than stored.
	for _, field := range []struct {
		name  string
		value *string
	}{{"linearTeamId", p.LinearTeamID}, {"linearRoutingLabel", p.LinearRoutingLabel}} {
		if field.value != nil && strings.TrimSpace(*field.value) == "" {
			return invalid(field.name, "required")
		}
	}
	if n, ok := p.Timeouts["harnessSec"]; ok && n <= 0 {
		return invalid("timeouts.harnessSec", "out_of_range")
	}
	for key := range p.Timeouts {
		if key != "harnessSec" {
			return invalid("timeouts", "invalid")
		}
	}
	return nil
}
