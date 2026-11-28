package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/programauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// Spec 20 4.4 and 4.6. The limits are Evaloom constants; change the spec to
// change them.
const (
	programMaxOpenTasks   = 2
	programMaxDailyTasks  = 4
	programMaxTitleRunes  = 500
	programMaxBodyRunes   = 20000
	programMaxIdemKeyLen  = 200
	programMaxRequestBody = 1 << 20
)

// programDay is the day boundary for the daily limit: 00:00 JST (spec 20 4.6).
var programDay = time.FixedZone("JST", 9*60*60)

type programClientKey struct{}

// forwardedHeader reports headers that a proxy such as tailscale serve adds
// (spec 20 4.2, Q238). A request carrying any of them did not come straight
// from a local program.
func forwardedHeader(h http.Header) bool {
	for name := range h {
		lower := strings.ToLower(name)
		switch {
		case lower == "x-forwarded-for", lower == "x-forwarded-host", lower == "x-forwarded-proto",
			lower == "forwarded", strings.HasPrefix(lower, "tailscale-"):
			return true
		}
	}
	return false
}

// bearer returns the single Bearer credential, or "".
func bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// programClient resolves the request's Bearer credential as a program key.
// Absent, malformed, unknown and revoked keys return ok=false.
func (a *OwnerAuth) programClient(r *http.Request) (store.ProgramClient, bool, error) {
	key := bearer(r)
	if key == "" {
		return store.ProgramClient{}, false, nil
	}
	client, err := programauth.Authenticate(r.Context(), a.db, key)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ProgramClient{}, false, nil
	}
	if err != nil {
		return store.ProgramClient{}, false, err
	}
	return client, true, nil
}

// protectProgram is the program authentication class (spec 20 4.1, 4.2):
// loopback Host only, no browser Origin, no proxy headers, and a program key.
// Owner sessions and Runner tokens are a different class and are refused.
func (a *OwnerAuth) protectProgram(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != a.host {
			a.reject(w, r, Forbidden, "host_mismatch", "program APIs accept only the loopback host")
			return
		}
		if len(r.Header.Values("Origin")) != 0 {
			a.reject(w, r, Forbidden, "origin_mismatch", "program APIs do not accept browser requests")
			return
		}
		if forwardedHeader(r.Header) {
			a.reject(w, r, Forbidden, "forwarded_request", "program APIs do not accept proxied requests")
			return
		}
		token, _, err := a.ownerSession(r)
		if err != nil {
			WriteError(w, a.logger, err)
			return
		}
		if token != "" {
			a.reject(w, r, Forbidden, "wrong_auth_class", "owner credentials cannot access program APIs")
			return
		}
		runner, err := a.runnerIdentity(r)
		if err != nil {
			WriteError(w, a.logger, err)
			return
		}
		if runner != "" {
			a.reject(w, r, Forbidden, "wrong_auth_class", "Runner credentials cannot access program APIs")
			return
		}
		client, ok, err := a.programClient(r)
		if err != nil {
			WriteError(w, a.logger, err)
			return
		}
		if !ok {
			a.reject(w, r, Unauthenticated, "invalid_client_key", "program key is missing or invalid")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), programClientKey{}, client)))
	})
}

func requestProgramClient(r *http.Request) store.ProgramClient {
	client, _ := r.Context().Value(programClientKey{}).(store.ProgramClient)
	return client
}

func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > programMaxIdemKeyLen || strings.TrimSpace(key) == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

type programTaskCreated struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

