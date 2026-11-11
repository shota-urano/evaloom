package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

const authorizeContent = "release:r1"

func authorizeFixture(t *testing.T) (*authFixture, string, string, string) {
	t.Helper()
	f, token, executionID, _ := resultFixture(t, "publishing")
	stageExec(t, f, `UPDATE harness_profiles SET definition=json_set(definition,'$.approvals',json(?))
		WHERE id=(SELECT profile_id FROM executions WHERE id=?)`,
		`[{"operation":"merge_pull_request","description":"Merge","waivable":true},{"operation":"send","description":"Send","waivable":false}]`, executionID)
	approvalID := ulid.Make().String()
	insertApproval(t, f, approvalID, executionID, "approved")
	stageExec(t, f, `UPDATE approvals SET content_id=?,decided_at=?,expires_at=? WHERE id=?`, authorizeContent,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), approvalID)
	stageExec(t, f, `UPDATE owner_decisions SET status='answered',answer='{"action":"approve"}',answered_at=? WHERE subject_id=?`, time.Now().UTC().Format(time.RFC3339), approvalID)
	return f, token, executionID, approvalID
}

func authorizeBody(approvalID any) map[string]any {
	return map[string]any{"operation": "merge_pull_request", "approvalId": approvalID, "contentId": authorizeContent}
}

func postAuthorize(t *testing.T, f *authFixture, token, executionID string, input any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return rawAuthorize(t, f, token, executionID, string(data))
}

func rawAuthorize(t *testing.T, f *authFixture, token, executionID, data string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/executions/"+executionID+"/authorize", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Body = io.NopCloser(strings.NewReader(data))
	})
}

func requireAuthorization(t *testing.T, status int, body []byte, allowed bool, reason string) {
	t.Helper()
	var reply contract.AuthorizeResponse
	if status != http.StatusOK || json.Unmarshal(body, &reply) != nil || reply.Allowed != allowed || reply.Reason != reason {
		t.Fatalf("authorize=%d %s; want allowed=%v reason=%q", status, body, allowed, reason)
	}
}

func TestAuthorizeCancellationInvalidatesOnlyUnusedApprovals(t *testing.T) {
	f, token, id, approvalID := authorizeFixture(t)
	otherID := ulid.Make().String()
	insertApproval(t, f, otherID, id, "consumed")
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	status, body = postAuthorize(t, f, token, id, authorizeBody(approvalID))
	requireAuthorization(t, status, body, false, "cancelled")
	if status, reason := approvalStatus(t, f, approvalID); status != "invalidated" || reason != "cancelled" {
		t.Fatalf("approval=%s/%s", status, reason)
	}
	if status, _ := approvalStatus(t, f, otherID); status != "consumed" {
		t.Fatalf("used approval=%s", status)
	}
	input := authorizeBody(nil)
	input["operation"] = "publish"
	status, body = postAuthorize(t, f, token, id, input)
	requireAuthorization(t, status, body, false, "cancelled")
}

func TestAuthorizeContentMismatchInvalidatesAndDoesNotReapprove(t *testing.T) {
	f, token, id, approvalID := authorizeFixture(t)
	input := authorizeBody(approvalID)
	input["contentId"] = "release:R1" // opaque, case-sensitive comparison
	status, body := postAuthorize(t, f, token, id, input)
	requireAuthorization(t, status, body, false, "content_changed")
	if status, reason := approvalStatus(t, f, approvalID); status != "invalidated" || reason != "content_changed" {
		t.Fatalf("approval=%s/%s", status, reason)
	}
	status, body = postAuthorize(t, f, token, id, authorizeBody(approvalID))
	requireAuthorization(t, status, body, false, "not_approved")
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE subject_id=? AND status='answered'`, approvalID); n != 1 {
		t.Fatal("content mismatch changed the answered decision")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='approval.invalidate' AND target_id=? AND actor='system'`, approvalID); n != 1 {
		t.Fatal("missing invalidation audit")
	}
}

