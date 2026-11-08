package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

// approvedFixture is waitingApprovalFixture followed by the owner's
// approval, so the Execution is merging with one approved approval.
func approvedFixture(t *testing.T) (f *authFixture, token, executionID, taskID string, approval approvalRow) {
	t.Helper()
	f, token, executionID, taskID, approval = waitingApprovalFixture(t)
	status, body := decide(t, f, approval.ID, map[string]any{"decision": "approve", "contentVersion": approval.ContentVersion})
	decodeApproval(t, status, body, 200)
	if execution, taskStatus, _ := executionAndTask(t, f, executionID); execution != "merging" || taskStatus != "waiting_approval" {
		t.Fatalf("state = %s/%s; want merging/waiting_approval", execution, taskStatus)
	}
	return f, token, executionID, taskID, approval
}

// prState is what the Git Publisher read from GitHub (3.5.2 step 1).
func prState(head string) map[string]any {
	return map[string]any{"headSha": head, "baseBranch": "main", "open": true, "merged": false}
}

func postConsume(t *testing.T, f *authFixture, token, id string, input any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, "POST", "/api/approvals/"+id+"/consume", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(string(data)))
	})
}

func consume(t *testing.T, f *authFixture, token, id string, input any) approvalConsumeResponse {
	t.Helper()
	status, body := postConsume(t, f, token, id, input)
	if status != 200 {
		t.Fatalf("consume = %d %s; want 200", status, body)
	}
	var out approvalConsumeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("invalid consume JSON %s: %v", body, err)
	}
	return out
}

type consumedRow struct {
	Status, Reason string
	UseCount       int
	Consumed       bool
}

func consumedState(t *testing.T, f *authFixture, id string) consumedRow {
	t.Helper()
	var r consumedRow
	if err := f.db.QueryRow(`SELECT status, coalesce(invalidated_reason, ''), use_count, consumed_at IS NOT NULL
		FROM approvals WHERE id = ?`, id).Scan(&r.Status, &r.Reason, &r.UseCount, &r.Consumed); err != nil {
		t.Fatal(err)
	}
	return r
}

func consumeAudits(t *testing.T, f *authFixture, id string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT actor || '/' || json_extract(detail, '$.allowed') || '/' || coalesce(json_extract(detail, '$.reason'), '')
		FROM audit_logs WHERE action = 'approval.consume' AND target_type = 'approval' AND target_id = ? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func invalidationAudits(t *testing.T, f *authFixture, id, reason string) int {
	t.Helper()
	return stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE actor = 'system' AND action = 'approval.invalidate'
		AND target_id = ? AND json_extract(detail, '$.reason') = ?`, id, reason)
}

// Table row 1: matching content consumes the approval (use_count=1) and
// allows the operation with the content_version as the merge sha. Table row
// 4: a second consume of the used approval is refused and changes nothing.
func TestConsumeMatchingContentAllowsOnceThenAlreadyUsed(t *testing.T) {
	f, token, id, _, approval := approvedFixture(t)
	got := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if got != (approvalConsumeResponse{Allowed: true, ContentVersion: approval.ContentVersion, MergeMethod: "merge"}) {
		t.Fatalf("consume = %+v; want allowed with content version", got)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "consumed", UseCount: 1, Consumed: true}) {
		t.Fatalf("approval = %+v; want consumed once", row)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "merging" || taskStatus != "waiting_approval" {
		t.Fatalf("state = %s/%s; want merging until the merge result", execution, taskStatus)
	}
	again := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if again != (approvalConsumeResponse{Reason: "already_used"}) {
		t.Fatalf("second consume = %+v; want refused already_used", again)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "consumed", UseCount: 1, Consumed: true}) {
		t.Fatalf("approval after second consume = %+v; want unchanged", row)
	}
	if audits := consumeAudits(t, f, approval.ID); len(audits) != 2 || audits[0] != "runner:claim-runner/1/" ||
		audits[1] != "runner:claim-runner/0/already_used" {
		t.Fatalf("consume audits = %v", audits)
	}
}

// Table row 2 and condition 22: after approval, a different head SHA is
// refused; the approval is invalidated (content_changed) and the Execution
// returns to the approval wait.
func TestConsumeAfterHeadChangeIsContentChanged(t *testing.T) {
	f, token, id, _, approval := approvedFixture(t)
	got := consume(t, f, token, approval.ID, prState(strings.Repeat("e", 40)))
	if got != (approvalConsumeResponse{Reason: "content_changed"}) {
		t.Fatalf("consume = %+v; want refused content_changed", got)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "invalidated", Reason: "content_changed"}) {
		t.Fatalf("approval = %+v; want invalidated content_changed", row)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "waiting_approval" || taskStatus != "waiting_approval" {
		t.Fatalf("state = %s/%s; want waiting_approval", execution, taskStatus)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='status_changed'
		AND json_extract(payload,'$.from')='merging' AND json_extract(payload,'$.to')='waiting_approval'
		AND json_extract(payload,'$.reason')='approval_content_changed'`, id); n != 1 {
		t.Fatalf("return events = %d; want 1", n)
	}
	if n := invalidationAudits(t, f, approval.ID, "content_changed"); n != 1 {
		t.Fatalf("invalidation audits = %d; want 1", n)
	}
	// The refused approval can never be consumed afterwards, even with the
	// original head.
	again := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if again.Allowed || again.Reason != "content_changed" {
		t.Fatalf("consume after invalidation = %+v", again)
	}
}