// createProgramTask serves POST /api/programs/tasks (spec 20 4.4).
func createProgramTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := requestProgramClient(r)
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || !validIdempotencyKey(keys[0]) {
			WriteError(w, logger, invalid("Idempotency-Key", "required"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, programMaxRequestBody)
		var input struct {
			ProjectID          *json.RawMessage `json:"projectId"`
			HarnessID          *json.RawMessage `json:"harnessId"`
			Title              string           `json:"title"`
			Body               string           `json:"body"`
			AcceptanceCriteria string           `json:"acceptanceCriteria"`
		}
		err := DecodeJSON(r, &input)
		switch {
		case err != nil:
		case input.ProjectID != nil:
			err = invalid("projectId", "not_allowed")
		case input.HarnessID != nil:
			err = invalid("harnessId", "not_allowed")
		case strings.TrimSpace(input.Title) == "":
			err = invalid("title", "required")
		case utf8.RuneCountInString(input.Title) > programMaxTitleRunes:
			err = invalid("title", "too_long")
		case strings.TrimSpace(input.Body) == "":
			err = invalid("body", "required")
		case utf8.RuneCountInString(input.Body) > programMaxBodyRunes:
			err = invalid("body", "too_long")
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		idempotencyKey := "program:" + client.ID + ":" + keys[0]
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		existing, err := store.GetTaskByIdempotencyKey(ctx, tx, idempotencyKey)
		if err == nil {
			writeJSON(w, http.StatusOK, programTaskCreated{existing.ID, string(existing.Status), existing.CreatedAt})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			WriteError(w, logger, err)
			return
		}
		profileID, err := manualTaskProfileID(ctx, tx, client.ProjectID, client.HarnessID)
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Code == PreconditionFailed {
			err = &Error{Code: PreconditionFailed, Message: "the key's routing target is not available",
				Details: []Detail{{Field: "route", Reason: "route_unavailable"}}}
		}
		if err == nil {
			err = checkManualAcceptanceCriteria(ctx, tx, profileID, input.AcceptanceCriteria)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		now := time.Now().UTC()
		y, m, d := now.In(programDay).Date()
		open, today, err := store.ProgramTaskCounts(ctx, tx, client.ID, time.Date(y, m, d, 0, 0, 0, 0, programDay))
		if err == nil {
			switch {
			case open >= programMaxOpenTasks:
				err = &Error{Code: TooManyRequests, Message: "too many unfinished tasks for this key",
					Details: []Detail{{Field: "tasks", Reason: "open_limit"}}}
			case today >= programMaxDailyTasks:
				err = &Error{Code: TooManyRequests, Message: "daily task limit reached for this key",
					Details: []Detail{{Field: "tasks", Reason: "daily_limit"}}}
			}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		executionID, err := ulid.New(ulid.Timestamp(now), rand.Reader)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		created := model.Task{
			ProjectID: client.ProjectID, HarnessID: client.HarnessID, Title: input.Title, Body: input.Body,
			AcceptanceCriteria: input.AcceptanceCriteria, CollaborationMode: "solo", Status: model.TaskReceived,
			IdempotencyKey: idempotencyKey, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
		}
		err = store.InsertProgramTask(ctx, tx, &created, client.ID)
		if err == nil {
			err = store.InsertInitialExecution(ctx, tx, executionID.String(), created.ID, created.CreatedAt)
		}
		if err == nil {
			err = task.QueueExecutionTx(ctx, tx, created.ID, executionID.String(), model.TaskReceived)
		}
		if err == nil {
			created, err = store.GetTaskByIdempotencyKey(ctx, tx, idempotencyKey)
		}
		if err == nil {
			detail := manualTaskAuditDetail(created, executionID.String())
			detail["programClientId"] = client.ID
			_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
				Actor: "program:" + client.ID, Action: "task.create", TargetType: "task", TargetID: created.ID,
				Detail: detail,
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
		writeJSON(w, http.StatusCreated, programTaskCreated{created.ID, string(created.Status), created.CreatedAt})
	}
}

type programExecutionView struct {
	ID           string  `json:"id"`
	Seq          int     `json:"seq"`
	Status       string  `json:"status"`
	ErrorClass   *string `json:"errorClass"`
	ErrorMessage *string `json:"errorMessage"`
}

type programFileView struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type programTaskView struct {
	TaskID    string                `json:"taskId"`
	Status    string                `json:"status"`
	CreatedAt string                `json:"createdAt"`
	UpdatedAt string                `json:"updatedAt"`
	Execution *programExecutionView `json:"execution"`
	Files     []programFileView     `json:"files"`
}

// loadProgramTask reads the client's own task and the files it may take out:
// only for a completed latest Execution (spec 20 4.5, Q237).
func loadProgramTask(r *http.Request, db *sql.DB) (store.ProgramTask, []store.ProgramFile, error) {
	id := r.PathValue("id")
	if err := ValidateID("id", id, TaskID); err != nil {
		return store.ProgramTask{}, nil, &Error{Code: NotFound, Message: "Task not found"}
	}
	t, err := store.GetProgramTask(r.Context(), db, requestProgramClient(r).ID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ProgramTask{}, nil, &Error{Code: NotFound, Message: "Task not found"}
	}
	if err != nil {
		return store.ProgramTask{}, nil, err
	}
	if t.Status != string(model.TaskCompleted) || t.Execution == nil || t.Execution.Status != "succeeded" {
		return t, nil, nil
	}
	files, err := store.ListProgramFiles(r.Context(), db, t.Execution.ID)
	return t, files, err
}

// getProgramTask serves GET /api/programs/tasks/{id} (spec 20 4.5).
func getProgramTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, files, err := loadProgramTask(r, db)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		view := programTaskView{TaskID: t.ID, Status: t.Status, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			Files: []programFileView{}}
		if e := t.Execution; e != nil {
			view.Execution = &programExecutionView{e.ID, e.Seq, e.Status, e.ErrorClass, e.ErrorMessage}
		}
		for _, f := range files {
			view.Files = append(view.Files, programFileView{f.Name, f.SizeBytes, f.SHA256})
		}
		writeJSON(w, http.StatusOK, view)
	}
}

// getProgramFile serves GET /api/programs/tasks/{id}/files/{name...}.
func getProgramFile(db *sql.DB, logger *slog.Logger, config ArtifactConfig) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		t, files, err := loadProgramTask(r, db)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		name := r.PathValue("name")
		var chosen *store.ProgramFile
		for i := range files {
			if files[i].Name == name {
				chosen = &files[i]
				break
			}
		}
		if chosen == nil {
			WriteError(w, logger, &Error{Code: NotFound, Message: "File not found"})
			return
		}
		if config.Store == nil {
			WriteError(w, logger, fmt.Errorf("artifact storage must be configured"))
			return
		}
		artifact, file, err := config.Store.Open(r.Context(), t.Execution.ID, chosen.ArtifactID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = &Error{Code: NotFound, Message: "File not found"}
			}
			WriteError(w, logger, err)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.SizeBytes, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, file); err != nil {
			logger.Error("Program file response failed", "artifactId", artifact.ID, "error", err)
		}
	}
}
