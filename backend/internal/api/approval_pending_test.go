package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

const pendingPRURL = "https://github.com/example/repo/pull/12"

func setApprovalPolicy(t *testing.T, f *authFixture, project, value string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE projects SET approval_policy = json_object('version', 1, 'mergePullRequest', ?) WHERE id = ?`, value, project); err != nil {
		t.Fatal(err)
	}
}

// publishPR reports the push of sha and the PR (spec 11 3.3) and then the
// Runner's final publishing -> succeeded report, as the Git Publisher does.
func publishPR(t *testing.T, f *authFixture, token, id, sha string, created bool) (int, []byte) {
	t.Helper()
	stageExec(t, f, `UPDATE executions SET verified_sha=? WHERE id=?`, sha, id)
	stageOK(t, f, token, id, map[string]any{"fromStatus": "publishing", "status": "publishing", "pushedSha": sha, "branchName": "evaloom/result"})
	stageOK(t, f, token, id, map[string]any{"fromStatus": "publishing", "status": "publishing", "prNumber": 12, "prUrl": pendingPRURL, "prCreated": created})
	return postResult(t, f, token, id, resultBody("publishing", "succeeded"))
}

type approvalRow struct {
	ID, TaskID, ExecutionID, Operation, Target, ContentVersion, ParamsHash, Status, Reason string
	MaxUses, UseCount                                                                      int
	Undecided                                                                              bool
}

func approvalsOf(t *testing.T, f *authFixture, taskID string) []approvalRow {
	t.Helper()
	rows, err := f.db.Query(`SELECT id, task_id, execution_id, operation, target, content_version, params_hash, status,
		coalesce(invalidated_reason, ''), max_uses, use_count,
		decided_by IS NULL AND decided_at IS NULL AND expires_at IS NULL AND consumed_at IS NULL AND result IS NULL
		FROM approvals WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []approvalRow
	for rows.Next() {
		var r approvalRow
		if err := rows.Scan(&r.ID, &r.TaskID, &r.ExecutionID, &r.Operation, &r.Target, &r.ContentVersion, &r.ParamsHash,
			&r.Status, &r.Reason, &r.MaxUses, &r.UseCount, &r.Undecided); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requirePendingApproval checks the 3.4 binding of a pending approval.
func requirePendingApproval(t *testing.T, r approvalRow, taskID, executionID, sha string) {
	t.Helper()
	wantTarget := store.MergeTarget{Repository: "example/repo", PRNumber: 12, BaseBranch: "main", HeadBranch: "evaloom/result"}
	var target store.MergeTarget
	if err := json.Unmarshal([]byte(r.Target), &target); err != nil || target != wantTarget {
		t.Fatalf("target = %s (%v); want %+v", r.Target, err, wantTarget)
	}
	hash, err := store.MergeParamsHash(wantTarget, sha)
	if err != nil {
		t.Fatal(err)
	}
	if r.TaskID != taskID || r.ExecutionID != executionID || r.Operation != "merge_pull_request" || r.ContentVersion != sha ||
		r.ParamsHash != hash || r.Status != "pending" || r.Reason != "" || r.MaxUses != 1 || r.UseCount != 0 || !r.Undecided {
		t.Fatalf("approval = %+v; want pending merge_pull_request bound to %s", r, sha)
	}
}

func requireWaitingApproval(t *testing.T, f *authFixture, id string) {
	t.Helper()
	var execution, taskStatus string
	var finished, event bool
	if err := f.db.QueryRow(`SELECT e.status, t.status, e.finished_at IS NOT NULL,
		EXISTS(SELECT 1 FROM execution_events WHERE execution_id = e.id AND type = 'status_changed'
			AND json_extract(payload, '$.from') = 'publishing' AND json_extract(payload, '$.to') = 'waiting_approval')
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(&execution, &taskStatus, &finished, &event); err != nil {
		t.Fatal(err)
	}
	if execution != "waiting_approval" || taskStatus != "waiting_approval" || finished || !event {
		t.Fatalf("execution=%s task=%s finished=%v event=%v; want waiting_approval without finishing", execution, taskStatus, finished, event)
	}
}

// Acceptance (17 3.3): creating a PR in a require_approval Project moves the
// task to waiting_approval and writes a pending approval row; the wait is
// notified exactly once, even when the report is replayed and the
// dispatcher runs again.
func TestPublishedPRWaitsForApprovalAndNotifiesOnce(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	stageExec(t, f, `UPDATE executions SET branch_name='evaloom/result' WHERE id=?`, id)
	taskID := executionTaskID(t, f, id)
	sha := strings.Repeat("b", 40)
	status, body := publishPR(t, f, token, id, sha, true)
	if status != 204 {
		t.Fatalf("succeeded report = %d %s", status, body)
	}
	requireWaitingApproval(t, f, id)
	approvals := approvalsOf(t, f, taskID)
	if len(approvals) != 1 {
		t.Fatalf("approvals = %+v; want one pending row", approvals)
	}
	requirePendingApproval(t, approvals[0], taskID, id, sha)
	if n := stageCount(t, f, `SELECT count(*) FROM usage_records WHERE execution_id=?`, id); n != 1 {
		t.Fatalf("usage rows = %d; want the succeeded report's measurement", n)
	}

	// A replayed final report is a stale transition and changes nothing.
	status, body = postResult(t, f, token, id, resultBody("publishing", "succeeded"))
	assertAuthError(t, status, body, 409, Conflict)
	if again := approvalsOf(t, f, taskID); !reflect.DeepEqual(again, approvals) {
		t.Fatalf("replay changed approvals: %+v", again)
	}

	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	for range 2 {
		if err := d.DispatchPending(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	calls := fake.recorded()
	if len(calls) != 1 || calls[0].Status != model.ExecutionWaitingApproval || calls[0].ExecutionID != id ||
		calls[0].TaskID != taskID || calls[0].PRURL != pendingPRURL || calls[0].Stage != model.ExecutionPublishing {
		t.Fatalf("notifications = %+v; want one waiting_approval", calls)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id=?`, id); n != 1 {
		t.Fatalf("notification requests = %d; want 1", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='notification_sent'
		AND json_extract(payload,'$.status')='waiting_approval'`, id); n != 1 {
		t.Fatalf("notification_sent events = %d; want 1", n)
	}
}

// Acceptance (17 3.3 step 1, 3.6): a continue that adds a commit to the same
// PR invalidates the previous unused approval (superseded, audited with
// actor system) and creates a new pending approval bound to the new head.
func TestContinueCommitSupersedesPreviousApproval(t *testing.T) {
	f, token, first, _ := resultFixture(t, "publishing")
	stageExec(t, f, `UPDATE executions SET branch_name='evaloom/result',
		base_sha=(SELECT base_sha FROM worktrees WHERE id=executions.worktree_id) WHERE id=?`, first)
	taskID := executionTaskID(t, f, first)
	oldSHA, newSHA := strings.Repeat("b", 40), strings.Repeat("e", 40)
	if status, body := publishPR(t, f, token, first, oldSHA, true); status != 204 {
		t.Fatalf("first PR = %d %s", status, body)
	}
	previous := approvalsOf(t, f, taskID)
	if len(previous) != 1 {
		t.Fatalf("approvals = %+v", previous)
	}
	// The approval was approved but its merge failed before consume, so the
	// approval stays unused while the owner continues the task (3.5, Q57).
	// The decision and merge APIs belong to later tasks; set the state directly.
	stageExec(t, f, `UPDATE approvals SET status='approved', decided_by='owner', decided_at='2026-11-08T00:00:00Z',
		expires_at='2999-01-01T00:00:00Z' WHERE id=?`, previous[0].ID)
	stageExec(t, f, `UPDATE executions SET status='failed', error_class='publish_failed', finished_at='2026-11-08T00:00:01Z' WHERE id=?`, first)
	stageExec(t, f, `UPDATE tasks SET status='failed' WHERE id=?`, taskID)
	// A decided approval of the same PR is history and must not change.
	stageExec(t, f, `INSERT INTO approvals (id, task_id, execution_id, operation, target, content_version, params_hash,
		status, decided_by, decided_at, created_at) SELECT 'approval-rejected', task_id, execution_id, operation, target,
		content_version, params_hash, 'rejected', 'owner', '2026-11-07T00:00:00Z', '2026-11-07T00:00:00Z' FROM approvals WHERE id=?`, previous[0].ID)

	status, body := requestRetry(t, f, taskID, `{"mode":"continue"}`, nil)
	requireRetrySuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	second := requireClaim(t, status, body).Execution.ID
	for _, edge := range [][2]model.ExecutionStatus{{"claimed", "preparing"}, {"preparing", "running"}, {"running", "verifying"}, {"verifying", "publishing"}} {
		fromTask, _ := task.TaskStatusForExecution(edge[0])
		if err := task.TransitionExecution(context.Background(), f.db, task.Transition{TaskID: taskID, ExecutionID: second,
			FromTask: fromTask, FromExecution: edge[0], ToExecution: edge[1]}); err != nil {
			t.Fatal(err)
		}
	}
	// The Git Publisher found the open PR and pushed the new commit to it.
	if status, body := publishPR(t, f, token, second, newSHA, false); status != 204 {
		t.Fatalf("continue PR = %d %s", status, body)
	}
	requireWaitingApproval(t, f, second)

	approvals := approvalsOf(t, f, taskID)
	byID := map[string]approvalRow{}
	for _, r := range approvals {
		byID[r.ID] = r
	}
	if len(approvals) != 3 {
		t.Fatalf("approvals = %+v; want old, rejected and new", approvals)
	}
	if old := byID[previous[0].ID]; old.Status != "invalidated" || old.Reason != "superseded" || old.ContentVersion != oldSHA {
		t.Fatalf("previous approval = %+v; want invalidated/superseded", old)
	}
	if rejected := byID["approval-rejected"]; rejected.Status != "rejected" || rejected.Reason != "" {
		t.Fatalf("decided approval changed: %+v", rejected)
	}
	var fresh []approvalRow
	for _, r := range approvals {
		if r.Status == "pending" {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) != 1 {
		t.Fatalf("pending approvals = %+v; want 1", fresh)
	}
	requirePendingApproval(t, fresh[0], taskID, second, newSHA)
	var actor, reason, detailExecution string
	if err := f.db.QueryRow(`SELECT actor, json_extract(detail,'$.reason'), json_extract(detail,'$.executionId') FROM audit_logs
		WHERE action='approval.invalidate' AND target_type='approval' AND target_id=?`, previous[0].ID).Scan(&actor, &reason, &detailExecution); err != nil ||
		actor != "system" || reason != "superseded" || detailExecution != first {
		t.Fatalf("invalidation audit = %s/%s/%s err=%v", actor, reason, detailExecution, err)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='approval.invalidate'`); n != 1 {
		t.Fatalf("invalidation audits = %d; want 1", n)
	}
	// Each Execution's wait is notified once.
	if n := stageCount(t, f, `SELECT count(*) FROM notification_requests WHERE status='waiting_approval'
		AND execution_id IN (?, ?)`, first, second); n != 2 {
		t.Fatalf("waiting_approval requests = %d; want one per Execution", n)
	}
}

// 3.2: a Project that excludes PR merges from approval completes on the PR
// and creates no approval.
func TestDirectPushPolicyCompletesWithoutApproval(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	stageExec(t, f, `UPDATE executions SET branch_name='evaloom/result' WHERE id=?`, id)
	setApprovalPolicy(t, f, "project-a", "direct_push")
	if status, body := publishPR(t, f, token, id, strings.Repeat("b", 40), true); status != 204 {
		t.Fatalf("succeeded report = %d %s", status, body)
	}
	after := readResultSnapshot(t, f, id)
	if after.Execution != "succeeded" || after.Task != "completed" {
		t.Fatalf("state = %+v; want succeeded/completed", after)
	}
	if got := approvalsOf(t, f, executionTaskID(t, f, id)); len(got) != 0 {
		t.Fatalf("approvals = %+v; want none", got)
	}
}

// 3.4, Fail Closed: an approval cannot bind to a PR URL that is not a GitHub
// PR of the reported number, so the report is rejected without writes.
func TestPublishedPRWithoutGitHubURLIsRejectedWithoutWrites(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	stageExec(t, f, `UPDATE executions SET branch_name='evaloom/result',verified_sha=?,pushed_sha=?,pr_number=12,pr_url='https://github.com/example/repo/pull/13' WHERE id=?`,
		strings.Repeat("b", 40), strings.Repeat("b", 40), id)
	before := readResultSnapshot(t, f, id)
	status, body := postResult(t, f, token, id, resultBody("publishing", "succeeded"))
	assertAuthError(t, status, body, 422, PreconditionFailed)
	if after := readResultSnapshot(t, f, id); after != before {
		t.Fatalf("rejected report mutated DB: %+v -> %+v", before, after)
	}
	if got := approvalsOf(t, f, executionTaskID(t, f, id)); len(got) != 0 {
		t.Fatalf("approvals = %+v; want none", got)
	}
}
