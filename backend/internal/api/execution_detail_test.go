package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/store"
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

func TestGetExecutionReturnsPinnedVersionContextsGateResultsAndPRURL(t *testing.T) {
	f, token, profiles := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "execution-detail-pins")
	status, body := requestClaim(t, f, token)
	id := requireClaim(t, status, body).Execution.ID
	pin := readExecutionPin(t, f, id)
	from := "claimed"
	prURL := "https://example.com/pr/42"
	for _, next := range []string{"preparing", "running", "verifying", "publishing", "succeeded"} {
		input := resultBody(from, next)
		if next == "succeeded" {
			input["prUrl"] = prURL
		}
		status, body = postResult(t, f, token, id, input)
		if status != 204 {
			t.Fatalf("report %s: %d %s", next, status, body)
		}
		from = next
	}
	// A new assignment and a newer Execution must not change historical pins
	// or leak the newer Execution's Context and Gate results into this one.
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
	requireAssignment(t, status, body, 200)
	status, body = requestRetry(t, f, created.ID, `{"mode":"fresh"}`, nil)
	requireRetrySuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	otherID := requireClaim(t, status, body).Execution.ID

	const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const targetSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const recordedAt = "2026-10-28T02:03:04.123456Z"
	var wantContexts []any
	for _, source := range []struct{ layer, kind, path string }{
		{"global", "global_policy", "context/policy.md"},
		{"harness", "harness_prompt", "prompts/developer.md"},
		{"project", "agents_md", "AGENTS.md"},
		{"project", "agents_md", "backend/AGENTS.md"},
		{"task", "task_input", "task.json"},
	} {
		revisionID := ulid.Make().String()
		content := "context-body-must-not-be-returned:" + source.path
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		if _, err := f.db.Exec(`INSERT INTO context_revisions
			(id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at)
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
		(id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at)
		VALUES (?, ?, 'project', 'agents_md', 'other.md', ?, 'other-hash', 'other-content', ?)`,
		ulid.Make().String(), otherID, targetSHA, recordedAt); err != nil {
		t.Fatal(err)
	}

	var wantGates []any
	for i, result := range []string{"pass", "blocked", "failed"} {
		gateID := ulid.Make().String()
		findings := []store.Finding{}
		wantFindings := []any{}
		if i == 1 {
			findings = append(findings, store.Finding{
				Rule: "existing_test_deleted", Path: "backend/foo_test.go", Line: 42, Message: "既存テストが削除された",
			})
			wantFindings = append(wantFindings, map[string]any{
				"rule": "existing_test_deleted", "path": "backend/foo_test.go", "line": float64(42),
				"message": "既存テストが削除された",
			})
		}
		if err := store.RecordGateResult(context.Background(), f.db, store.GateResult{
			ID: gateID, ExecutionID: id, Gate: i + 1, Result: result,
			BaseSHA: baseSHA, TargetSHA: targetSHA, Findings: findings,
		}); err != nil {
			t.Fatal(err)
		}
		var gateCreatedAt string
		if err := f.db.QueryRow(`SELECT created_at FROM gate_results WHERE id = ?`, gateID).Scan(&gateCreatedAt); err != nil {
			t.Fatal(err)
		}
		wantGates = append(wantGates, map[string]any{
			"id": gateID, "gate": float64(i + 1), "result": result, "baseSha": baseSHA,
			"targetSha": targetSHA, "findings": wantFindings, "createdAt": gateCreatedAt,
		})
	}
	if err := store.RecordGateResult(context.Background(), f.db, store.GateResult{
		ID: ulid.Make().String(), ExecutionID: otherID, Gate: 1, Result: "pass", BaseSHA: targetSHA, TargetSHA: targetSHA,
	}); err != nil {
		t.Fatal(err)
	}

	detail := requireExecutionDetail(t, f, id)
	var started, finished string
	if err := f.db.QueryRow(`SELECT started_at, finished_at FROM executions WHERE id = ?`, id).Scan(&started, &finished); err != nil {
		t.Fatal(err)
	}
	requireDetailFields(t, detail, map[string]any{
		"id": id, "taskId": created.ID, "seq": float64(1), "mode": "continue", "status": "succeeded",
		"runnerId": pin.Runner, "role": pin.Role, "harnessId": pin.Harness, "profileId": pin.Profile,
		"profileVersion": profiles[0].Version, "profileCommitSha": profiles[0].CommitSHA,
		"errorClass": nil, "startedAt": started, "finishedAt": finished, "prUrl": prURL,
		"contextRevisions": wantContexts, "gateResults": wantGates,
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
		"profileVersion": nil, "profileCommitSha": nil, "prUrl": nil,
		"startedAt": nil, "finishedAt": nil, "errorClass": nil,
		"contextRevisions": []any{}, "gateResults": []any{},
	})
}

func TestGetExecutionReturnsRepeatedGateResultsAndStoredError(t *testing.T) {
	f := taskFixture(t)
	id := pendingDetailExecution(t, f)
	if _, err := f.db.Exec(`UPDATE executions SET status = 'failed', error_class = 'test_failed',
		started_at = '2026-10-28T01:00:00.123456+00:00', finished_at = '2026-10-28T01:02:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, result := range []string{"blocked", "pass"} {
		gateID := ulid.Make().String()
		ids = append(ids, gateID)
		if err := store.RecordGateResult(context.Background(), f.db, store.GateResult{
			ID: gateID, ExecutionID: id, Gate: 3, Result: result, BaseSHA: "base", TargetSHA: "target",
		}); err != nil {
			t.Fatal(err)
		}
	}
	detail := requireExecutionDetail(t, f, id)
	requireDetailFields(t, detail, map[string]any{
		"status": "failed", "errorClass": "test_failed", "startedAt": "2026-10-28T01:00:00.123456Z",
		"finishedAt": "2026-10-28T01:02:00Z",
	})
	gates := detail["gateResults"].([]any)
	if len(gates) != 2 {
		t.Fatalf("gate history=%v", gates)
	}
	for i, result := range []string{"blocked", "pass"} {
		requireDetailFields(t, gates[i].(map[string]any), map[string]any{
			"id": ids[i], "gate": float64(3), "result": result, "findings": []any{},
		})
	}
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

func TestGetExecutionInvalidStoredResultsReturn500(t *testing.T) {
	for _, findings := range []string{"not-json", "null", `{}`, `["bad-finding"]`} {
		t.Run(findings, func(t *testing.T) {
			f := taskFixture(t)
			id := pendingDetailExecution(t, f)
			if _, err := f.db.Exec(`INSERT INTO gate_results
				(id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
				VALUES (?, ?, 1, 'pass', 'base', 'target', ?, '2026-10-28T00:00:00Z')`,
				ulid.Make().String(), id, findings); err != nil {
				t.Fatal(err)
			}
			status, body := f.request(t, "GET", "/api/executions/"+id, true, "", nil)
			assertAuthError(t, status, body, 500, Internal)
			if strings.Contains(string(body), findings) || len(body) == 0 {
				t.Fatalf("invalid internal error response: %s", body)
			}
		})
	}
}
