package linearfeedback_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// failureRecord is the failure part of a linear_outbox row (spec 16 3.7).
type failureRecord struct {
	status        string
	executionID   sql.NullString
	failureReason sql.NullString
	failedAt      sql.NullString
}

func readFailureRecord(t *testing.T, db *sql.DB, dedupeKey string) failureRecord {
	t.Helper()
	var r failureRecord
	if err := db.QueryRow(`SELECT status, execution_id, failure_reason, failed_at FROM linear_outbox
		WHERE dedupe_key = ?`, dedupeKey).Scan(&r.status, &r.executionID, &r.failureReason, &r.failedAt); err != nil {
		t.Fatal(err)
	}
	return r
}

// enqueueHold queues a missing_acceptance_criteria comment for "linear-1"
// (a trigger without an Execution) and sets the succeeded row aside.
func enqueueHold(t *testing.T, db *sql.DB) string {
	t.Helper()
	mustExec(t, db, `UPDATE linear_outbox SET status = 'sent'`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	trigger := linearoutbox.Trigger{TaskID: "linear-1", Kind: linearoutbox.KindMissingAcceptanceCriteria,
		TargetID: "missing_acceptance_criteria"}
	if err := linearoutbox.EnqueueTx(context.Background(), tx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return trigger.DedupeKey()
}

// 受け入れ基準 (spec 16 3.4.2, Q85): Linear の失敗の reason が、認証エラーで
// auth、Issue が削除・アーカイブされていたときに issue_removed、5回目も
// 失敗したときに retries_exhausted になり、notification_failed の reason と
// linear_outbox の failure_reason が同じ値になる。failed_at は failed にした
// 時刻。Execution のある行はイベントとして、無い行は outbox の行として、
// 一覧に1回だけ出る。
func TestFailureReasonAndRecord(t *testing.T) {
	gqlNotFound := `{"errors":[{"message":"Entity not found","extensions":{"code":"INVALID_INPUT"}}]}`
	scenarios := []struct {
		name, reason string
		fake         func() *fakeLinear
	}{
		{"auth", "auth", func() *fakeLinear {
			return &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: `{}`, issueBody: issueJSON(false)}
		}},
		{"deleted", "issue_removed", func() *fakeLinear {
			return &fakeLinear{commentStatus: http.StatusOK, commentBody: gqlNotFound, issueBody: `{"data":{"issue":null}}`}
		}},
		{"archived", "issue_removed", func() *fakeLinear {
			return &fakeLinear{commentStatus: http.StatusOK, commentBody: gqlNotFound, issueBody: issueJSON(true)}
		}},
		{"exhausted", "retries_exhausted", func() *fakeLinear {
			return &fakeLinear{commentStatus: http.StatusInternalServerError, commentBody: `{}`, issueBody: issueJSON(false)}
		}},
	}
	for _, sc := range scenarios {
		for _, withExecution := range []bool{true, false} {
			name := sc.name + "/execution"
			if !withExecution {
				name = sc.name + "/no-execution"
			}
			t.Run(name, func(t *testing.T) {
				fake := sc.fake()
				db, worker, c, logs := setup(t, fake)
				key := dedupeKey
				if !withExecution {
					key = enqueueHold(t, db)
				}
				var failedAt time.Time
				for range 6 {
					failedAt = c.t
					sendDue(t, worker)
					if readFailureRecord(t, db, key).status != "pending" {
						break
					}
					c.advance(time.Hour)
				}
				r := readFailureRecord(t, db, key)
				if r.status != "failed" || r.failureReason.String != sc.reason {
					t.Fatalf("row = %+v; want failed with reason %s", r, sc.reason)
				}
				got, err := time.Parse(time.RFC3339Nano, r.failedAt.String)
				if err != nil || !got.Equal(failedAt) {
					t.Fatalf("failed_at = %q; want %s", r.failedAt.String, failedAt)
				}
				events := notificationEvents(t, db)
				failures, err := store.ListNotificationFailures(context.Background(), db, nil, 10)
				if err != nil || len(failures) != 1 {
					t.Fatalf("notification failures = %+v, %v; want exactly one", failures, err)
				}
				item := failures[0]
				if item.Reason != sc.reason || item.Channel != "linear" || item.TaskID != "linear-1" || item.TaskTitle != "Title" {
					t.Fatalf("listed failure = %+v", item)
				}
				if withExecution {
					if r.executionID.String != "linear-1-e1" {
						t.Fatalf("execution_id = %+v; want linear-1-e1", r.executionID)
					}
					if len(events) != 1 || events[0].kind != "notification_failed" || events[0].payload["reason"] != r.failureReason.String {
						t.Fatalf("events = %+v; want one notification_failed with reason %s", events, r.failureReason.String)
					}
					if item.EventID == nil || item.ExecutionID == nil || *item.ExecutionID != "linear-1-e1" ||
						item.OutboxID != nil || item.Kind != "succeeded" {
						t.Fatalf("listed failure = %+v; want the event, not the outbox row", item)
					}
				} else {
					if r.executionID.Valid {
						t.Fatalf("execution_id = %+v; want NULL", r.executionID)
					}
					if len(events) != 0 {
						t.Fatalf("events = %+v; want none for a trigger without an Execution", events)
					}
					if n := countAllNotificationEvents(t, db); n != 0 {
						t.Fatalf("notification events = %d; want none", n)
					}
					var outboxID string
					if err := db.QueryRow(`SELECT id FROM linear_outbox WHERE dedupe_key = ?`, key).Scan(&outboxID); err != nil {
						t.Fatal(err)
					}
					if item.EventID != nil || item.ExecutionID != nil || item.OutboxID == nil || *item.OutboxID != outboxID ||
						item.Kind != "missing_acceptance_criteria" {
						t.Fatalf("listed failure = %+v; want outbox row %s", item, outboxID)
					}
					if created, err := time.Parse(time.RFC3339Nano, item.CreatedAt); err != nil || !created.Equal(failedAt) {
						t.Fatalf("listed time = %q; want failed_at %s", item.CreatedAt, failedAt)
					}
				}
				if strings.Contains(logs.String(), keyCanary) {
					t.Fatal("API key leaked into the log")
				}
			})
		}
	}
}

func countAllNotificationEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM execution_events WHERE type IN ('notification_sent', 'notification_failed')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