// A params_hash that the stored binding no longer produces is not used.
func TestConsumeWithMismatchedParamsHashIsContentChanged(t *testing.T) {
	f, token, _, _, approval := approvedFixture(t)
	stageExec(t, f, `UPDATE approvals SET params_hash = ? WHERE id = ?`, strings.Repeat("0", 64), approval.ID)
	got := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if got != (approvalConsumeResponse{Reason: "content_changed"}) {
		t.Fatalf("consume = %+v; want refused content_changed", got)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "invalidated", Reason: "content_changed"}) {
		t.Fatalf("approval = %+v", row)
	}
}

// Table row 3: a changed base branch, a closed PR or a merged PR is
// target_changed.
func TestConsumeAfterTargetChangeIsTargetChanged(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"base branch": func(s map[string]any) { s["baseBranch"] = "develop" },
		"closed":      func(s map[string]any) { s["open"] = false },
		"merged":      func(s map[string]any) { s["open"] = false; s["merged"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f, token, id, _, approval := approvedFixture(t)
			state := prState(approval.ContentVersion)
			change(state)
			got := consume(t, f, token, approval.ID, state)
			if got != (approvalConsumeResponse{Reason: "target_changed"}) {
				t.Fatalf("consume = %+v; want refused target_changed", got)
			}
			if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "invalidated", Reason: "target_changed"}) {
				t.Fatalf("approval = %+v", row)
			}
			if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "waiting_approval" || taskStatus != "waiting_approval" {
				t.Fatalf("state = %s/%s", execution, taskStatus)
			}
			if n := invalidationAudits(t, f, approval.ID, "target_changed"); n != 1 {
				t.Fatalf("invalidation audits = %d", n)
			}
		})
	}
}

// Table row 5: an approval past expires_at is refused and marked expired;
// the Execution returns to the wait so the owner can renew it.
func TestConsumeAfterExpiryIsExpired(t *testing.T) {
	f, token, id, _, approval := approvedFixture(t)
	stageExec(t, f, `UPDATE approvals SET expires_at = '2026-01-01T00:00:00Z' WHERE id = ?`, approval.ID)
	got := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if got != (approvalConsumeResponse{Reason: "expired"}) {
		t.Fatalf("consume = %+v; want refused expired", got)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "expired"}) {
		t.Fatalf("approval = %+v; want expired unused", row)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, id); execution != "waiting_approval" || taskStatus != "waiting_approval" {
		t.Fatalf("state = %s/%s", execution, taskStatus)
	}
	status, body := approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/renew", "")
	decodeApproval(t, status, body, 201)
}

