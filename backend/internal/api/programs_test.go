package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/programauth"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type programFixture struct {
	*authFixture
	files   *store.ArtifactStore
	handler http.Handler
	key     string
}

// newProgramFixture registers harness-a with a default Profile and a program
// key routed directly to it (spec 20 4.3, Q235).
func newProgramFixture(t *testing.T) *programFixture {
	t.Helper()
	f, _, profiles := assignmentFixture(t)
	status, body := projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profiles[0].ID})
	requireHarness(t, status, body, 200)
	files, err := store.NewArtifactStore(f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	key, err := programauth.Register(context.Background(), f.db, "brain-research", "brain research", "", "harness-a")
	if err != nil {
		t.Fatal(err)
	}
	p := &programFixture{authFixture: f, files: files, key: key}
	p.handler = NewHandlerWithArtifacts(f.db, nil, f.auth, nil, ArtifactConfig{Store: files, MaxBytes: 1 << 20}, f.adapters)
	return p
}

func (p *programFixture) do(t *testing.T, handler http.Handler, method, path, key string, body any, alter func(*http.Request)) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(data))
	}
	r := httptest.NewRequest(method, p.server.URL+path, reader)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if alter != nil {
		alter(r)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (p *programFixture) create(t *testing.T, idem string, body any) (int, []byte) {
	t.Helper()
	return p.do(t, p.handler, "POST", "/api/programs/tasks", p.key, body, func(r *http.Request) {
		if idem != "" {
			r.Header.Set("Idempotency-Key", idem)
		}
	})
}

func (p *programFixture) mustCreate(t *testing.T, idem string, want int) programTaskCreated {
	t.Helper()
	status, body := p.create(t, idem, programInput())
	return requireCreated(t, status, body, want)
}

func (p *programFixture) ownerTask(t *testing.T, idem string, in map[string]any) model.Task {
	t.Helper()
	status, body := taskRequest(t, p.authFixture, idem, in, nil)
	return requireTask(t, status, body, http.StatusCreated)
}

func programInput() map[string]any {
	return map[string]any{"title": "second-brain: つながりの良し悪し", "body": "課題: second-brain\n問い: 無理なつなぎを落とす方法"}
}

func requireCreated(t *testing.T, status int, body []byte, want int) programTaskCreated {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	var created programTaskCreated
	if err := json.Unmarshal(body, &created); err != nil || ValidateID("id", created.TaskID, TaskID) != nil {
		t.Fatalf("response=%s err=%v", body, err)
	}
	return created
}

func (p *programFixture) finish(t *testing.T, taskID, taskStatus, executionStatus string) string {
	t.Helper()
	var executionID string
	if err := p.db.QueryRow(`SELECT id FROM executions WHERE task_id = ? ORDER BY seq DESC LIMIT 1`, taskID).Scan(&executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, executionStatus, executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, taskStatus, taskID); err != nil {
		t.Fatal(err)
	}
	return executionID
}

// spec 20 4.4: the key decides the routing target, the task is source=program
// and queued like a manual task, and replays return the original.
func TestProgramTaskCreation(t *testing.T) {
	p := newProgramFixture(t)
	status, body := p.create(t, "second-brain-2026-11-29", programInput())
	created := requireCreated(t, status, body, http.StatusCreated)
	if created.Status != string(model.TaskQueued) {
		t.Fatalf("created=%+v", created)
	}
	var source, harness, client, idem, executionStatus string
	var project *string
	err := p.db.QueryRow(`SELECT t.source, t.project_id, t.harness_id, t.program_client_id, t.idempotency_key, e.status
		FROM tasks t JOIN executions e ON e.task_id = t.id WHERE t.id = ?`, created.TaskID).
		Scan(&source, &project, &harness, &client, &idem, &executionStatus)
	if err != nil || source != "program" || project != nil || harness != "harness-a" || client != "brain-research" ||
		idem != "program:brain-research:second-brain-2026-11-29" || executionStatus != "pending" {
		t.Fatalf("stored source=%s project=%v harness=%s client=%s idem=%s execution=%s err=%v",
			source, project, harness, client, idem, executionStatus, err)
	}
	var actor string
	if err := p.db.QueryRow(`SELECT actor FROM audit_logs WHERE action = 'task.create' AND target_id = ?`, created.TaskID).Scan(&actor); err != nil || actor != "program:brain-research" {
		t.Fatalf("audit actor=%q err=%v", actor, err)
	}
	changed := programInput()
	changed["title"] = "different"
	status, body = p.create(t, "second-brain-2026-11-29", changed)
	if replay := requireCreated(t, status, body, http.StatusOK); replay.TaskID != created.TaskID {
		t.Fatalf("replay=%+v original=%+v", replay, created)
	}
	// The manual creation's key space is separate.
	in := manualTaskInput()
	delete(in, "projectId")
	in["harnessId"] = "harness-a"
	owner := p.ownerTask(t, "second-brain-2026-11-29", in)
	if owner.ID == created.TaskID {
		t.Fatal("program and owner idempotency keys collided")
	}
}

func TestProgramTaskInputValidation(t *testing.T) {
	p := newProgramFixture(t)
	with := func(k string, v any) map[string]any {
		in := programInput()
		in[k] = v
		return in
	}
	cases := []struct {
		name, idem    string
		body          any
		field, reason string
	}{
		{"missing key", "", programInput(), "Idempotency-Key", "required"},
		{"non ascii key", "日本", programInput(), "Idempotency-Key", "required"},
		{"long key", strings.Repeat("k", 201), programInput(), "Idempotency-Key", "required"},
		{"harness chosen", "a", with("harnessId", "harness-a"), "harnessId", "not_allowed"},
		{"project chosen", "b", with("projectId", "project-a"), "projectId", "not_allowed"},
		{"blank title", "c", with("title", "  "), "title", "required"},
		{"long title", "d", with("title", strings.Repeat("あ", 501)), "title", "too_long"},
		{"blank body", "e", with("body", ""), "body", "required"},
		{"long body", "f", with("body", strings.Repeat("x", 20001)), "body", "too_long"},
		{"unknown field", "g", with("extra", 1), "body", "invalid_json"},
	}
	for _, c := range cases {
		status, body := p.create(t, c.idem, c.body)
		requireProjectError(t, status, body, http.StatusBadRequest, ValidationFailed, c.field, c.reason)
	}
	var count int
	if err := p.db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tasks=%d err=%v", count, err)
	}
}

