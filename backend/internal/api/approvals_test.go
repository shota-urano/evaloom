package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitingApprovalFixture puts the Execution in waiting_approval with one
// pending approval bound to sha.
func waitingApprovalFixture(t *testing.T) (f *authFixture, token, executionID, taskID string, approval approvalRow) {
	t.Helper()
	f, token, executionID, _ = resultFixture(t, "publishing")
	taskID = executionTaskID(t, f, executionID)
	openApproval(t, f, executionID, strings.Repeat("b", 40))
	requireWaitingApproval(t, f, executionID)
	rows := approvalsOf(t, f, taskID)
	if len(rows) != 1 {
		t.Fatalf("approvals = %+v; want one pending", rows)
	}
	return f, token, executionID, taskID, rows[0]
}

func approvalRequest(t *testing.T, f *authFixture, method, path, body string) (int, []byte) {
	t.Helper()
	return f.request(t, method, path, true, f.csrf, func(r *http.Request) {
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
			r.Body = io.NopCloser(strings.NewReader(body))
		}
	})
}

func decide(t *testing.T, f *authFixture, id string, body map[string]any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return approvalRequest(t, f, "POST", "/api/approvals/"+id+"/decision", string(encoded))
}

func decodeApproval(t *testing.T, status int, body []byte, want int) approvalDetailView {
	t.Helper()
	if status != want {
		t.Fatalf("status = %d %s; want %d", status, body, want)
	}
	var view approvalDetailView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("invalid approval JSON %s: %v", body, err)
	}
	return view
}

