package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Spec 17 3.5.1 step 2: an approved merge is handed to the Runner as an
// "approved operation" in the claim response. The claim does not change the
// Execution, and once consume used the approval it is no longer offered.
func TestClaimReturnsApprovedOperationUntilConsumed(t *testing.T) {
	f, token, executionID, taskID, approval := approvedFixture(t)
	status, body := requestClaim(t, f, token)
	var claim store.OperationClaim
	if status != 200 || json.Unmarshal(body, &claim) != nil || claim.Operation == nil {
		t.Fatalf("claim = %d %s; want an approved operation", status, body)
	}
	var target store.MergeTarget
	if err := json.Unmarshal(claim.Operation.Target, &target); err != nil {
		t.Fatal(err)
	}
	if claim.Operation.ApprovalID != approval.ID || claim.Operation.Operation != "merge_pull_request" ||
		claim.Operation.ContentVersion != approval.ContentVersion || claim.Execution.ID != executionID ||
		claim.Execution.TaskID != taskID || target.PRNumber < 1 || target.BaseBranch != "main" || claim.Project.ID == "" {
		t.Fatalf("operation claim = %+v target=%+v", claim, target)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, executionID); execution != "merging" || taskStatus != "waiting_approval" {
		t.Fatalf("state after claim = %s/%s; want unchanged merging", execution, taskStatus)
	}
	consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if status, body := requestClaim(t, f, token); status != 204 {
		t.Fatalf("claim after consume = %d %s; want no work", status, body)
	}
}

// An Execution claim carries operation=null.
func TestExecutionClaimHasNullOperation(t *testing.T) {
	f, token, _ := claimFixture(t)
	createClaimTask(t, f, "project-a", "operation-null")
	status, body := requestClaim(t, f, token)
	if status != 200 || !strings.Contains(string(body), `"operation":null`) {
		t.Fatalf("claim = %d %s; want operation null", status, body)
	}
}

func mergeSucceeded(sha string) map[string]any {
	return map[string]any{"fromStatus": "merging", "status": "succeeded", "mergeCommitSha": sha}
}

// Spec 17 3.5.2 step 4: the merge result completes the task and is stored
// in approvals.result. Without a consumed approval no merge was allowed, so
// the report is refused.
func TestMergeResultCompletesTaskOnlyAfterConsume(t *testing.T) {
	f, token, executionID, _, approval := approvedFixture(t)
	merge := strings.Repeat("c", 40)
	if status, body := postResult(t, f, token, executionID, mergeSucceeded(merge)); status != 409 {
		t.Fatalf("result before consume = %d %s; want 409", status, body)
	}
	consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	if status, body := postResult(t, f, token, executionID, map[string]any{"fromStatus": "merging", "status": "succeeded"}); status != 400 {
		t.Fatalf("result without merge SHA = %d %s; want 400", status, body)
	}
	if status, body := postResult(t, f, token, executionID, mergeSucceeded(merge)); status != 204 {
		t.Fatalf("result = %d %s; want 204", status, body)
	}
	if execution, taskStatus, _ := executionAndTask(t, f, executionID); execution != "succeeded" || taskStatus != "completed" {
		t.Fatalf("state = %s/%s; want succeeded/completed", execution, taskStatus)
	}
	var result string
	if err := f.db.QueryRow(`SELECT json_extract(result, '$.mergeCommitSha') FROM approvals WHERE id = ?`, approval.ID).Scan(&result); err != nil || result != merge {
		t.Fatalf("approval result = %q %v", result, err)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id = ? AND status = 'succeeded'`, executionID); n != 1 {
		t.Fatalf("succeeded notifications = %d; want 1", n)
	}
	if status, body := postResult(t, f, token, executionID, mergeSucceeded(merge)); status != 409 {
		t.Fatalf("second result = %d %s; want 409", status, body)
	}
}

// Spec 17 6章: GitHub refusing the merge because the head changed fails the
// task and invalidates the consumed approval (content_changed).
func TestMergeRefusedForChangedHeadInvalidatesApproval(t *testing.T) {
	f, token, executionID, _, approval := approvedFixture(t)
	consume(t, f, token, approval.ID, prState(approval.ContentVersion))
	bad := map[string]any{"fromStatus": "merging", "status": "failed", "errorClass": "publish_failed", "errorMessage": "other"}
	if status, body := postResult(t, f, token, executionID, bad); status != 400 {
		t.Fatalf("unknown reason = %d %s; want 400", status, body)
	}
	bad["errorMessage"] = "content_changed"
	if status, body := postResult(t, f, token, executionID, bad); status != 204 {
		t.Fatalf("result = %d %s; want 204", status, body)
	}
	if execution, taskStatus, message := executionAndTask(t, f, executionID); execution != "failed" || taskStatus != "failed" || message != "content_changed" {
		t.Fatalf("state = %s/%s %s; want failed content_changed", execution, taskStatus, message)
	}
	if row := consumedState(t, f, approval.ID); row != (consumedRow{Status: "invalidated", Reason: "content_changed", UseCount: 1, Consumed: true}) {
		t.Fatalf("approval = %+v; want invalidated content_changed", row)
	}
	if n := invalidationAudits(t, f, approval.ID, "content_changed"); n != 1 {
		t.Fatalf("invalidation audits = %d; want 1", n)
	}
}