func TestProgramTaskUnavailableRoute(t *testing.T) {
	p := newProgramFixture(t)
	if _, err := p.db.Exec(`UPDATE harnesses SET default_profile_id = NULL WHERE id = 'harness-a'`); err != nil {
		t.Fatal(err)
	}
	status, body := p.create(t, "k", programInput())
	requireProjectError(t, status, body, http.StatusUnprocessableEntity, PreconditionFailed, "route", "route_unavailable")
}

// spec 20 4.6, Q236: two unfinished tasks and four per JST day per key.
// Replays are not counted.
func TestProgramTaskLimits(t *testing.T) {
	p := newProgramFixture(t)
	first := p.mustCreate(t, "1", http.StatusCreated)
	second := p.mustCreate(t, "2", http.StatusCreated)
	status, body := p.create(t, "3", programInput())
	requireProjectError(t, status, body, http.StatusTooManyRequests, TooManyRequests, "tasks", "open_limit")
	p.mustCreate(t, "1", http.StatusOK)
	p.finish(t, first.TaskID, "completed", "succeeded")
	p.finish(t, second.TaskID, "failed", "failed")
	third := p.mustCreate(t, "3", http.StatusCreated)
	p.finish(t, third.TaskID, "cancelled", "cancelled")
	p.mustCreate(t, "4", http.StatusCreated)
	status, body = p.create(t, "5", programInput())
	requireProjectError(t, status, body, http.StatusTooManyRequests, TooManyRequests, "tasks", "daily_limit")
	// A task created before today's 00:00 JST does not count toward today.
	yesterday := time.Now().In(programDay).Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := p.db.Exec(`UPDATE tasks SET created_at = ? WHERE id IN (?, ?)`, yesterday, first.TaskID, second.TaskID); err != nil {
		t.Fatal(err)
	}
	p.mustCreate(t, "5", http.StatusCreated)
}