// Table row 6: a cancellation requested while merging is applied at consume:
// the approval is invalidated (cancelled), the Execution and task cancelled.
func TestConsumeWithCancelRequestIsCancelled(t *testing.T) {
	f, token, id, taskID, approval := approvedFixture(t)
	if status, body := requestCancellation(t, f, taskID); status != 204 {
		t.Fatalf("cancel = %d %s", status, body)
	}
	got := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if got != (approvalConsumeResponse{Reason: "cancelled"}) {
		t.Fatalf("consume = %+v; want refused cancelled", got)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "invalidated", Reason: "cancelled"}) {
		t.Fatalf("approval = %+v", row)
	}
	after := cancellationState(t, f, id)
	if after.Execution != "cancelled" || after.Task != "cancelled" || after.ErrorClass != "cancelled" || !after.Finished.Valid {
		t.Fatalf("state = %+v; want cancelled", after)
	}
	if n := invalidationAudits(t, f, approval.ID, "cancelled"); n != 1 {
		t.Fatalf("invalidation audits = %d", n)
	}
}

// An approval that was never approved (pending, rejected) is refused and
// left as it is.
func TestConsumeUnapprovedIsRefusedUnchanged(t *testing.T) {
	f, token, id, _, approval := waitingApprovalFixture(t)
	got := consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if got != (approvalConsumeResponse{Reason: "not_approved"}) {
		t.Fatalf("consume = %+v; want refused not_approved", got)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "pending"}) {
		t.Fatalf("approval = %+v; want pending", row)
	}
	if execution, _, _ := executionAndTask(t, f, id); execution != "waiting_approval" {
		t.Fatalf("execution = %s", execution)
	}
}

// Acceptance: two concurrent consumes of the same approval allow exactly once.
func TestConcurrentConsumeAllowsOnce(t *testing.T) {
	f, token, _, _, approval := approvedFixture(t)
	const callers = 2
	results := make([]approvalConsumeResponse, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			status, body := postConsume(t, f, token, approval.ID, prState(approval.ContentVersion))
			if status != 200 {
				t.Errorf("consume %d = %d %s", i, status, body)
				return
			}
			if err := json.Unmarshal(body, &results[i]); err != nil {
				t.Errorf("consume %d JSON: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	allowed := 0
	for _, r := range results {
		if r.Allowed {
			allowed++
		} else if r.Reason != "already_used" {
			t.Fatalf("refused consume reason = %q; want already_used", r.Reason)
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed = %d of %+v; want exactly 1", allowed, results)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "consumed", UseCount: 1, Consumed: true}) {
		t.Fatalf("approval = %+v; want consumed once", row)
	}
}

// 6章: the owner's session cannot call consume (403, audited), nor can a
// Runner other than the Execution's.
func TestConsumeRejectsOwnerSessionAndOtherRunner(t *testing.T) {
	f, _, _, _, approval := approvedFixture(t)
	status, body := f.request(t, "POST", "/api/approvals/"+approval.ID+"/consume", true, f.csrf, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{}`))
	})
	assertAuthError(t, status, body, 403, Forbidden)
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'
		AND target_id = ? AND json_extract(detail, '$.reason') = 'wrong_auth_class'`, "/api/approvals/"+approval.ID+"/consume"); n != 1 {
		t.Fatalf("owner rejection audits = %d", n)
	}
	other, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, body = postConsume(t, f, other, approval.ID, prState(approval.ContentVersion))
	assertAuthError(t, status, body, 403, Forbidden)
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "approved"}) {
		t.Fatalf("approval = %+v; want approved", row)
	}
}

// The PR state fields are required; malformed input changes nothing.
func TestConsumeValidatesInput(t *testing.T) {
	f, token, _, _, approval := approvedFixture(t)
	for name, change := range map[string]func(map[string]any){
		"no head":     func(s map[string]any) { delete(s, "headSha") },
		"short head":  func(s map[string]any) { s["headSha"] = "abc" },
		"no base":     func(s map[string]any) { s["baseBranch"] = "" },
		"no open":     func(s map[string]any) { delete(s, "open") },
		"no merged":   func(s map[string]any) { delete(s, "merged") },
		"extra field": func(s map[string]any) { s["mergeMethod"] = "squash" },
	} {
		t.Run(name, func(t *testing.T) {
			state := prState(approval.ContentVersion)
			change(state)
			status, body := postConsume(t, f, token, approval.ID, state)
			assertAuthError(t, status, body, 400, ValidationFailed)
		})
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "approved"}) {
		t.Fatalf("approval = %+v; want approved", row)
	}
	status, body := postConsume(t, f, token, "01ARZ3NDEKTSV4RRFFQ69G5FAV", prState(approval.ContentVersion))
	assertAuthError(t, status, body, 404, NotFound)
}
