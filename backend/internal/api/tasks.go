package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

var shortNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

func createTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" {
			WriteError(w, logger, invalid("Idempotency-Key", "required"))
			return
		}
		var input struct {
			ProjectID          string  `json:"projectId"`
			HarnessID          string  `json:"harnessId"`
			Title              string  `json:"title"`
			Body               string  `json:"body"`
			AcceptanceCriteria string  `json:"acceptanceCriteria"`
			ShortName          *string `json:"shortName"`
		}
		err := DecodeJSON(r, &input)
		if err == nil {
			// Exactly one routing target (spec 04 3.1); absent and "" are the
			// same. The reason matches the routes API's one-destination rule.
			switch {
			case (input.ProjectID == "") == (input.HarnessID == ""):
				err = invalid("body", "exactly_one_destination")
			case input.ProjectID != "":
				err = ValidateID("projectId", input.ProjectID, SlugID)
			default:
				err = ValidateID("harnessId", input.HarnessID, SlugID)
			}
		}
		if err == nil {
			for _, field := range []struct{ name, value string }{
				{"title", input.Title}, {"body", input.Body},
			} {
				if strings.TrimSpace(field.value) == "" {
					err = invalid(field.name, "required")
					break
				}
			}
		}
		shortName := "task"
		if input.ShortName != nil {
			shortName = *input.ShortName
		}
		if err == nil && !shortNamePattern.MatchString(shortName) {
			err = invalid("shortName", "invalid_format")
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		// The single store connection serializes this lookup with creation, so
		// simultaneous retries see the committed original before inserting.
		existing, err := store.GetTaskByIdempotencyKey(ctx, tx, keys[0])
		if err == nil {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			WriteError(w, logger, err)
			return
		}
		profileID, err := manualTaskProfileID(ctx, tx, input.ProjectID, input.HarnessID)
		if err == nil {
			err = checkManualAcceptanceCriteria(ctx, tx, profileID, input.AcceptanceCriteria)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		now := time.Now().UTC()
		executionID, err := ulid.New(ulid.Timestamp(now), rand.Reader)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		created := model.Task{
			ProjectID: input.ProjectID, HarnessID: input.HarnessID, Title: input.Title, Body: input.Body,
			AcceptanceCriteria: input.AcceptanceCriteria, ShortName: shortName,
			TaskType: "development", CollaborationMode: "solo", Status: model.TaskReceived,
			IdempotencyKey: keys[0], CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
		}
		err = store.InsertTask(ctx, tx, &created)
		if err == nil {
			err = store.InsertInitialExecution(ctx, tx, executionID.String(), created.ID, created.CreatedAt)
		}
		if err == nil {
			err = task.QueueExecutionTx(ctx, tx, created.ID, executionID.String(), model.TaskReceived)
		}
		if err == nil {
			// Read the timestamps and state actually saved by the transition.
			created, err = store.GetTaskByIdempotencyKey(ctx, tx, keys[0])
		}
		if err == nil {
			_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
				Actor: "owner", Action: "task.create", TargetType: "task", TargetID: created.ID,
				Detail: manualTaskAuditDetail(created, executionID.String()),
			})
		}
		if err == nil {
			_, err = store.AppendExecutionEvent(ctx, tx, executionID.String(), model.EventStatusChanged,
				json.RawMessage(`{"from":"received","to":"queued","reason":"task_created"}`))
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	}
}

// manualTaskProfileID resolves the routing target's Profile (spec 04 3.1):
// projects.profile_id, or harnesses.default_profile_id for a direct routing.
// An unregistered target or a harness without a default version is 422.
func manualTaskProfileID(ctx context.Context, tx *sql.Tx, projectID, harnessID string) (string, error) {
	if projectID != "" {
		project, err := store.GetProject(ctx, tx, projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", routePrecondition("projectId", "not_found", "Project is not registered")
		}
		return project.ProfileID, err
	}
	harness, err := store.GetHarness(ctx, tx, harnessID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", routePrecondition("harnessId", "not_found", "Harness is not registered")
	}
	if err != nil {
		return "", err
	}
	if harness.DefaultProfileID == nil || *harness.DefaultProfileID == "" {
		return "", routePrecondition("harnessId", "default_profile_missing", "Harness has no default Profile version")
	}
	return *harness.DefaultProfileID, nil
}

func routePrecondition(field, reason, message string) error {
	return &Error{Code: PreconditionFailed, Message: message, Details: []Detail{{Field: field, Reason: reason}}}
}

