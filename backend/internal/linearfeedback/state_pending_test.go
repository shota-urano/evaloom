package linearfeedback_test

import (
	"net/http"
	"testing"
)

// State sending is a separate implementation unit. Until it is wired, the
// comment Worker must neither send NULL as a comment nor block comments behind
// pending/skipped state rows (spec 16 3.6.3).
func TestCommentWorkerLeavesStateChangesPending(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false)}
	db, worker, _, _ := setup(t, fake)
	mustExec(t, db, `UPDATE linear_outbox SET created_at = '2000-01-01T00:00:00Z' WHERE action = 'set_state'`)
	mustExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, action, state_name,
		dedupe_key, status, execution_id, created_at) VALUES ('skipped', 'linear-1', 'issue-1', 'started',
		'set_state', 'In Progress', 'skipped-state', 'skipped', 'linear-1-e1', '1999-01-01T00:00:00Z')`)
	for range 2 {
		sendDue(t, worker)
	}
	if fake.commentCount() != 1 {
		t.Fatalf("comments = %d, want 1", fake.commentCount())
	}
	var pending, skipped, unknown int
	if err := db.QueryRow(`SELECT sum(status = 'pending'), sum(status = 'skipped'), sum(outcome_unknown)
		FROM linear_outbox WHERE action = 'set_state'`).Scan(&pending, &skipped, &unknown); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || skipped != 1 || unknown != 0 {
		t.Fatalf("state rows: pending=%d skipped=%d unknown=%d", pending, skipped, unknown)
	}
}
