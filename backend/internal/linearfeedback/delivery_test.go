package linearfeedback_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Enqueue stores UTC RFC3339Nano timestamps, whose fractional seconds have
// variable width. Oldest-first delivery must preserve subsecond ordering.
func TestCommentDeliveryOrdersVariablePrecisionTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name, older, newer string
	}{
		{"whole second", "2026-11-08T00:00:00Z", "2026-11-08T00:00:00.1Z"},
		{"fraction prefix", "2026-11-08T00:00:00.1Z", "2026-11-08T00:00:00.100000001Z"},
		{"same millisecond", "2026-11-08T00:00:00.100000001Z", "2026-11-08T00:00:00.100000002Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
			db, worker, _, _ := setup(t, fake)
			// Reverse ID order to ensure timestamps determine delivery order,
			// even when both timestamps fall within the same millisecond.
			mustExec(t, db, `UPDATE linear_outbox SET id = 'z-older', created_at = ? WHERE dedupe_key = ?`, tc.older, dedupeKey)
			mustExec(t, db, `INSERT INTO linear_outbox
				(id, task_id, linear_issue_id, kind, dedupe_key, body, status, created_at, execution_id)
				SELECT 'a-newer', task_id, linear_issue_id, kind, dedupe_key || '-newer', 'newer', 'pending', ?, execution_id
				FROM linear_outbox WHERE dedupe_key = ?`, tc.newer, dedupeKey)

			sendDue(t, worker)

			if fake.commentCount() != 2 || !strings.HasSuffix(fake.comments[0]["body"].(string), dedupeKey) || fake.comments[1]["body"] != "newer" {
				t.Fatalf("comments = %v; want older then newer", fake.comments)
			}
		})
	}
}

// A restarted Worker honors the persisted retry deadline, then records a
// recovered delivery as sent, without a terminal failure or another send.
func TestCommentRetryRecoversAfterWorkerRestart(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusInternalServerError, commentBody: `{}`, issueBody: issueJSON(false)}
	db, worker, c, _ := setup(t, fake)
	before := taskStates(t, db)
	sendDue(t, worker)
	if r := outboxRow(t, db, dedupeKey); r.status != "pending" || r.attempts != 1 {
		t.Fatalf("after failure: row = %+v", r)
	}
	fake.mu.Lock()
	fake.commentStatus, fake.commentBody = http.StatusOK, okComment
	fake.mu.Unlock()
	c.advance(time.Minute - time.Nanosecond)
	sendDue(t, restart(worker, c))
	if n := fake.commentCount(); n != 1 {
		t.Fatalf("requests before retry deadline = %d; want 1", n)
	}
	c.advance(time.Nanosecond)
	sendDue(t, restart(worker, c))
	if r := outboxRow(t, db, dedupeKey); r.status != "sent" || r.attempts != 2 || r.commentID.String != "comment-1" || r.nextAttemptAt.Valid || !r.sentAt.Valid {
		t.Fatalf("after recovery: row = %+v", r)
	}
	c.advance(time.Hour)
	sendDue(t, restart(worker, c))
	if n := fake.commentCount(); n != 2 {
		t.Fatalf("requests after recovery = %d; want 2", n)
	}
	if events := notificationEvents(t, db); len(events) != 1 || events[0].kind != "notification_sent" || events[0].payload["channel"] != "linear" {
		t.Fatalf("events = %+v; want one Linear notification_sent", events)
	}
	if after := taskStates(t, db); after != before {
		t.Fatalf("task states changed: before %+v, after %+v", before, after)
	}
}

// Receipt and event must commit together. A failed event write leaves the
// delivery pending for recovery, without reverting the completed task.
func TestCommentReceiptRollsBackWithFailedEventWrite(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, c, _ := setup(t, fake)
	before := taskStates(t, db)
	mustExec(t, db, `CREATE TRIGGER reject_linear_receipt BEFORE INSERT ON execution_events
		WHEN NEW.type = 'notification_sent' AND json_extract(NEW.payload, '$.channel') = 'linear'
		BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`)
	if err := worker.SendDue(context.Background()); err == nil || !strings.Contains(err.Error(), "receipt unavailable") {
		t.Fatalf("SendDue error = %v; want receipt write failure", err)
	}
	if r := outboxRow(t, db, dedupeKey); r.status != "pending" || r.commentID.Valid || r.sentAt.Valid || r.outcomeUnknown != 1 {
		t.Fatalf("row = %+v; want pending without a partial receipt", r)
	}
	if events := notificationEvents(t, db); len(events) != 0 {
		t.Fatalf("events = %+v; want none after rollback", events)
	}
	if n := fake.commentCount(); n != 1 {
		t.Fatalf("requests = %d; want the batch stopped after the failed write", n)
	}
	mustExec(t, db, `DROP TRIGGER reject_linear_receipt`)
	sendDue(t, restart(worker, c))
	if r := outboxRow(t, db, dedupeKey); r.status != "sent" || r.commentID.String != "comment-1" || !r.sentAt.Valid {
		t.Fatalf("recovered row = %+v", r)
	}
	if events := notificationEvents(t, db); len(events) != 1 || events[0].kind != "notification_sent" || events[0].payload["channel"] != "linear" {
		t.Fatalf("events = %+v; want one committed receipt", events)
	}
	if after := taskStates(t, db); after != before {
		t.Fatalf("task states changed: before %+v, after %+v", before, after)
	}
}
