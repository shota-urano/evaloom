package api

import (
	"context"
	"strings"
	"testing"
)

// linearTask turns the fixture's task into a Linear task linked to an Issue.
func linearTask(t *testing.T, f *authFixture, taskID string) {
	t.Helper()
	execSQL(t, f, `UPDATE tasks SET source = 'linear' WHERE id = ?`, taskID)
	execSQL(t, f, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id,
		task_id, attach_seq, created_at) VALUES ('link-1', 'issue-1', 'ENG-1', 'https://linear.app/x/ENG-1',
		'team-a', ?, 1, '2026-11-11T00:00:00Z')`, taskID)
}

// spec 17 3.3 step 3, 3.6: asking again for the same operation of the same
// Execution invalidates the unused approval (superseded) and withdraws its
// open decision (superseded); the new request is the only unused approval.
func TestApprovalRequestSupersedesUnusedApprovalOfSameOperation(t *testing.T) {
	for _, previous := range []string{"pending", "approved"} {
		t.Run(previous, func(t *testing.T) {
			f, token, id, taskID := reportFixture(t)
			approvalDeclarations(t, f, id)
			attempt := runTo(t, f, token, id, "publish")
			decisionStatus := map[string]string{"pending": "open", "approved": "answered"}[previous]
			execSQL(t, f, `INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, basis, detail, status, created_at)
				VALUES ('old-decision', 'approval', ?, ?, 'old-approval', 'content-0', '{}', ?, '2026-11-11T00:00:00Z')`, taskID, id, decisionStatus)
			execSQL(t, f, `INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target,
				content_id, params_hash, status, created_at)
				VALUES ('old-approval', ?, ?, 'old-decision', 'check', 'merge_pull_request', '{}', 'content-0', 'hash', ?, '2026-11-11T00:00:00Z')`,
				taskID, id, previous)
			request := stageApprovalRequest("merge_pull_request")
			request["contentId"] = "content-2"
			finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "needs_approval", map[string]any{
				"contentId": "content-2", "approvalRequests": []any{request},
			})))
			requireState(t, f, id, "waiting_approval", "waiting_approval", "")
			if n := count(t, f, `SELECT count(*) FROM approvals WHERE id = 'old-approval' AND status = 'invalidated'
				AND invalidated_reason = 'superseded'`); n != 1 {
				t.Fatalf("old approval not superseded")
			}
			wantDecision := map[string]string{"pending": "withdrawn", "approved": "answered"}[previous]
			if n := count(t, f, `SELECT count(*) FROM owner_decisions WHERE id = 'old-decision' AND status = ?
				AND (status <> 'withdrawn' OR withdrawn_reason = 'superseded')`, wantDecision); n != 1 {
				t.Fatalf("old decision is not %s", wantDecision)
			}
			if n := count(t, f, `SELECT count(*) FROM approvals a JOIN owner_decisions d ON d.id = a.decision_id
				WHERE a.execution_id = ? AND a.operation = 'merge_pull_request' AND a.status = 'pending'
				AND a.content_id = 'content-2' AND a.requested_stage = 'publish' AND d.status = 'open' AND d.kind = 'approval'`, id); n != 1 {
				t.Fatalf("new pending approvals=%d", n)
			}
			if n := count(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'approval.invalidate' AND target_id = 'old-approval'
				AND actor = 'system' AND json_extract(detail, '$.reason') = 'superseded'`); n != 1 {
				t.Fatalf("invalidation audits=%d", n)
			}
		})
	}
}