// spec 20 4.1, 4.2, 9章: authentication classes and the loopback boundary.
func TestProgramAuthenticationBoundary(t *testing.T) {
	p := newProgramFixture(t)
	runnerToken, err := runnerauth.Register(context.Background(), p.db, "probe-runner", "probe", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := programauth.Register(context.Background(), p.db, "revoked", "revoked", "", "harness-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeProgramClient(context.Background(), p.db, "revoked", time.Now()); err != nil {
		t.Fatal(err)
	}
	get := "/api/programs/tasks/task-001"
	expect := func(name string, status int, body []byte, want int, code Code) {
		t.Helper()
		var envelope struct{ Error Error }
		if err := json.Unmarshal(body, &envelope); err != nil || status != want || envelope.Error.Code != code {
			t.Errorf("%s: status=%d body=%s; want %d %s", name, status, body, want, code)
		}
	}
	status, body := p.do(t, p.handler, "GET", get, "", nil, nil)
	expect("no key", status, body, 401, Unauthenticated)
	status, body = p.do(t, p.handler, "GET", get, "not-a-key", nil, nil)
	expect("malformed key", status, body, 401, Unauthenticated)
	status, body = p.do(t, p.handler, "GET", get, other, nil, nil)
	expect("revoked key", status, body, 401, Unauthenticated)
	status, body = p.do(t, p.handler, "GET", get, runnerToken, nil, nil)
	expect("runner token on program API", status, body, 403, Forbidden)
	status, body = p.do(t, p.handler, "GET", get, "", nil, func(r *http.Request) { r.AddCookie(p.cookie) })
	expect("owner session on program API", status, body, 403, Forbidden)
	status, body = p.do(t, p.handler, "GET", "/api/tasks", p.key, nil, nil)
	expect("program key on owner API", status, body, 403, Forbidden)
	status, body = p.do(t, p.handler, "POST", "/api/runners/probe-runner/claim", p.key, map[string]any{}, nil)
	expect("program key on Runner API", status, body, 403, Forbidden)
	for name, alter := range map[string]func(*http.Request){
		"origin":           func(r *http.Request) { r.Header.Set("Origin", p.server.URL) },
		"x-forwarded-for":  func(r *http.Request) { r.Header.Set("X-Forwarded-For", "100.64.0.2") },
		"forwarded":        func(r *http.Request) { r.Header.Set("Forwarded", "for=100.64.0.2") },
		"x-forwarded-host": func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "evaloom.example.ts.net") },
		"tailscale header": func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "someone@example.com") },
		"other host":       func(r *http.Request) { r.Host = "evil.example" },
	} {
		status, body = p.do(t, p.handler, "GET", get, p.key, nil, alter)
		expect(name, status, body, 403, Forbidden)
	}
	// The remote (tailnet) host is allowed for owners but never for programs.
	remote, err := NewOwnerAuth(p.db, nil, p.auth.host, "evaloom.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	remoteHandler := NewHandlerWithArtifacts(p.db, nil, remote, nil, ArtifactConfig{Store: p.files, MaxBytes: 1 << 20}, p.adapters)
	status, body = p.do(t, remoteHandler, "GET", get, p.key, nil, func(r *http.Request) { r.Host = "evaloom.example.ts.net" })
	expect("remote host", status, body, 403, Forbidden)
	var rejects int
	if err := p.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'`).Scan(&rejects); err != nil || rejects < 13 {
		t.Fatalf("auth.reject audits=%d err=%v", rejects, err)
	}
	var leaked int
	if err := p.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE instr(detail, ?) > 0 OR instr(target_id, ?) > 0`, p.key, p.key).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("key leaked into audit_logs: %d err=%v", leaked, err)
	}
	var tasks int
	if err := p.db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("rejected requests created tasks: %d", tasks)
	}
}

// spec 20 4.5: a key sees only its own tasks.
func TestProgramTaskVisibility(t *testing.T) {
	p := newProgramFixture(t)
	mine := p.mustCreate(t, "mine", http.StatusCreated)
	otherKey, err := programauth.Register(context.Background(), p.db, "other", "other", "", "harness-a")
	if err != nil {
		t.Fatal(err)
	}
	in := manualTaskInput()
	delete(in, "projectId")
	in["harnessId"] = "harness-a"
	owner := p.ownerTask(t, "owner", in)
	for name, c := range map[string]struct{ key, id string }{
		"other key's task": {otherKey, mine.TaskID},
		"owner task":       {p.key, owner.ID},
		"missing task":     {p.key, "task-999"},
		"invalid id":       {p.key, "nope"},
	} {
		status, body := p.do(t, p.handler, "GET", "/api/programs/tasks/"+c.id, c.key, nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("%s: status=%d body=%s", name, status, body)
		}
		status, _ = p.do(t, p.handler, "GET", "/api/programs/tasks/"+c.id+"/files/report.md", c.key, nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("%s file: status=%d", name, status)
		}
	}
	status, body := p.do(t, p.handler, "GET", "/api/programs/tasks/"+mine.TaskID, p.key, nil, nil)
	var view programTaskView
	if status != 200 || json.Unmarshal(body, &view) != nil || view.TaskID != mine.TaskID || view.Status != "queued" ||
		view.Execution == nil || view.Execution.Status != "pending" || view.Files == nil || len(view.Files) != 0 {
		t.Fatalf("own task status=%d body=%s", status, body)
	}
}

func (p *programFixture) saveArtifact(t *testing.T, executionID, id, stage, kind, name, content string) {
	t.Helper()
	if _, err := p.files.Save(context.Background(), store.Artifact{ID: id, ExecutionID: executionID, Stage: stage, Kind: kind, Name: name},
		strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
}

// spec 20 4.5, Q237: only a completed task exposes the check stage's out files
// and summary.md; logs, request/result JSON and questions never.
func TestProgramTaskFiles(t *testing.T) {
	p := newProgramFixture(t)
	created := p.mustCreate(t, "files", http.StatusCreated)
	executionID := p.finish(t, created.TaskID, "running", "checking")
	p.saveArtifact(t, executionID, "01ARZ3NDEKTSV4RRFFQ69G5FA1", "work", "out_file", "report.md", "work draft")
	p.saveArtifact(t, executionID, "01ARZ3NDEKTSV4RRFFQ69G5FA2", "work", "work_summary", "summary.md", "要約")
	p.saveArtifact(t, executionID, "01ARZ3NDEKTSV4RRFFQ69G5FA3", "work", "log", "work.log", "log")
	p.saveArtifact(t, executionID, "01ARZ3NDEKTSV4RRFFQ69G5FA4", "check", "out_file", "report.md", "# checked report")
	p.saveArtifact(t, executionID, "01ARZ3NDEKTSV4RRFFQ69G5FA5", "check", "result_json", "result.json", "{}")
	p.saveArtifact(t, executionID, "01ARZ3NDEKTSV4RRFFQ69G5FA6", "work", "question", "question.json", "{}")

	path := "/api/programs/tasks/" + created.TaskID
	status, body := p.do(t, p.handler, "GET", path, p.key, nil, nil)
	var view programTaskView
	if status != 200 || json.Unmarshal(body, &view) != nil || len(view.Files) != 0 {
		t.Fatalf("unfinished task must expose no files: %d %s", status, body)
	}
	if status, _ := p.do(t, p.handler, "GET", path+"/files/report.md", p.key, nil, nil); status != 404 {
		t.Fatalf("unfinished file status=%d", status)
	}

	p.finish(t, created.TaskID, "completed", "succeeded")
	status, body = p.do(t, p.handler, "GET", path, p.key, nil, nil)
	view = programTaskView{}
	if status != 200 || json.Unmarshal(body, &view) != nil {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var names []string
	for _, f := range view.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "summary.md,report.md" || view.Status != "completed" || view.Execution.Status != "succeeded" {
		t.Fatalf("files=%v view=%s", names, body)
	}
	for name, want := range map[string]string{"report.md": "# checked report", "summary.md": "要約"} {
		status, content := p.do(t, p.handler, "GET", path+"/files/"+name, p.key, nil, nil)
		if status != 200 || string(content) != want {
			t.Errorf("%s: status=%d content=%q", name, status, content)
		}
	}
	for _, name := range []string{"work.log", "result.json", "question.json", "missing.md"} {
		if status, _ := p.do(t, p.handler, "GET", path+"/files/"+name, p.key, nil, nil); status != 404 {
			t.Errorf("%s: status=%d; want 404", name, status)
		}
	}
}
