package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func prMergedOnGitHub(sha string) map[string]any {
	return map[string]any{"fromStatus": "waiting_approval", "status": "succeeded", "mergeCommitSha": sha}
}

func prClosedOnGitHub() map[string]any {
	return map[string]any{"fromStatus": "waiting_approval", "status": "failed", "errorMessage": "pull_request_closed"}
}

// Spec 17 3.6: the heartbeat response lists the PRs of this Runner that wait
// for approval, so that the Runner can read them periodically. A merging
// Execution is not listed (the approved operation reads the PR itself).
func TestHeartbeatListsPullRequestsWaitingForApproval(t *testing.T) {
	f, token, executionID, taskID, approval := waitingApprovalFixture(t)
	status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
	var response struct {
		Waiting []store.WaitingApproval `json:"waitingApprovals"`
	}
	if status != 200 || json.Unmarshal(body, &response) != nil || len(response.Waiting) != 1 {
		t.Fatalf("heartbeat = %d %s; want one waiting PR", status, body)
	}
	item := response.Waiting[0]
	if item.ExecutionID != executionID || item.TaskID != taskID || string(item.Target) != approval.Target ||
		item.Project.ID != "project-a" || item.Project.GitRemoteURL == "" {
		t.Fatalf("waiting = %+v; want the approval's PR", item)
	}
	stageExec(t, f, `UPDATE approvals SET status='approved' WHERE id=?`, approval.ID)
	stageExec(t, f, `UPDATE executions SET status='merging' WHERE id=?`, executionID)
	status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
	if status != 200 || !strings.Contains(string(body), `"waitingApprovals":[]`) {
		t.Fatalf("heartbeat while merging = %d %s; want no waiting PR", status, body)
	}
}

// Q58: a PR merged directly on GitHub while it waited for approval
// invalidates the approval (target_changed) and completes the task.
func TestMergedOnGitHubWhileWaitingCompletesTask(t *testing.T) {
	f, token, executionID, _, approval := waitingApprovalFixture(t)
	merge := strings.Repeat("c", 40)
	if status, body := postResult(t, f, token, executionID, map[string]any{"fromStatus": "waiting_approval", "status": "succeeded"}); status != 400 {
		t.Fatalf("without merge SHA = %d %s; want 400", status, body)
	}
	if status, body := postResult(t, f, token, executionID, prMergedOnGitHub(merge)); status != 204 {
		t.Fatalf("result = %d %s; want 204", status, body)
	}
	if execution, taskStatus, message := executionAndTask(t, f, executionID); execution != "succeeded" || taskStatus != "completed" || message != "" {
		t.Fatalf("state = %s/%s %q; want succeeded/completed", execution, taskStatus, message)
	}
	if row := consumedState(t, f, approval.ID); row.Status != "invalidated" || row.Reason != "target_changed" || row.UseCount != 0 {
		t.Fatalf("approval = %+v; want invalidated target_changed", row)
	}
	if n := invalidationAudits(t, f, approval.ID, "target_changed"); n != 1 {
		t.Fatalf("invalidation audits = %d; want 1", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE actor = 'runner:claim-runner' AND action = 'execution.result'
		AND target_id = ? AND json_extract(detail, '$.mergeCommitSha') = ? AND json_extract(detail, '$.reason') = 'pull_request_merged'`, executionID, merge); n != 1 {
		t.Fatalf("result audits = %d; want 1", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'status_changed'
		AND json_extract(payload, '$.reason') = 'pull_request_merged' AND json_extract(payload, '$.to') = 'succeeded'`, executionID); n != 1 {
		t.Fatalf("status events = %d; want 1", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id = ? AND status = 'succeeded'`, executionID); n != 1 {
		t.Fatalf("succeeded notifications = %d; want 1", n)
	}
	if status, body := postResult(t, f, token, executionID, prMergedOnGitHub(merge)); status != 409 {
		t.Fatalf("second result = %d %s; want 409", status, body)
	}
}

// Q58: a PR closed on GitHub while it waited for approval invalidates the
// approval (target_changed) and fails the task (pull_request_closed).
func TestClosedOnGitHubWhileWaitingFailsTask(t *testing.T) {
	f, token, executionID, _, approval := waitingApprovalFixture(t)
	if status, body := postResult(t, f, token, executionID, prClosedOnGitHub()); status != 204 {
		t.Fatalf("result = %d %s; want 204", status, body)
	}
	if execution, taskStatus, message := executionAndTask(t, f, executionID); execution != "failed" || taskStatus != "failed" || message != "pull_request_closed" {
		t.Fatalf("state = %s/%s %q; want failed pull_request_closed", execution, taskStatus, message)
	}
	var class *string
	if err := f.db.QueryRow(`SELECT error_class FROM executions WHERE id = ?`, executionID).Scan(&class); err != nil || class != nil {
		t.Fatalf("error_class = %v %v; want NULL", class, err)
	}
	if row := consumedState(t, f, approval.ID); row.Status != "invalidated" || row.Reason != "target_changed" {
		t.Fatalf("approval = %+v; want invalidated target_changed", row)
	}
	if n := invalidationAudits(t, f, approval.ID, "target_changed"); n != 1 {
		t.Fatalf("invalidation audits = %d; want 1", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id = ? AND status = 'failed'`, executionID); n != 1 {
		t.Fatalf("failed notifications = %d; want 1", n)
	}
}

// Only the PR outcome can be reported from waiting_approval, and nothing
// changes on a refused report.
func TestApprovalWaitResultValidation(t *testing.T) {
	f, token, executionID, _, approval := waitingApprovalFixture(t)
	merge := strings.Repeat("c", 40)
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  int
	}{
		{"bad merge SHA", prMergedOnGitHub("abc"), 400},
		{"merge SHA on failure", map[string]any{"fromStatus": "waiting_approval", "status": "failed", "errorMessage": "pull_request_closed", "mergeCommitSha": merge}, 400},
		{"error class", map[string]any{"fromStatus": "waiting_approval", "status": "failed", "errorClass": "publish_failed", "errorMessage": "pull_request_closed"}, 400},
		{"unknown reason", map[string]any{"fromStatus": "waiting_approval", "status": "failed", "errorMessage": "other"}, 400},
		{"missing reason", map[string]any{"fromStatus": "waiting_approval", "status": "failed"}, 400},
		{"other fields", map[string]any{"fromStatus": "waiting_approval", "status": "succeeded", "mergeCommitSha": merge, "pushedSha": merge}, 400},
		{"wrong from", map[string]any{"fromStatus": "merging", "status": "succeeded", "mergeCommitSha": merge}, 409},
		{"other target", map[string]any{"fromStatus": "waiting_approval", "status": "cancelled"}, 409},
	} {
		if status, body := postResult(t, f, token, executionID, tc.input); status != tc.want {
			t.Fatalf("%s = %d %s; want %d", tc.name, status, body, tc.want)
		}
	}
	if execution, taskStatus, _ := executionAndTask(t, f, executionID); execution != "waiting_approval" || taskStatus != "waiting_approval" {
		t.Fatalf("state = %s/%s; want unchanged", execution, taskStatus)
	}
	if row := consumedState(t, f, approval.ID); row.Status != "pending" {
		t.Fatalf("approval = %+v; want pending", row)
	}
	// Without a merge approval there was no PR waiting for approval.
	stageExec(t, f, `DELETE FROM approvals WHERE id = ?`, approval.ID)
	if status, body := postResult(t, f, token, executionID, prClosedOnGitHub()); status != 409 {
		t.Fatalf("without approval = %d %s; want 409", status, body)
	}
}
