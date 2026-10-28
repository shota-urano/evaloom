package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// Decode independently of response structs to check the public JSON names.
func requireTaskDetail(t *testing.T, f *authFixture, id string) map[string]any {
	t.Helper()
	status, body := f.request(t, "GET", "/api/tasks/"+id, true, "", nil)
	if status != 200 {
		t.Fatalf("GET task: %d %s", status, body)
	}
	var detail map[string]any
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func requireDetailFields(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for field, value := range want {
		if actual, ok := got[field]; !ok || !reflect.DeepEqual(actual, value) {
			t.Fatalf("%s=%#v (present=%v); want %#v; response=%v", field, actual, ok, value, got)
		}
	}
}

func TestGetTaskReturnsOverviewAndPendingExecution(t *testing.T) {
	f := taskFixture(t)
	input := manualTaskInput()
	status, body := taskRequest(t, f, "task-detail", input, nil)
	created := requireTask(t, status, body, 201)
	detail := requireTaskDetail(t, f, created.ID)
	requireDetailFields(t, detail, map[string]any{
		"id": created.ID, "projectId": input["projectId"], "title": input["title"],
		"body": input["body"], "acceptanceCriteria": input["acceptanceCriteria"],
		"status": "queued", "prUrl": nil,
	})
	if _, ok := detail["idempotencyKey"]; ok {
		t.Fatal("response exposes idempotency key")
	}
	executions := detail["executions"].([]any)
	if len(executions) != 1 {
		t.Fatalf("executions=%v", executions)
	}
	execution := executions[0].(map[string]any)
	requireDetailFields(t, execution, map[string]any{
		"seq": float64(1), "mode": "continue", "status": "pending", "profileVersion": nil,
		"profileCommitSha": nil, "startedAt": nil, "finishedAt": nil, "errorClass": nil,
	})
	if id, ok := execution["id"].(string); !ok || ValidateID("id", id, ULID) != nil {
		t.Fatalf("invalid execution ID: %v", execution["id"])
	}
	latest := detail["latestExecution"].(map[string]any)
	requireDetailFields(t, latest, map[string]any{"id": execution["id"], "seq": float64(1), "status": "pending"})
}

func TestGetTaskReturnsOrderedHistoryAndLatestRecordedPRURL(t *testing.T) {
	f, token, profiles := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "detail-history")
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body).Execution.ID
	finish := func(id, terminal, prURL string) {
		t.Helper()
		from := "claimed"
		for _, next := range []string{"preparing", "running", "verifying", "publishing", terminal} {
			input := resultBody(from, next)
			if next == terminal {
				input["prUrl"] = prURL
			}
			status, body := postResult(t, f, token, id, input)
			if status != 204 {
				t.Fatalf("report %s: %d %s", next, status, body)
			}
			from = next
		}
	}
	finish(first, "succeeded", "https://example.com/pr/1")
	requireDetailFields(t, requireTaskDetail(t, f, created.ID), map[string]any{
		"status": "completed", "prUrl": "https://example.com/pr/1",
	})

	// Later assignments must not change historical Profile pins.
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
	requireAssignment(t, status, body, 200)
	status, body = requestRetry(t, f, created.ID, `{"mode":"fresh"}`, nil)
	requireRetrySuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	second := requireClaim(t, status, body).Execution.ID
	finish(second, "failed", "https://example.com/pr/2")
	status, body = requestRetry(t, f, created.ID, `{"mode":"continue"}`, nil)
	requireRetrySuccess(t, status, body)
	// Another task must not appear in this history.
	createClaimTask(t, f, "project-b", "other-history")

	detail := requireTaskDetail(t, f, created.ID)
	requireDetailFields(t, detail, map[string]any{
		"id": created.ID, "projectId": "project-a", "status": "queued", "prUrl": "https://example.com/pr/2",
	})
	for _, summary := range requireTaskList(t, f, "").Items {
		if summary.ID == created.ID && (summary.PRURL == nil || *summary.PRURL != detail["prUrl"]) {
			t.Fatalf("detail PR URL differs from list: list=%v detail=%v", summary.PRURL, detail["prUrl"])
		}
	}
	executions := detail["executions"].([]any)
	if len(executions) != 3 {
		t.Fatalf("history=%v", executions)
	}
	for i, want := range []map[string]any{
		{"id": first, "seq": float64(1), "mode": "continue", "status": "succeeded",
			"profileVersion": profiles[0].Version, "profileCommitSha": profiles[0].CommitSHA, "errorClass": nil},
		{"id": second, "seq": float64(2), "mode": "fresh", "status": "failed",
			"profileVersion": profiles[1].Version, "profileCommitSha": profiles[1].CommitSHA, "errorClass": "execution_failed"},
		{"seq": float64(3), "mode": "continue", "status": "pending", "profileVersion": nil,
			"profileCommitSha": nil, "errorClass": nil, "startedAt": nil, "finishedAt": nil},
	} {
		execution := executions[i].(map[string]any)
		requireDetailFields(t, execution, want)
		if i < 2 {
			var started, finished *string
			id := execution["id"].(string)
			if err := f.db.QueryRow(`SELECT started_at, finished_at FROM executions WHERE id = ?`, id).Scan(&started, &finished); err != nil {
				t.Fatal(err)
			}
			if finished == nil || started == nil {
				t.Fatalf("timestamps were not persisted: started=%v finished=%v", started, finished)
			}
			requireDetailFields(t, execution, map[string]any{"startedAt": *started})
			requireDetailFields(t, execution, map[string]any{"finishedAt": *finished})
			if _, err := ParseTimestamp(*finished); err != nil {
				t.Fatal(err)
			}
		}
	}
	requireDetailFields(t, detail["latestExecution"].(map[string]any), map[string]any{"seq": float64(3), "status": "pending", "mode": "continue"})
}