func executionAndTask(t *testing.T, f *authFixture, id string) (execution, taskStatus, message string) {
	t.Helper()
	if err := f.db.QueryRow(`SELECT e.status, t.status, coalesce(e.error_message, '') FROM executions e
		JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(&execution, &taskStatus, &message); err != nil {
		t.Fatal(err)
	}
	return
}

type decisionAudit struct {
	Actor, Decision, ContentVersion, ExecutionID, ExpiresAt string
}

func decisionAudits(t *testing.T, f *authFixture, approvalID string) []decisionAudit {
	t.Helper()
	rows, err := f.db.Query(`SELECT actor, json_extract(detail,'$.decision'), json_extract(detail,'$.contentVersion'),
		json_extract(detail,'$.executionId'), coalesce(json_extract(detail,'$.expiresAt'), '')
		FROM audit_logs WHERE action='approval.decide' AND target_type='approval' AND target_id=? ORDER BY id`, approvalID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []decisionAudit
	for rows.Next() {
		var a decisionAudit
		if err := rows.Scan(&a.Actor, &a.Decision, &a.ContentVersion, &a.ExecutionID, &a.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// Acceptance: a contentVersion that differs from the row's is 422 and
// changes nothing.
func TestApprovalDecisionWithStaleContentVersionIs422(t *testing.T) {
	f, _, id, taskID, approval := waitingApprovalFixture(t)
	for _, decision := range []string{"approve", "reject"} {
		status, body := decide(t, f, approval.ID, map[string]any{"decision": decision, "contentVersion": strings.Repeat("c", 40)})
		assertAuthError(t, status, body, 422, PreconditionFailed)
	}
	if got := approvalsOf(t, f, taskID); len(got) != 1 || got[0] != approval {
		t.Fatalf("approvals changed: %+v", got)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "waiting_approval" || taskStatus != "waiting_approval" {
		t.Fatalf("state = %s/%s; want waiting_approval", execution, taskStatus)
	}
	if audits := decisionAudits(t, f, approval.ID); len(audits) != 0 {
		t.Fatalf("audits = %+v; want none", audits)
	}
}

// Approval records the owner's decision with the requested validity, moves
// the Execution to merging (3.5.1) and audits the decision. A second
// decision on the decided approval is 409 and changes nothing.
func TestApproveRecordsDecisionAndRejectsSecondDecision(t *testing.T) {
	f, _, id, taskID, approval := waitingApprovalFixture(t)
	before := time.Now().UTC()
	status, body := decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion, "ttlSec": 3600})
	view := decodeApproval(t, status, body, 200)
	after := time.Now().UTC()
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions d JOIN approvals a ON a.decision_id=d.id
		WHERE a.id=? AND d.status='answered' AND json_extract(d.answer,'$.action')='approve'
		AND json_extract(d.answer,'$.contentId')=a.content_id AND d.answered_at=a.decided_at`, approval.ID); n != 1 {
		t.Fatalf("answered decisions = %d; want 1", n)
	}
	if view.ID != approval.ID || view.Status != "approved" || view.DecidedBy == nil || *view.DecidedBy != "owner" ||
		view.DecidedAt == nil || view.ExpiresAt == nil || view.ContentVersion != approval.ContentVersion || view.UseCount != 0 {
		t.Fatalf("approval = %+v; want approved by owner", view)
	}
	if exp := view.ExpiresAt.Time; exp.Before(before.Add(time.Hour)) || exp.After(after.Add(time.Hour)) {
		t.Fatalf("expiresAt = %v; want decision time + 3600s", exp)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "pending" || taskStatus != "queued" {
		t.Fatalf("state = %s/%s; want merging/waiting_approval", execution, taskStatus)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='status_changed'
		AND json_extract(payload,'$.from')='waiting_approval' AND json_extract(payload,'$.to')='pending'
		AND json_extract(payload,'$.reason')='approval_approved'`, id); n != 1 {
		t.Fatalf("approval events = %d; want 1", n)
	}
	audits := decisionAudits(t, f, approval.ID)
	if len(audits) != 1 || audits[0] != (decisionAudit{Actor: "owner", Decision: "approve", ContentVersion: approval.ContentVersion,
		ExecutionID: id, ExpiresAt: *approvalsExpiresAt(t, f, approval.ID)}) {
		t.Fatalf("audits = %+v; want one owner approve", audits)
	}

	stored := approvalsOf(t, f, taskID)
	for _, decision := range []string{"approve", "reject"} {
		status, body = decide(t, f, approval.ID, map[string]any{"decision": decision, "contentVersion": approval.ContentVersion})
		assertAuthError(t, status, body, 409, Conflict)
	}
	if got := approvalsOf(t, f, taskID); len(got) != 1 || got[0] != stored[0] {
		t.Fatalf("second decision changed approvals: %+v", got)
	}
	if audits := decisionAudits(t, f, approval.ID); len(audits) != 1 {
		t.Fatalf("audits = %+v; want only the first decision", audits)
	}
	if execution, _, _ := executionAndTask(t, f, id); execution != "pending" {
		t.Fatalf("execution = %s; want merging", execution)
	}
}

func approvalsExpiresAt(t *testing.T, f *authFixture, id string) *string {
	t.Helper()
	var value *string
	if err := f.db.QueryRow(`SELECT expires_at FROM approvals WHERE id=?`, id).Scan(&value); err != nil || value == nil {
		t.Fatalf("expires_at = %v err=%v", value, err)
	}
	return value
}

// Q55: without ttlSec the approval is valid for 24 hours.
func TestApproveDefaultsToTwentyFourHours(t *testing.T) {
	f, _, _, _, approval := waitingApprovalFixture(t)
	before := time.Now().UTC()
	status, body := decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
	view := decodeApproval(t, status, body, 200)
	after := time.Now().UTC()
	if view.ExpiresAt == nil || view.ExpiresAt.Before(before.Add(24*time.Hour)) || view.ExpiresAt.After(after.Add(24*time.Hour)) {
		t.Fatalf("expiresAt = %v; want +24h", view.ExpiresAt)
	}
}

// Decisions on invalidated or expired approvals are 409 as well (5章).
func TestDecisionOnInvalidatedOrExpiredApprovalIs409(t *testing.T) {
	for _, update := range []string{
		`UPDATE approvals SET status='invalidated', invalidated_reason='superseded' WHERE id=?`,
		`UPDATE approvals SET status='expired', decided_at='2026-11-01T00:00:00Z', expires_at='2026-11-02T00:00:00Z' WHERE id=?`,
		`UPDATE approvals SET status='rejected', decided_at='2026-11-01T00:00:00Z' WHERE id=?`,
	} {
		f, _, _, _, approval := waitingApprovalFixture(t)
		stageExec(t, f, update, approval.ID)
		status, body := decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
		assertAuthError(t, status, body, 409, Conflict)
		if audits := decisionAudits(t, f, approval.ID); len(audits) != 0 {
			t.Fatalf("audits = %+v; want none", audits)
		}
	}
}

// Acceptance (Q57): rejection fails the task with reason approval_rejected,
// audits the decision, and the owner can continue the task.
func TestRejectFailsTaskWithApprovalRejected(t *testing.T) {
	f, _, id, taskID, approval := waitingApprovalFixture(t)
	status, body := decide(t, f, approval.ID, map[string]any{"decision": "reject", "contentVersion": approval.ContentVersion})
	view := decodeApproval(t, status, body, 200)
	if view.Status != "rejected" || view.DecidedBy == nil || *view.DecidedBy != "owner" || view.DecidedAt == nil || view.ExpiresAt != nil {
		t.Fatalf("approval = %+v; want rejected by owner without expiry", view)
	}
	execution, taskStatus, message := executionAndTask(t, f, id)
	if execution != "failed" || taskStatus != "failed" || message != "approval_rejected" {
		t.Fatalf("state = %s/%s/%q; want failed with approval_rejected", execution, taskStatus, message)
	}
	var finished bool
	if err := f.db.QueryRow(`SELECT finished_at IS NOT NULL FROM executions WHERE id=?`, id).Scan(&finished); err != nil || !finished {
		t.Fatalf("finished=%v err=%v; want finished", finished, err)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='status_changed'
		AND json_extract(payload,'$.to')='failed' AND json_extract(payload,'$.reason')='approval_rejected'`, id); n != 1 {
		t.Fatalf("rejection events = %d; want 1", n)
	}
	audits := decisionAudits(t, f, approval.ID)
	if len(audits) != 1 || audits[0] != (decisionAudit{Actor: "owner", Decision: "reject", ContentVersion: approval.ContentVersion, ExecutionID: id}) {
		t.Fatalf("audits = %+v; want one owner reject", audits)
	}
	status, body = decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
	assertAuthError(t, status, body, 409, Conflict)

	status, body = requestRetry(t, f, taskID, `{"mode":"continue"}`, nil)
	requireRetrySuccess(t, status, body)
}

// Input validation: unknown decisions, missing or malformed contentVersion
// and ttlSec outside approve are 400 without changes.
func TestApprovalDecisionValidation(t *testing.T) {
	f, _, _, taskID, approval := waitingApprovalFixture(t)
	sha := approval.ContentVersion
	for _, body := range []string{
		`{"decision":"maybe","contentVersion":"` + sha + `"}`,
		`{"contentVersion":"` + sha + `"}`,
		`{"decision":"approve"}`,
		`{"decision":"approve","contentVersion":"abc"}`,
		`{"decision":"approve","contentVersion":"` + sha + `","ttlSec":0}`,
		`{"decision":"reject","contentVersion":"` + sha + `","ttlSec":60}`,
		`{"decision":"approve","contentVersion":"` + sha + `","extra":1}`,
	} {
		status, out := approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/decision", body)
		assertAuthError(t, status, out, 400, ValidationFailed)
	}
	status, out := decide(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", map[string]any{"decision": "approve", "contentVersion": sha})
	assertAuthError(t, status, out, 404, NotFound)
	if got := approvalsOf(t, f, taskID); len(got) != 1 || got[0] != approval {
		t.Fatalf("approvals changed: %+v", got)
	}
}

// 6章: a Runner token cannot decide; the request is 403 and audited.
func TestRunnerTokenCannotDecideApproval(t *testing.T) {
	f, token, _, taskID, approval := waitingApprovalFixture(t)
	path := "/api/approvals/" + approval.ID + "/decision"
	status, body := f.request(t, "POST", path, false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(`{"decision":"approve","contentVersion":"` + approval.ContentVersion + `"}`))
	})
	assertAuthError(t, status, body, 403, Forbidden)
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='auth.reject' AND target_id=?
		AND json_extract(detail,'$.reason')='wrong_auth_class'`, path); n != 1 {
		t.Fatalf("auth.reject audits = %d; want 1", n)
	}
	if got := approvalsOf(t, f, taskID); len(got) != 1 || got[0] != approval {
		t.Fatalf("approvals changed: %+v", got)
	}
}

// Acceptance (3.6): renew on an expired approval creates a new pending
// approval with the same binding. Both an approval already marked expired
// and an approved one past expires_at (still merging) are renewable.
func TestRenewExpiredApprovalCreatesPending(t *testing.T) {
	for _, tc := range []struct {
		name, approvalUpdate, executionStatus string
	}{
		{"marked expired", `UPDATE approvals SET status='expired', decided_at='2026-11-01T00:00:00Z',
			expires_at='2026-11-02T00:00:00Z' WHERE id=?`, "waiting_approval"},
		{"approved past expiry", `UPDATE approvals SET expires_at='2026-11-02T00:00:00Z' WHERE id=?`, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, id, taskID, approval := waitingApprovalFixture(t)
			if tc.executionStatus == "pending" {
				status, body := decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
				decodeApproval(t, status, body, 200)
			}
			stageExec(t, f, tc.approvalUpdate, approval.ID)
			if execution, _, _ := executionAndTask(t, f, id); execution != tc.executionStatus {
				t.Fatalf("setup execution = %s; want %s", execution, tc.executionStatus)
			}

			status, body := approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/renew", "")
			view := decodeApproval(t, status, body, 201)
			if view.ID == approval.ID || view.Status != "pending" || view.ContentVersion != approval.ContentVersion {
				t.Fatalf("renewed = %+v; want a new pending approval", view)
			}
			rows := approvalsOf(t, f, taskID)
			byID := map[string]approvalRow{}
			for _, r := range rows {
				byID[r.ID] = r
			}
			if len(rows) != 2 || byID[approval.ID].Status != "expired" {
				t.Fatalf("approvals = %+v; want the expired one and a new pending one", rows)
			}
			fresh := byID[view.ID]
			requirePendingApproval(t, fresh, taskID, id, approval.ContentVersion)
			if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions d JOIN approvals a ON a.decision_id=d.id
				WHERE a.id=? AND d.status='open' AND d.subject_id=a.id AND d.basis=a.content_id`, view.ID); n != 1 {
				t.Fatalf("renewed open decisions = %d; want 1", n)
			}
			if fresh.ParamsHash != approval.ParamsHash || fresh.Target != approval.Target {
				t.Fatalf("renewed binding = %+v; want %+v", fresh, approval)
			}
			if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "waiting_approval" || taskStatus != "waiting_approval" {
				t.Fatalf("state = %s/%s; want waiting_approval", execution, taskStatus)
			}
			var actor, renewed, previous string
			if err := f.db.QueryRow(`SELECT actor, json_extract(detail,'$.renewedApprovalId'), json_extract(detail,'$.previousStatus')
				FROM audit_logs WHERE action='approval.renew' AND target_id=?`, approval.ID).Scan(&actor, &renewed, &previous); err != nil ||
				actor != "owner" || renewed != view.ID {
				t.Fatalf("renew audit = %s/%s/%s err=%v", actor, renewed, previous, err)
			}

			// The new pending approval can be decided; the expired one cannot be renewed twice.
			status, body = approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/renew", "")
			assertAuthError(t, status, body, 409, Conflict)
			status, body = decide(t, f, view.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
			if got := decodeApproval(t, status, body, 200); got.Status != "approved" {
				t.Fatalf("renewed approval decision = %+v", got)
			}
		})
	}
}