// spec 17 3.3 steps 4-7, spec 16 3.2: every operation that is not waived
// gets its own pending approval and kind=approval decision; the owner is
// notified once, and a Linear task gets one decision_open comment per
// decision that carries only the decision's ID, not the approval content.
func TestApprovalRequestNotifiesOnceAndWritesDecisionOpenForLinearTask(t *testing.T) {
	f, token, id, taskID := reportFixture(t)
	linearTask(t, f, taskID)
	setDefinition(t, f, id, "$.approvals", `[{"operation":"merge_pull_request","description":"Merge"},
		{"operation":"post_release","description":"Post"}]`)
	attempt := runTo(t, f, token, id, "check")
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request"), stageApprovalRequest("post_release")},
	})))
	requireState(t, f, id, "waiting_approval", "waiting_approval", "")
	if n := count(t, f, `SELECT count(*) FROM approvals a JOIN owner_decisions d ON d.id = a.decision_id AND d.subject_id = a.id
		WHERE a.execution_id = ? AND a.status = 'pending' AND d.kind = 'approval' AND d.status = 'open' AND d.basis = a.content_id`, id); n != 2 {
		t.Fatalf("pending approvals with decisions=%d", n)
	}
	fake := &recordingNotifier{}
	dispatcher := notificationDispatcher(f, fake)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fake.recorded(); len(calls) != 1 {
		t.Fatalf("approval notifications=%+v", calls)
	}
	rows, err := f.db.Query(`SELECT d.id, o.dedupe_key, o.body, o.execution_id, o.action, o.status FROM owner_decisions d
		JOIN linear_outbox o ON o.kind = 'decision_open' AND o.dedupe_key = d.task_id || ':decision_open:' || d.id
		WHERE d.execution_id = ? AND d.kind = 'approval'`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var decisionID, key, body, executionID, action, status string
		if err := rows.Scan(&decisionID, &key, &body, &executionID, &action, &status); err != nil {
			t.Fatal(err)
		}
		n++
		if executionID != id || action != "comment" || status != "pending" {
			t.Fatalf("outbox row=%s %s %s", executionID, action, status)
		}
		if !strings.Contains(body, "管理画面で判断待ち") || !strings.Contains(body, decisionID) {
			t.Fatalf("body lacks the notice or the decision ID:\n%s", body)
		}
		for _, secret := range []string{"PR #1", "https://example.com/1", "Do merge_pull_request", "Do post_release", "content-1"} {
			if strings.Contains(body, secret) {
				t.Fatalf("body carries approval content %q:\n%s", secret, body)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("decision_open rows=%d; want one per decision", n)
	}
	if c := count(t, f, `SELECT count(*) FROM linear_outbox WHERE kind = 'decision_open'`); c != 2 {
		t.Fatalf("all decision_open rows=%d", c)
	}
	// Spec 16 3.2, 3.7: decision_open is the only comment of the approval
	// wait; the old waiting_approval comment is not written beside it.
	if c := count(t, f, `SELECT count(*) FROM linear_outbox WHERE action = 'comment' AND kind <> 'decision_open'`); c != 0 {
		t.Fatalf("other comment rows=%d; want none", c)
	}
}

// spec 16 3.1: a manual task writes no decision_open row.
func TestApprovalRequestWritesNoDecisionOpenForManualTask(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	approvalDeclarations(t, f, id)
	attempt := runTo(t, f, token, id, "check")
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request")},
	})))
	requireState(t, f, id, "waiting_approval", "waiting_approval", "")
	if n := count(t, f, `SELECT count(*) FROM linear_outbox`); n != 0 {
		t.Fatalf("outbox rows=%d", n)
	}
}

// spec 17 3.3 (Q59), 02 5.2: an Execution that reached waiting_approval
// through its check result frees its route's slot, so the next task of the
// same Project is claimable; while it was checking the slot was taken.
func TestWaitingApprovalFromCheckResultFreesClaimSlot(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	approvalDeclarations(t, f, id)
	attempt := runTo(t, f, token, id, "check")
	next := createClaimTask(t, f, "project-a", "next")
	status, body := requestClaim(t, f, token)
	requireNoJob(t, status, body)
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request")},
	})))
	requireState(t, f, id, "waiting_approval", "waiting_approval", "")
	status, body = requestClaim(t, f, token)
	if claim := requireClaim(t, status, body); claim.Execution.TaskID != next.ID {
		t.Fatalf("claimed %s; want %s", claim.Execution.TaskID, next.ID)
	}
}
