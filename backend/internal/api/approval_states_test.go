package api

import (
	"strings"
	"testing"
)

// setApprovalState puts the fixture's Execution into a spec 17 3.9 state and
// its task into waiting_approval, the mapped task state.
func setApprovalState(t *testing.T, f *authFixture, id, state string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, state, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET status = 'waiting_approval' WHERE id = (SELECT task_id FROM executions WHERE id = ?)`, id); err != nil {
		t.Fatal(err)
	}
}

func insertApproval(t *testing.T, f *authFixture, id, executionID, status string) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO approvals (id, task_id, execution_id, operation, target,
		content_version, params_hash, status, created_at)
		SELECT ?, task_id, id, 'merge_pull_request', '{"prNumber":12}', ?, ?, ?, created_at
		FROM executions WHERE id = ?`, id, strings.Repeat("c", 40), strings.Repeat("d", 64), status, executionID); err != nil {
		t.Fatal(err)
	}
}

func approvalStatus(t *testing.T, f *authFixture, id string) (string, string) {
	t.Helper()
	var status, reason string
	if err := f.db.QueryRow(`SELECT status, coalesce(invalidated_reason, '') FROM approvals WHERE id = ?`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

// Spec 17 3.6: cancelling a task waiting for approval cancels it and
// invalidates the unused approval (cancelled), audited with actor system.
func TestCancelWaitingApprovalInvalidatesUnusedApproval(t *testing.T) {
	f, _, id, _ := resultFixture(t, "publishing")
	setApprovalState(t, f, id, "waiting_approval")
	insertApproval(t, f, "approval-old", id, "rejected")
	insertApproval(t, f, "approval-open", id, "pending")
	before := cancellationState(t, f, id)
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 || len(body) != 0 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	requireCancelled(t, f, id, before)
	if status, reason := approvalStatus(t, f, "approval-open"); status != "invalidated" || reason != "cancelled" {
		t.Fatalf("open approval = %s/%s; want invalidated/cancelled", status, reason)
	}
	if status, reason := approvalStatus(t, f, "approval-old"); status != "rejected" || reason != "" {
		t.Fatalf("decided approval changed to %s/%s", status, reason)
	}
	var audits int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE actor = 'system'
		AND action = 'approval.invalidate' AND target_type = 'approval' AND target_id = 'approval-open'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("invalidation audits = %d, err = %v; want 1", audits, err)
	}
}

// Spec 17 3.5.2: a merging Execution may hold a consumed approval, so the
// cancellation is only requested; the Runner decides at consume time.
func TestCancelMergingRecordsRequestWithoutTransition(t *testing.T) {
	f, _, id, _ := resultFixture(t, "publishing")
	setApprovalState(t, f, id, "merging")
	insertApproval(t, f, "approval-approved", id, "approved")
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 || len(body) != 0 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	after := cancellationState(t, f, id)
	if after.Execution != "merging" || after.Task != "waiting_approval" || !after.Requested.Valid || after.Finished.Valid {
		t.Fatalf("merging cancellation = %+v; want request only", after)
	}
	if status, _ := approvalStatus(t, f, "approval-approved"); status != "approved" {
		t.Fatalf("approval = %s; want approved until consume", status)
	}
	// A repeated request keeps the first timestamp.
	status, body = requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("repeat cancel=%d %s", status, body)
	}
	if again := cancellationState(t, f, id); again != after {
		t.Fatalf("repeat cancel changed state: before=%+v after=%+v", after, again)
	}
}

// Transitions outside the table are 409 conflicts over HTTP as well: the
// Runner result API cannot move an approval-state Execution.
func TestApprovalStatesRejectTransitionsOutsideTableWith409(t *testing.T) {
	for _, state := range []string{"waiting_approval", "merging"} {
		for _, to := range []string{"succeeded", "failed", "publishing"} {
			t.Run(state+"->"+to, func(t *testing.T) {
				f, token, id, _ := resultFixture(t, "publishing")
				setApprovalState(t, f, id, state)
				before := cancellationState(t, f, id)
				status, body := postResult(t, f, token, id, resultBody(state, to))
				assertAuthError(t, status, body, 409, Conflict)
				if after := cancellationState(t, f, id); after != before {
					t.Fatalf("rejected result mutated DB: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}