// renew is only for expired approvals: pending, unexpired approved and
// rejected approvals are 409 without changes.
func TestRenewNonExpiredApprovalIs409(t *testing.T) {
	f, _, _, taskID, approval := waitingApprovalFixture(t)
	status, body := approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/renew", "")
	assertAuthError(t, status, body, 409, Conflict)
	status, body = decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
	decodeApproval(t, status, body, 200)
	status, body = approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/renew", "")
	assertAuthError(t, status, body, 409, Conflict)
	if got := approvalsOf(t, f, taskID); len(got) != 1 || got[0].Status != "approved" {
		t.Fatalf("approvals = %+v; want only the approved one", got)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='approval.renew'`); n != 0 {
		t.Fatalf("renew audits = %d; want 0", n)
	}
}

// List filters by status and pages; detail shows the target and the content
// version.
func TestListAndGetApprovals(t *testing.T) {
	f, _, id, _, approval := waitingApprovalFixture(t)
	stageExec(t, f, `INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target, content_id, params_hash,
		status, decided_at, created_at) SELECT '00000000000000000000000001', task_id, execution_id, decision_id, requested_stage, operation, target,
		content_id, params_hash, 'rejected', '2026-11-07T00:00:00Z', '2026-11-07T00:00:00Z' FROM approvals WHERE id=?`, approval.ID)

	status, body := approvalRequest(t, f, "GET", "/api/approvals?status=pending", "")
	var page Page[approvalView]
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].ID != approval.ID ||
		page.Items[0].Status != "pending" || page.NextCursor != "" {
		t.Fatalf("pending list = %d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/approvals?limit=1", "")
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].ID != approval.ID || page.NextCursor == "" {
		t.Fatalf("first page = %d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/approvals?limit=1&cursor="+page.NextCursor, "")
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].ID != "00000000000000000000000001" ||
		page.Items[0].Status != "rejected" || page.NextCursor != "" {
		t.Fatalf("second page = %d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/approvals?status=unknown", "")
	assertAuthError(t, status, body, 400, ValidationFailed)

	status, body = approvalRequest(t, f, "GET", "/api/approvals/"+approval.ID, "")
	view := decodeApproval(t, status, body, 200)
	var target map[string]any
	if err := json.Unmarshal(view.Target, &target); err != nil || target["name"] != "example" {
		t.Fatalf("target = %s err=%v", view.Target, err)
	}
	if view.ContentVersion != approval.ContentVersion || view.ExecutionID != id {
		t.Fatalf("detail = %s", body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/approvals/01ARZ3NDEKTSV4RRFFQ69G5FAV", "")
	assertAuthError(t, status, body, 404, NotFound)
}