func TestGetTaskMissingReturns404NotFound(t *testing.T) {
	f := taskFixture(t)
	status, body := f.request(t, "GET", "/api/tasks/task-999999", true, "", nil)
	assertAuthError(t, status, body, 404, NotFound)
}

func TestGetTaskResumeKeepsSameExecutionAndMode(t *testing.T) {
	f, _, id, decision := resumeFixture(t, "gate1_test_change")
	taskID := executionTaskID(t, f, id)
	before := requireTaskDetail(t, f, taskID)["executions"].([]any)[0].(map[string]any)
	status, body := requestResume(t, f, taskID, decision)
	requireResumeSuccess(t, status, body)
	detail := requireTaskDetail(t, f, taskID)
	requireDetailFields(t, detail, map[string]any{"status": "queued"})
	executions := detail["executions"].([]any)
	if len(executions) != 1 {
		t.Fatalf("resume added history: %v", executions)
	}
	requireDetailFields(t, executions[0].(map[string]any), map[string]any{
		"id": id, "seq": float64(1), "mode": "continue", "status": "pending",
		"profileVersion": before["profileVersion"], "profileCommitSha": before["profileCommitSha"],
		"startedAt": before["startedAt"], "finishedAt": nil,
	})
}

func TestGetTaskStorageFailureReturns500(t *testing.T) {
	f := taskFixture(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	status, body := f.request(t, "GET", "/api/tasks/task-001", true, "", nil)
	assertAuthError(t, status, body, 500, Internal)
}

func TestGetTaskRequiresOwnerSession(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "detail-auth")
	for _, id := range []string{created.ID, "task-999999"} {
		status, body := f.request(t, "GET", "/api/tasks/"+id, false, "", nil)
		assertAuthError(t, status, body, 401, Unauthenticated)
	}
	status, body := f.request(t, "GET", "/api/tasks/"+created.ID, false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	assertAuthError(t, status, body, 403, Forbidden)
}

func TestGetTaskRejectsMalformedID(t *testing.T) {
	f := taskFixture(t)
	status, body := f.request(t, "GET", "/api/tasks/invalid", true, "", nil)
	requireProjectError(t, status, body, 400, ValidationFailed, "id", "invalid_format")
}
