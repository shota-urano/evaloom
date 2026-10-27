package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

func taskFixture(t *testing.T) *authFixture {
	t.Helper()
	f, _, _ := assignmentFixture(t)
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", "1.2.0")
	requireAssignment(t, status, body, 201)
	return f
}

func manualTaskInput() map[string]any {
	return map[string]any{"projectId": "project-a", "title": "Add login", "body": "Implement the login form.", "acceptanceCriteria": "Login tests pass."}
}

// newAuthFixture reserves /api/tasks for its authentication probe. Exercise the
// production router directly here, with the same real login session and DB.
func taskRequest(t *testing.T, f *authFixture, key string, input any, alter func(*http.Request)) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", f.server.URL+"/api/tasks", strings.NewReader(string(data)))
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", f.server.URL)
	r.Header.Set("X-CSRF-Token", f.csrf)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if alter != nil {
		alter(r)
	}
	w := httptest.NewRecorder()
	NewHandler(f.db, nil, f.auth).ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func requireTask(t *testing.T, status int, body []byte, want int) model.Task {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	var task model.Task
	if err := json.Unmarshal(body, &task); err != nil || ValidateID("id", task.ID, TaskID) != nil {
		t.Fatalf("invalid task response: %s err=%v", body, err)
	}
	for _, value := range []string{task.CreatedAt, task.UpdatedAt} {
		if _, err := ParseTimestamp(value); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func requireTaskCounts(t *testing.T, f *authFixture, want int) {
	t.Helper()
	for _, table := range []string{"tasks", "executions", "execution_events"} {
		if got := projectCount(t, f, table); got != want {
			t.Fatalf("%s rows=%d; want %d", table, got, want)
		}
	}
	var audits int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'task.create'`).Scan(&audits); err != nil || audits != want {
		t.Fatalf("task.create audits=%d err=%v; want %d", audits, err, want)
	}
}

func TestManualTaskCreatesQueuedTaskAndInitialPendingExecution(t *testing.T) {
	f := taskFixture(t)
	for i, shortName := range []string{"task", "add-login"} {
		input := manualTaskInput()
		if i == 1 {
			input["shortName"] = shortName
		}
		key := fmt.Sprintf("create-%d", i)
		status, body := taskRequest(t, f, key, input, nil)
		created := requireTask(t, status, body, 201)
		if created.ID != fmt.Sprintf("task-%03d", i+1) || created.ProjectID != "project-a" ||
			created.Title != input["title"] || created.Body != input["body"] || created.AcceptanceCriteria != input["acceptanceCriteria"] ||
			created.ShortName != shortName || created.Status != model.TaskQueued || created.TaskType != "development" || created.CollaborationMode != "solo" {
			t.Fatalf("unexpected task: %+v", created)
		}
		// Compare all task columns independently of the store's response reader.
		var stored model.Task
		err := f.db.QueryRow(`SELECT id, project_id, title, body, acceptance_criteria, short_name,
			task_type, collaboration_mode, status, idempotency_key, created_at, updated_at FROM tasks WHERE id = ?`, created.ID).
			Scan(&stored.ID, &stored.ProjectID, &stored.Title, &stored.Body, &stored.AcceptanceCriteria, &stored.ShortName,
				&stored.TaskType, &stored.CollaborationMode, &stored.Status, &stored.IdempotencyKey, &stored.CreatedAt, &stored.UpdatedAt)
		if err != nil || stored.IdempotencyKey != key {
			t.Fatalf("stored=%+v err=%v", stored, err)
		}
		stored.IdempotencyKey = ""
		if stored != created || strings.Contains(string(body), key) {
			t.Fatalf("response differs from DB or exposes key: stored=%+v body=%s", stored, body)
		}
		var executionID, executionStatus, mode, timestamp string
		var seq, unpinned int
		err = f.db.QueryRow(`SELECT id, seq, mode, status, created_at,
			runner_id IS NULL AND role IS NULL AND harness_id IS NULL AND profile_id IS NULL
			AND profile_version IS NULL AND profile_commit_sha IS NULL AND claimed_at IS NULL
			FROM executions WHERE task_id = ?`, created.ID).
			Scan(&executionID, &seq, &mode, &executionStatus, &timestamp, &unpinned)
		if err != nil || ValidateID("executionId", executionID, ULID) != nil || seq != 1 || mode != "continue" || executionStatus != "pending" || unpinned != 1 || timestamp != created.CreatedAt {
			t.Fatalf("execution=%s seq=%d mode=%s status=%s unpinned=%d time=%s err=%v", executionID, seq, mode, executionStatus, unpinned, timestamp, err)
		}
		var eventType, payload, actor, targetType, targetID, detail string
		err = f.db.QueryRow(`SELECT type, payload FROM execution_events WHERE execution_id = ?`, executionID).Scan(&eventType, &payload)
		var event map[string]string
		if err != nil || json.Unmarshal([]byte(payload), &event) != nil || eventType != "status_changed" || event["from"] != "received" || event["to"] != "queued" || event["reason"] != "task_created" {
			t.Fatalf("event=%s payload=%s err=%v", eventType, payload, err)
		}
		err = f.db.QueryRow(`SELECT actor, target_type, target_id, detail FROM audit_logs WHERE action = 'task.create' AND target_id = ?`, created.ID).
			Scan(&actor, &targetType, &targetID, &detail)
		var audit map[string]string
		if err != nil || json.Unmarshal([]byte(detail), &audit) != nil || actor != "owner" || targetType != "task" || targetID != created.ID || audit["executionId"] != executionID || audit["projectId"] != created.ProjectID {
			t.Fatalf("audit=%s %s %s %s err=%v", actor, targetType, targetID, detail, err)
		}
		requireTaskCounts(t, f, i+1)
	}
}

func TestManualTaskRejectsEmptyAcceptanceCriteriaWithoutRows(t *testing.T) {
	f := taskFixture(t)
	for _, value := range []any{nil, "", " \t\n\u3000"} {
		input := manualTaskInput()
		if value == nil {
			delete(input, "acceptanceCriteria")
		} else {
			input["acceptanceCriteria"] = value
		}
		status, body := taskRequest(t, f, "empty-criteria", input, nil)
		requireProjectError(t, status, body, 400, ValidationFailed, "acceptanceCriteria", "required")
		requireTaskCounts(t, f, 0)
	}
}

func TestManualTaskRequiresAssignmentForActualDefaultRole(t *testing.T) {
	f := taskFixture(t)
	// developer is assigned, but it is no longer this Project's default role.
	if _, err := f.db.Exec(`UPDATE projects SET default_role = 'custom role' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	status, body := taskRequest(t, f, "missing-default", manualTaskInput(), nil)
	requireProjectError(t, status, body, 422, PreconditionFailed, "projectId", "default_harness_missing")
	requireTaskCounts(t, f, 0)
	status, body = postAssignment(t, f, "project-a", "custom role", "harness-a", "1.2.0")
	requireAssignment(t, status, body, 201)
	status, body = taskRequest(t, f, "missing-default", manualTaskInput(), nil)
	requireTask(t, status, body, 201)
	requireTaskCounts(t, f, 1)
}

func TestManualTaskIdempotencyReturnsOriginalAcrossChangedInputAndAssignment(t *testing.T) {
	f := taskFixture(t)
	status, body := taskRequest(t, f, "replay", manualTaskInput(), nil)
	original := requireTask(t, status, body, 201)
	// Replays do not require the original assignment to remain current.
	if _, err := f.db.Exec(`UPDATE projects SET default_role = 'unassigned' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"project-a", "project-b", "missing"} {
		input := manualTaskInput()
		input["projectId"], input["title"] = project, "Different title"
		status, body := taskRequest(t, f, "replay", input, nil)
		if got := requireTask(t, status, body, 200); got != original {
			t.Fatalf("replay=%+v original=%+v", got, original)
		}
		requireTaskCounts(t, f, 1)
	}
}

func TestManualTaskConcurrentRetriesAndDistinctKeys(t *testing.T) {
	f := taskFixture(t)
	for _, distinct := range []bool{false, true} {
		var wg sync.WaitGroup
		var mu sync.Mutex
		ids := map[string]bool{}
		created := 0
		for i := range 12 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := "concurrent-replay"
				if distinct {
					key = fmt.Sprintf("distinct-%d", i)
				}
				status, body := taskRequest(t, f, key, manualTaskInput(), nil)
				var result model.Task
				if (status != 200 && status != 201) || json.Unmarshal(body, &result) != nil || result.Status != model.TaskQueued {
					t.Errorf("status=%d body=%s", status, body)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				ids[result.ID] = true
				if status == 201 {
					created++
				}
			}(i)
		}
		wg.Wait()
		want := 1
		if distinct {
			want = 12
		}
		if len(ids) != want || created != want {
			t.Fatalf("distinct=%t ids=%v creations=%d; want %d", distinct, ids, created, want)
		}
	}
	requireTaskCounts(t, f, 13)
	var queuedPending int
	err := f.db.QueryRow(`SELECT count(*) FROM tasks t JOIN executions e ON e.task_id = t.id
		WHERE t.status = 'queued' AND e.status = 'pending' AND e.seq = 1 AND e.mode = 'continue'`).Scan(&queuedPending)
	if err != nil || queuedPending != 13 {
		t.Fatalf("queued/pending pairs=%d err=%v; want 13", queuedPending, err)
	}
}

func TestManualTaskFailuresRollBackEntireCreation(t *testing.T) {
	f := taskFixture(t)
	for _, table := range []string{"executions", "audit_logs", "execution_events"} {
		trigger := `CREATE TRIGGER reject_creation BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`
		if _, err := f.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		status, body := taskRequest(t, f, "rollback", manualTaskInput(), nil)
		requireProjectError(t, status, body, 500, Internal, "", "")
		if strings.Contains(string(body), "storage unavailable") {
			t.Fatal("internal error disclosed")
		}
		requireTaskCounts(t, f, 0)
		if _, err := f.db.Exec(`DROP TRIGGER reject_creation`); err != nil {
			t.Fatal(err)
		}
	}
	status, body := taskRequest(t, f, "rollback", manualTaskInput(), nil)
	if got := requireTask(t, status, body, 201); got.ID != "task-001" {
		t.Fatalf("rollback consumed ID: %+v", got)
	}
	requireTaskCounts(t, f, 1)
}

func TestManualTaskInputValidation(t *testing.T) {
	f := taskFixture(t)
	for _, tc := range []struct {
		field  string
		value  any
		reason string
	}{
		{"projectId", "", "required"}, {"projectId", "INVALID", "invalid_format"},
		{"projectId", "missing", "not_found"}, {"title", " \t", "required"}, {"body", "", "required"},
		{"shortName", "", "invalid_format"}, {"shortName", "Upper", "invalid_format"},
		{"shortName", "a_b", "invalid_format"}, {"shortName", strings.Repeat("a", 41), "invalid_format"},
		{"taskType", "development", "invalid_json"}, {"collaborationMode", "solo", "invalid_json"},
	} {
		input := manualTaskInput()
		input[tc.field] = tc.value
		status, body := taskRequest(t, f, "validation", input, nil)
		field := tc.field
		if tc.reason == "invalid_json" {
			field = "body"
		}
		requireProjectError(t, status, body, 400, ValidationFailed, field, tc.reason)
	}
	for _, key := range []string{"", " \t", "duplicate"} {
		status, body := taskRequest(t, f, key, manualTaskInput(), func(r *http.Request) {
			if key == "duplicate" {
				r.Header.Add("Idempotency-Key", "second-key")
			}
		})
		requireProjectError(t, status, body, 400, ValidationFailed, "Idempotency-Key", "required")
	}
	status, body := taskRequest(t, f, "null-json", nil, nil)
	requireProjectError(t, status, body, 400, ValidationFailed, "body", "invalid_json")
	requireTaskCounts(t, f, 0)
	for _, shortName := range []string{strings.Repeat("a", 40), "a-1"} {
		input := manualTaskInput()
		input["shortName"] = shortName
		status, body := taskRequest(t, f, shortName, input, nil)
		if got := requireTask(t, status, body, 201); got.ShortName != shortName {
			t.Fatalf("shortName=%s; want %s", got.ShortName, shortName)
		}
	}
	requireTaskCounts(t, f, 2)
}

func TestManualTaskUsesOwnerAuthenticationBoundary(t *testing.T) {
	f := taskFixture(t)
	token, err := runnerauth.Register(context.Background(), f.db, "auth-runner", "Auth Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		alter  func(*http.Request)
		status int
		code   Code
	}{
		{"session", func(r *http.Request) { r.Header.Del("Cookie") }, 401, Unauthenticated},
		{"csrf", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403, Forbidden},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://other.invalid") }, 403, Forbidden},
		{"missing-origin", func(r *http.Request) { r.Header.Del("Origin") }, 403, Forbidden},
		{"host", func(r *http.Request) { r.Host = "other.invalid" }, 403, Forbidden},
		{"runner", func(r *http.Request) { r.Header.Del("Cookie"); r.Header.Set("Authorization", "Bearer "+token) }, 403, Forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := taskRequest(t, f, "auth-reject", manualTaskInput(), tc.alter)
			requireProjectError(t, status, body, tc.status, tc.code, "", "")
			requireTaskCounts(t, f, 0)
		})
	}
}
