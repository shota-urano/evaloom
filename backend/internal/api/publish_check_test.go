package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

func postPublishCheck(t *testing.T, f *authFixture, token, id, body string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/executions/"+id+"/publish-check", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(body))
	})
}

func publishCheckOf(t *testing.T, f *authFixture, token, id string) publishCheckResponse {
	t.Helper()
	status, body := postPublishCheck(t, f, token, id, `{}`)
	if status != 200 {
		t.Fatalf("publish-check = %d %s; want 200", status, body)
	}
	var out publishCheckResponse
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("invalid publish-check JSON %s: %v", body, err)
	}
	return out
}

func setPolicy(t *testing.T, f *authFixture, policy string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE projects SET approval_policy = ? WHERE id = 'project-a'`, policy); err != nil {
		t.Fatal(err)
	}
}

// Spec 17 3.7.1 steps 1 and 2: publish-check returns the current approval
// policy at the time of the call and the cancellation request. Unknown or
// unreadable stored policies are the default require_approval, so they never
// allow a direct push.
func TestPublishCheckReturnsCurrentPolicyAndCancellation(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	countSideEffects := func() int {
		return stageCount(t, f, `SELECT (SELECT count(*) FROM audit_logs) + (SELECT count(*) FROM execution_events)`)
	}
	before := countSideEffects()
	want := func(cancel bool, policy string) publishCheckResponse {
		return publishCheckResponse{CancelRequested: cancel, ApprovalPolicy: publishCheckPolicy{Version: 1, MergePullRequest: policy}}
	}
	for _, c := range []struct{ stored, want string }{
		{`{"mergePullRequest":"require_approval","version":1}`, "require_approval"},
		{`{"mergePullRequest":"direct_push","version":1}`, "direct_push"},
		{`{"mergePullRequest":"direct_push","version":2}`, "require_approval"},
		{`{"mergePullRequest":"DIRECT_PUSH","version":1}`, "require_approval"},
		{`{"version":1}`, "require_approval"},
		{`not json`, "require_approval"},
	} {
		setPolicy(t, f, c.stored)
		if got := publishCheckOf(t, f, token, id); got != want(false, c.want) {
			t.Fatalf("policy %s: publish-check = %+v; want %s", c.stored, got, c.want)
		}
	}
	setPolicy(t, f, `{"mergePullRequest":"direct_push","version":1}`)
	if _, err := f.db.Exec(`UPDATE executions SET cancel_requested_at = '2026-11-08T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if got := publishCheckOf(t, f, token, id); got != want(true, "direct_push") {
		t.Fatalf("publish-check = %+v; want cancellation requested", got)
	}
	// The check changes no state: no events, audit records or transitions.
	if after := countSideEffects(); after != before {
		t.Fatalf("audit records and events %d -> %d", before, after)
	}
	if execution, _, _ := executionAndTask(t, f, id); execution != "publishing" {
		t.Fatalf("execution = %s", execution)
	}
}

// Only the Runner of the latest publishing Execution may check it. The
// owner's session and other Runners get 403 (audited), other states 409.
func TestPublishCheckRejectsWrongCallerAndState(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	status, body := f.request(t, "POST", "/api/executions/"+id+"/publish-check", true, f.csrf, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{}`))
	})
	assertAuthError(t, status, body, 403, Forbidden)
	other, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, body = postPublishCheck(t, f, other, id, `{}`)
	assertAuthError(t, status, body, 403, Forbidden)
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'
		AND json_extract(detail, '$.reason') = 'execution_runner_mismatch'`); n != 1 {
		t.Fatalf("runner mismatch audits = %d", n)
	}
	status, body = postPublishCheck(t, f, token, ulid.Make().String(), `{}`)
	assertAuthError(t, status, body, 404, NotFound)
	status, body = postPublishCheck(t, f, token, "not-an-id", `{}`)
	assertAuthError(t, status, body, 400, ValidationFailed)
	status, body = postPublishCheck(t, f, token, id, `{"policy":"direct_push"}`)
	assertAuthError(t, status, body, 400, ValidationFailed)

	verifying, verifyingToken, verifyingID, _ := resultFixture(t, "verifying")
	status, body = postPublishCheck(t, verifying, verifyingToken, verifyingID, `{}`)
	assertAuthError(t, status, body, 409, Conflict)
}
