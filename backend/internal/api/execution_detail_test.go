package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/oklog/ulid/v2"
)

func requireExecutionDetail(t *testing.T, f *authFixture, id string) map[string]any {
	t.Helper()
	status, body := f.request(t, "GET", "/api/executions/"+id, true, "", nil)
	if status != 200 {
		t.Fatalf("GET execution: %d %s", status, body)
	}
	var detail map[string]any
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func pendingDetailExecution(t *testing.T, f *authFixture) string {
	t.Helper()
	created := createClaimTask(t, f, "project-a", "execution-detail")
	var id string
	if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, created.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGetExecutionReturnsPinnedVersionAndContexts(t *testing.T) {
	f, token, profiles := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "execution-detail-pins")
	status, body := requestClaim(t, f, token)
	id := requireClaim(t, status, body).Execution.ID
	pin := readExecutionPin(t, f, id)
	from := "claimed"
	for _, next := range []string{"working", "checking", "publishing", "succeeded"} {
		input := resultBody(from, next)
		status, body = postResult(t, f, token, id, input)
		if status != 204 {
			t.Fatalf("report %s: %d %s", next, status, body)
		}
		from = next
	}
	// A new assignment and a newer Execution must not change historical pins
	// or leak the newer Execution's Context into this one.
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
	requireAssignment(t, status, body, 200)
	status, body = requestRetry(t, f, created.ID, `{"mode":"fresh"}`, nil)
	requireRetrySuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	otherID := requireClaim(t, status, body).Execution.ID

	const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const recordedAt = "2026-10-28T02:03:04.123456Z"
	var wantContexts []any
	for _, source := range []struct{ layer, kind, path string }{
		{"global", "global_policy", "context/policy.md"},
		{"harness", "instructions", "prompts/developer.md"},
		{"reported", "reported", "AGENTS.md"},
		{"reported", "reported", "backend/AGENTS.md"},
		{"task", "task_input", "task.json"},
	} {
		revisionID := ulid.Make().String()
		content := "context-body-must-not-be-returned:" + source.path
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		if _, err := f.db.Exec(`INSERT INTO context_revisions
			(id, execution_id, layer, kind, source_path, source_revision, content_hash, content, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, revisionID, id, source.layer, source.kind,
			source.path, baseSHA, hash, content, recordedAt); err != nil {
			t.Fatal(err)
		}
		wantContexts = append(wantContexts, map[string]any{
			"id": revisionID, "layer": source.layer, "kind": source.kind, "sourcePath": source.path,
			"sourceCommitSha": baseSHA, "contentHash": hash,
		})
	}
	if _, err := f.db.Exec(`INSERT INTO context_revisions
		(id, execution_id, layer, kind, source_path, source_revision, content_hash, content, created_at)
		VALUES (?, ?, 'reported', 'reported', 'other.md', ?, 'other-hash', 'other-content', ?)`,
		ulid.Make().String(), otherID, baseSHA, recordedAt); err != nil {
		t.Fatal(err)
	}

	detail := requireExecutionDetail(t, f, id)
	var started, finished string
	if err := f.db.QueryRow(`SELECT started_at, finished_at FROM executions WHERE id = ?`, id).Scan(&started, &finished); err != nil {
		t.Fatal(err)
	}
	requireDetailFields(t, detail, map[string]any{
		"id": id, "taskId": created.ID, "seq": float64(1), "mode": "continue", "status": "succeeded",
		"runnerId": pin.Runner, "role": nil, "harnessId": pin.Harness, "profileId": pin.Profile,
		"profileVersion": profiles[0].Version, "profileCommitSha": profiles[0].CommitSHA,
		"errorClass": nil, "startedAt": started, "finishedAt": finished,
		"contextRevisions": wantContexts,
	})
	// Execution fields common to the task detail must retain the same names,
	// values and null semantics.
	history := requireTaskDetail(t, f, created.ID)["executions"].([]any)
	for field, value := range history[0].(map[string]any) {
		requireDetailFields(t, detail, map[string]any{field: value})
	}
}

func TestGetExecutionPendingReturnsNullPinsAndEmptyCollections(t *testing.T) {
	f := taskFixture(t)
	id := pendingDetailExecution(t, f)
	detail := requireExecutionDetail(t, f, id)
	requireDetailFields(t, detail, map[string]any{
		"id": id, "seq": float64(1), "mode": "continue", "status": "pending",
		"runnerId": nil, "role": nil, "harnessId": nil, "profileId": nil,
		"profileVersion": nil, "profileCommitSha": nil,
		"startedAt": nil, "finishedAt": nil, "errorClass": nil,
		"contextRevisions": []any{},
	})
}

func TestGetExecutionReturnsStoredError(t *testing.T) {
	f := taskFixture(t)
	id := pendingDetailExecution(t, f)
	if _, err := f.db.Exec(`UPDATE executions SET status = 'failed', error_class = 'check_failed',
		started_at = '2026-10-28T01:00:00.123456+00:00', finished_at = '2026-10-28T01:02:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	detail := requireExecutionDetail(t, f, id)
	requireDetailFields(t, detail, map[string]any{
		"status": "failed", "errorClass": "check_failed", "startedAt": "2026-10-28T01:00:00.123456Z",
		"finishedAt": "2026-10-28T01:02:00Z",
	})
}

func TestGetExecutionMissingReturns404NotFound(t *testing.T) {
	f := taskFixture(t)
	status, body := f.request(t, "GET", "/api/executions/"+ulid.Make().String(), true, "", nil)
	assertAuthError(t, status, body, 404, NotFound)
}

func TestGetExecutionRequiresOwnerSession(t *testing.T) {
	f, token, _ := claimFixture(t)
	id := pendingDetailExecution(t, f)
	for _, executionID := range []string{id, ulid.Make().String(), "invalid"} {
		status, body := f.request(t, "GET", "/api/executions/"+executionID, false, "", nil)
		assertAuthError(t, status, body, 401, Unauthenticated)
	}
	status, body := f.request(t, "GET", "/api/executions/"+id, false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	assertAuthError(t, status, body, 403, Forbidden)
}

func TestGetExecutionRejectsMalformedID(t *testing.T) {
	f := taskFixture(t)
	status, body := f.request(t, "GET", "/api/executions/invalid", true, "", nil)
	requireProjectError(t, status, body, 400, ValidationFailed, "id", "invalid_format")
}

func TestGetExecutionStorageFailureReturns500(t *testing.T) {
	f := taskFixture(t)
	id := pendingDetailExecution(t, f)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	status, body := f.request(t, "GET", "/api/executions/"+id, true, "", nil)
	assertAuthError(t, status, body, 500, Internal)
}