// checkManualAcceptanceCriteria rejects blank acceptance criteria only when
// the Profile declares acceptanceCriteria: required (spec 04 3.1, contract 5.2).
func checkManualAcceptanceCriteria(ctx context.Context, tx *sql.Tx, profileID, criteria string) error {
	if strings.TrimSpace(criteria) != "" {
		return nil
	}
	profile, err := store.GetHarnessProfileByID(ctx, tx, profileID)
	if err != nil {
		return err
	}
	var definition struct {
		AcceptanceCriteria string `json:"acceptanceCriteria"`
	}
	if err := json.Unmarshal(profile.Definition, &definition); err != nil {
		return err
	}
	if definition.AcceptanceCriteria == "required" {
		return invalid("acceptanceCriteria", "required")
	}
	return nil
}

func manualTaskAuditDetail(t model.Task, executionID string) map[string]any {
	detail := map[string]any{"executionId": executionID, "from": model.TaskReceived, "to": model.TaskQueued}
	if t.ProjectID != "" {
		detail["projectId"] = t.ProjectID
	} else {
		detail["harnessId"] = t.HarnessID
	}
	return detail
}

type executionSummaryView struct {
	ID         string  `json:"id"`
	Seq        int     `json:"seq"`
	Mode       string  `json:"mode"`
	Status     string  `json:"status"`
	ErrorClass *string `json:"errorClass"`
}

type taskSummaryView struct {
	model.Task
	// Source, IntakeBlockCause and Linear are spec 15 5章's additions.
	Source           string                `json:"source"`
	IntakeBlockCause *string               `json:"intakeBlockCause"`
	Linear           *taskLinearView       `json:"linear"`
	LatestExecution  *executionSummaryView `json:"latestExecution"`
}

// taskLinearView is the linked Linear Issue; null for manual tasks.
type taskLinearView struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

func newTaskSummaryView(t model.Task, origin store.TaskOrigin) taskSummaryView {
	view := taskSummaryView{Task: t, Source: origin.Source, IntakeBlockCause: origin.IntakeBlockCause}
	if origin.LinearIdentifier != nil && origin.LinearURL != nil {
		view.Linear = &taskLinearView{Identifier: *origin.LinearIdentifier, URL: *origin.LinearURL}
	}
	return view
}

// listTasks serves GET /api/tasks (13 3.2 Tasks 一覧). Optional filters are
// ?status= (a task status from data-model 5.1) and ?projectId=; both match
// exactly. Results are newest first with the shared limit/cursor paging.
func listTasks(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		page, err := ParsePage(query, "tasks")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		var filter store.TaskFilter
		for _, param := range []struct {
			name  string
			value *string
			check func(string) error
		}{
			{"status", &filter.Status, func(v string) error {
				if model.TaskStatus(v).Validate() != nil {
					return invalid("status", "invalid")
				}
				return nil
			}},
			{"projectId", &filter.ProjectID, func(v string) error { return ValidateID("projectId", v, SlugID) }},
		} {
			values, ok := query[param.name]
			if !ok || (len(values) == 1 && values[0] == "") {
				continue
			}
			if len(values) != 1 {
				WriteError(w, logger, invalid(param.name, "invalid"))
				return
			}
			if err := param.check(values[0]); err != nil {
				WriteError(w, logger, err)
				return
			}
			*param.value = values[0]
		}
		var before int64
		if page.After != "" {
			if ValidateID("cursor", page.After, TaskID) != nil {
				WriteError(w, logger, invalid("cursor", "invalid"))
				return
			}
			before, err = strconv.ParseInt(strings.TrimPrefix(page.After, "task-"), 10, 64)
			if err != nil || before <= 0 {
				WriteError(w, logger, invalid("cursor", "invalid"))
				return
			}
		}
		rows, err := store.ListTasks(r.Context(), db, filter, before, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		views := make([]taskSummaryView, 0, len(rows))
		for _, row := range rows {
			view := newTaskSummaryView(row.Task, row.TaskOrigin)
			if e := row.LatestExecution; e != nil {
				view.LatestExecution = &executionSummaryView{e.ID, e.Seq, e.Mode, e.Status, e.ErrorClass}
			}
			views = append(views, view)
		}
		writeJSON(w, http.StatusOK, NewPage(views, page.Limit, "tasks", func(v taskSummaryView) string { return v.ID }))
	}
}