func TestAuthorizeConcurrentRequestsConsumeExactlyOnce(t *testing.T) {
	f, token, id, approvalID := authorizeFixture(t)
	type response struct {
		status int
		body   []byte
	}
	start, results := make(chan struct{}), make(chan response, 2)
	for range 2 {
		go func() {
			<-start
			status, body := postAuthorize(t, f, token, id, authorizeBody(approvalID))
			results <- response{status, body}
		}()
	}
	close(start)
	allowed := 0
	for range 2 {
		r := <-results
		var reply contract.AuthorizeResponse
		if r.status != 200 || json.Unmarshal(r.body, &reply) != nil {
			t.Fatalf("authorize=%d %s", r.status, r.body)
		}
		if reply.Allowed {
			allowed++
		} else if reply.Reason != "already_used" {
			t.Fatalf("denial=%+v", reply)
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed=%d; want 1", allowed)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM approvals WHERE id=? AND status='consumed' AND use_count=1 AND consumed_at IS NOT NULL`, approvalID); n != 1 {
		t.Fatal("approval not consumed exactly once")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='approval_consumed'`, id); n != 2 {
		t.Fatalf("events=%d", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='approval.consume' AND target_id=? AND actor='runner:claim-runner'`, id); n != 2 {
		t.Fatalf("audits=%d", n)
	}
	for _, table := range []string{"execution_events", "audit_logs"} {
		query := `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='approval_consumed'
			AND json_extract(payload,'$.allowed')=1 AND json_extract(payload,'$.approvalId')=?`
		if table == "audit_logs" {
			query = `SELECT count(*) FROM audit_logs WHERE target_id=? AND action='approval.consume'
				AND json_extract(detail,'$.allowed')=1 AND json_extract(detail,'$.approvalId')=?`
		}
		if n := stageCount(t, f, query, id, approvalID); n != 1 {
			t.Fatalf("%s successful consume records=%d; want 1", table, n)
		}
	}
}

func TestAuthorizeDenialCases(t *testing.T) {
	for _, test := range []struct {
		name, update, reason, state string
	}{
		{"pending", `UPDATE approvals SET status='pending' WHERE id=?`, "not_approved", "pending"},
		{"rejected", `UPDATE approvals SET status='rejected' WHERE id=?`, "not_approved", "rejected"},
		{"invalidated", `UPDATE approvals SET status='invalidated',invalidated_reason='superseded' WHERE id=?`, "not_approved", "invalidated"},
		{"consumed", `UPDATE approvals SET status='consumed',use_count=1 WHERE id=?`, "already_used", "consumed"},
		{"uses exhausted", `UPDATE approvals SET use_count=1 WHERE id=?`, "already_used", "approved"},
		{"expired", `UPDATE approvals SET expires_at='2000-01-01T00:00:00Z' WHERE id=?`, "expired", "expired"},
		{"other execution", `UPDATE approvals SET execution_id=(SELECT id FROM executions WHERE id<>execution_id LIMIT 1) WHERE id=?`, "not_approved", "approved"},
		{"other operation", `UPDATE approvals SET operation='send' WHERE id=?`, "not_approved", "approved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, token, id, approvalID := authorizeFixture(t)
			if test.name == "other execution" {
				createClaimTask(t, f, "project-b", "other")
			}
			stageExec(t, f, test.update, approvalID)
			status, body := postAuthorize(t, f, token, id, authorizeBody(approvalID))
			requireAuthorization(t, status, body, false, test.reason)
			if status, _ := approvalStatus(t, f, approvalID); status != test.state {
				t.Fatalf("approval=%s; want %s", status, test.state)
			}
		})
	}
}

func TestAuthorizeUsesPinnedProfileAndCurrentWaivers(t *testing.T) {
	for _, route := range []string{"project", "harness"} {
		t.Run(route, func(t *testing.T) {
			f, token, id, _ := authorizeFixture(t)
			// Changing today's selected Profile must not change the declaration
			// used by this already pinned Execution.
			stageExec(t, f, `UPDATE projects SET profile_id=(SELECT id FROM harness_profiles WHERE harness_id=projects.harness_id AND version='2.0.0')
				WHERE id=(SELECT project_id FROM executions WHERE id=?)`, id)
			if route == "harness" {
				stageExec(t, f, `UPDATE executions SET route_kind='harness',project_id=NULL WHERE id=?`, id)
			}
			query := `UPDATE projects SET approval_waivers=? WHERE id=(SELECT project_id FROM executions WHERE id=?)`
			if route == "harness" {
				query = `UPDATE harnesses SET approval_waivers=? WHERE id=(SELECT harness_id FROM executions WHERE id=?)`
			}
			stageExec(t, f, query, `["merge_pull_request"]`, id)
			status, body := postAuthorize(t, f, token, id, authorizeBody(nil))
			requireAuthorization(t, status, body, true, "")
			stageExec(t, f, query, `[]`, id)
			status, body = postAuthorize(t, f, token, id, authorizeBody(nil))
			requireAuthorization(t, status, body, false, "not_approved")
			for _, operation := range []string{"publish", "undeclared", "send"} {
				stageExec(t, f, query, `["undeclared","send"]`, id)
				input := authorizeBody(nil)
				input["operation"] = operation
				status, body = postAuthorize(t, f, token, id, input)
				reason := "not_approved"
				if operation == "publish" {
					reason = ""
				}
				requireAuthorization(t, status, body, operation == "publish", reason)
			}
		})
	}
}

