package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
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
			Title              string  `json:"title"`
			Body               string  `json:"body"`
			AcceptanceCriteria string  `json:"acceptanceCriteria"`
			ShortName          *string `json:"shortName"`
		}
		err := DecodeJSON(r, &input)
		if err == nil {
			err = ValidateID("projectId", input.ProjectID, SlugID)
		}
		if err == nil {
			for _, field := range []struct{ name, value string }{
				{"title", input.Title}, {"body", input.Body}, {"acceptanceCriteria", input.AcceptanceCriteria},
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
		project, err := store.GetProject(ctx, tx, input.ProjectID)
		if errors.Is(err, sql.ErrNoRows) {
			err = invalid("projectId", "not_found")
		}
		if err == nil {
			_, err = store.GetProjectHarness(ctx, tx, project.ID, project.DefaultRole)
			if errors.Is(err, sql.ErrNoRows) {
				err = &Error{Code: PreconditionFailed, Message: "Project has no Default Harness assignment",
					Details: []Detail{{Field: "projectId", Reason: "default_harness_missing"}}}
			}
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
			ProjectID: input.ProjectID, Title: input.Title, Body: input.Body,
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
				Detail: map[string]any{"projectId": created.ProjectID, "executionId": executionID.String(),
					"from": model.TaskReceived, "to": model.TaskQueued},
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
