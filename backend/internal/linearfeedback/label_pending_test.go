package linearfeedback_test

import (
	"context"
	"net/http"
	"testing"
)

// Invalidated labels fail independently without becoming comments or
// blocking delivery of other actions (spec 16 3.6.5).
func TestInvalidatedLabelDoesNotBlockComment(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, _, _ := setup(t, fake)
	mustExec(t, db, `INSERT INTO routes VALUES ('route:project','project',NULL,'t','t')`)
	const labelKey = "linear-1:label:decision"
	mustExec(t, db, `INSERT INTO linear_outbox (id,task_id,linear_issue_id,action,kind,label_name,
		dedupe_key,status,created_at) VALUES ('label','linear-1','issue-1','add_label','route_approved',
		'route:project',?,'pending','2000-01-01T00:00:00Z')`, labelKey)
	mustExec(t, db, `DELETE FROM routes WHERE label='route:project'`)
	if err := worker.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.commentCount(); got != 1 {
		t.Fatalf("comment requests=%d; want the original success comment only", got)
	}
	if got := outboxRow(t, db, labelKey); got.status != "failed" || got.attempts != 1 || got.outcomeUnknown != 0 {
		t.Fatalf("invalid label was not failed: %+v", got)
	}
	if got := outboxRow(t, db, dedupeKey); got.status != "sent" {
		t.Fatalf("success comment was blocked: %+v", got)
	}
}