func TestAuthorizeZeroUpdatedRowsNeverAllows(t *testing.T) {
	f, token, id, approvalID := authorizeFixture(t)
	stageExec(t, f, `CREATE TRIGGER ignore_consume BEFORE UPDATE OF status ON approvals
		WHEN NEW.status='consumed' BEGIN SELECT RAISE(IGNORE); END`)
	status, body := postAuthorize(t, f, token, id, authorizeBody(approvalID))
	requireAuthorization(t, status, body, false, "not_approved")
	if status, _ := approvalStatus(t, f, approvalID); status != "approved" {
		t.Fatal("ignored update changed approval")
	}
}

func TestAuthorizeRejectsAuthenticationOwnershipAndState(t *testing.T) {
	f, token, id, approvalID := authorizeFixture(t)
	input, _ := json.Marshal(authorizeBody(approvalID))
	status, body := f.request(t, "POST", "/api/executions/"+id+"/authorize", true, f.csrf, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(string(input)))
	})
	assertAuthError(t, status, body, 403, Forbidden)
	status, body = rawAuthorize(t, f, "", id, string(input))
	assertAuthError(t, status, body, 401, Unauthenticated)
	otherToken, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, body = rawAuthorize(t, f, otherToken, id, string(input))
	assertAuthError(t, status, body, 403, Forbidden)
	stageExec(t, f, `UPDATE executions SET status='checking' WHERE id=?`, id)
	status, body = rawAuthorize(t, f, token, id, string(input))
	assertAuthError(t, status, body, 409, Conflict)
	status, body = rawAuthorize(t, f, token, ulid.Make().String(), string(input))
	assertAuthError(t, status, body, 404, NotFound)
	if status, _ := approvalStatus(t, f, approvalID); status != "approved" {
		t.Fatal("rejected request changed approval")
	}
}

func TestAuthorizeRejectsMalformedInputWithoutMutation(t *testing.T) {
	f, token, id, approvalID := authorizeFixture(t)
	for _, input := range []string{
		`{}`, `null`, `[]`, `{"operation":"publish","contentId":"r1"}`,
		`{"operation":"publish","approvalId":null,"contentId":null}`,
		`{"operation":"publish","approvalId":"bad","contentId":"r1"}`,
		`{"operation":"UPPER","approvalId":null,"contentId":"r1"}`,
		`{"operation":"publish","approvalId":null,"contentId":""}`,
		`{"operation":"publish","approvalId":null,"contentId":"日本語"}`,
		`{"operation":"publish","approvalId":null,"contentId":"line\n"}`,
		`{"Operation":"publish","approvalId":null,"contentId":"r1"}`,
		`{"operation":"publish","operation":"publish","approvalId":null,"contentId":"r1"}`,
		`{"op":"authorize","operation":"publish","approvalId":null,"contentId":"r1"}`,
		`{"operation":"publish","approvalId":null,"contentId":"r1"} {}`,
	} {
		status, body := rawAuthorize(t, f, token, id, input)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	if status, _ := approvalStatus(t, f, approvalID); status != "approved" {
		t.Fatal("malformed request changed approval")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE type='approval_consumed'`); n != 0 {
		t.Fatal("malformed request created event")
	}
}

func TestAuthorizeStorageFailureRollsBackAndDenies(t *testing.T) {
	for _, trigger := range []string{"BEFORE UPDATE ON approvals", "BEFORE INSERT ON execution_events", "BEFORE INSERT ON audit_logs"} {
		t.Run(trigger, func(t *testing.T) {
			f, token, id, approvalID := authorizeFixture(t)
			stageExec(t, f, `CREATE TRIGGER reject_authorize `+trigger+` BEGIN SELECT RAISE(ABORT,'authorize unavailable'); END`)
			status, body := postAuthorize(t, f, token, id, authorizeBody(approvalID))
			var reply struct {
				Allowed *bool `json:"allowed"`
				Error   Error `json:"error"`
			}
			if status != 500 || json.Unmarshal(body, &reply) != nil || reply.Allowed == nil || *reply.Allowed || reply.Error.Code != Internal || strings.Contains(string(body), "authorize unavailable") {
				t.Fatalf("failure=%d %s", status, body)
			}
			if status, _ := approvalStatus(t, f, approvalID); status != "approved" {
				t.Fatal("failure consumed approval")
			}
			if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE type='approval_consumed'`); n != 0 {
				t.Fatal("failure persisted event")
			}
		})
	}
}
