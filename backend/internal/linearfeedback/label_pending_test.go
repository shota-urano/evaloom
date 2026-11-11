package linearfeedback_test

import (
	"context"
	"net/http"
	"testing"
)

// Schema support precedes the label transport. A pending label must neither
// be sent as a comment nor block delivery of actions this Worker supports.
func TestPendingLabelWaitsWithoutBlockingSupportedActions(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, _, _ := setup(t, fake)
	mustExec(t, db, `INSERT INTO routes VALUES ('route:project','project',NULL,'t','t')`)
	const labelKey = "linear-1:label:decision"
	mustExec(t, db, `INSERT INTO linear_outbox (id,task_id,linear_issue_id,action,kind,label_name,
		dedupe_key,status,created_at) VALUES ('label','linear-1','issue-1','add_label','route_approved',
		'route:project',?,'pending','2000-01-01T00:00:00Z')`, labelKey)
	if err := worker.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.commentCount(); got != 1 {
		t.Fatalf("comment requests=%d; want the original success comment only", got)
	}
	if got := outboxRow(t, db, labelKey); got.status != "pending" || got.attempts != 0 || got.outcomeUnknown != 0 {
		t.Fatalf("pending label was changed: %+v", got)
	}
	if got := outboxRow(t, db, dedupeKey); got.status != "sent" {
		t.Fatalf("success comment was blocked: %+v", got)
	}
}
