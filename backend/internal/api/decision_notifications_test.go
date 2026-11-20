package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/notification"
)

func jsonNotification(n notification.Notification) (string, error) {
	raw, err := json.Marshal(n)
	return string(raw), err
}

func routeNotificationFixture(t *testing.T) (*authFixture, string) {
	t.Helper()
	f, token, proposal, _ := routeProposalFixture(t)
	if status, body := proposalResult(t, f, token, proposal, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("proposal=%d %s", status, body)
	}
	return f, routeDecisionOf(t, f, proposal)
}

// Acceptance: all four decision kinds notify once; successful delivery is
// durable across a fresh DB connection and Dispatcher (no shared memory).
func TestOwnerDecisionKindsNotifyOnceAcrossRestart(t *testing.T) {
	for _, kind := range []string{"route", "approval", "blocked", "question"} {
		t.Run(kind, func(t *testing.T) {
			var f *authFixture
			var decision string
			switch kind {
			case "route":
				f, decision = routeNotificationFixture(t)
			case "approval":
				var id string
				f, _, id, _, _ = waitingApprovalFixture(t)
				decision = openDecisionID(t, f, id, kind)
			case "blocked":
				var id string
				f, id = publishUnknownFixture(t)
				decision = openDecisionID(t, f, id, kind)
			case "question":
				var id string
				f, id = questionFixture(t)
				decision = openDecisionID(t, f, id, kind)
			}
			fake := &recordingNotifier{}
			empty := notification.Dispatcher{DB: f.db}
			if err := empty.DispatchPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			d := notificationDispatcher(f, fake)
			for range 2 {
				if err := d.DispatchPending(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			calls := fake.recorded()
			if len(calls) != 1 || calls[0].DecisionID != decision || string(calls[0].DecisionKind) != kind || calls[0].Body == "" {
				t.Fatalf("decision notifications=%+v", calls)
			}
			if kind == "route" {
				if calls[0].ExecutionID != "" || count(t, f, `SELECT count(*) FROM audit_logs WHERE action='notification.sent'
					AND target_type='owner_decision' AND target_id=?`, decision) != 1 {
					t.Fatal("route notification lacks Q148 audit receipt")
				}
			} else if count(t, f, `SELECT count(*) FROM execution_events WHERE type='notification_sent'
				AND json_extract(payload,'$.kind')='decision' AND json_extract(payload,'$.decisionId')=?`, decision) != 1 {
				t.Fatal("decision notification lacks execution receipt")
			}
			if kind == "blocked" && (!strings.Contains(calls[0].Body, "続行") || !strings.Contains(calls[0].Body, "出す操作の結果がわからない")) {
				t.Fatalf("publish_unknown body=%q", calls[0].Body)
			}
			restartNotificationFixture(t, f)
			fresh := &recordingNotifier{}
			restarted := notificationDispatcher(f, fresh)
			if err := restarted.DispatchPending(context.Background()); err != nil || len(fresh.recorded()) != 0 {
				t.Fatalf("restart resent decision: %+v, %v", fresh.recorded(), err)
			}
		})
	}
}

// Acceptance: one report with two approvals produces one notification whose
// click target is the first decision. Separate reports remain separate batches.
func TestApprovalNotificationBatchesFollowReportBoundaries(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	setDefinition(t, f, id, "$.approvals", `[{"operation":"merge_pull_request","description":"Merge"},
		{"operation":"post_release","description":"Post"}]`)
	attempt := runTo(t, f, token, id, "check")
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request"), stageApprovalRequest("post_release")},
	})))
	first := openDecisionID(t, f, id, "approval")
	// Pending notifications, including their batch identity, survive restart.
	restartNotificationFixture(t, f)
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fake.recorded(); len(calls) != 1 || calls[0].DecisionID != first {
		t.Fatalf("batched notifications=%+v; first=%s", calls, first)
	}
	if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='notification_sent'
		AND json_extract(payload,'$.kind')='decision'`, id); n != 2 {
		t.Fatalf("per-decision receipts=%d", n)
	}
	// Restart with a fresh Dispatcher, so only persisted receipts can
	// prevent the other approval of the batch from being sent a second time.
	restartNotificationFixture(t, f)
	d = notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err != nil || len(fake.recorded()) != 1 {
		t.Fatalf("batch resent after restart: %+v, %v", fake.recorded(), err)
	}
}

// A renewed approval is a new row even though the Execution has remained
// waiting_approval. It must not be suppressed by the original batch's receipt.
func TestRenewedApprovalRequestsNewNotification(t *testing.T) {
	f, _, _, _, approval := waitingApprovalFixture(t)
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	execSQL(t, f, `UPDATE approvals SET status='expired' WHERE id=?`, approval.ID)
	status, body := approvalRequest(t, f, "POST", "/api/approvals/"+approval.ID+"/renew", "{}")
	if status != 201 {
		t.Fatalf("renew=%d %s", status, body)
	}
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := fake.recorded()
	if len(calls) != 2 || calls[0].DecisionID == calls[1].DecisionID {
		t.Fatalf("renewed notifications=%+v", calls)
	}
}

// Acceptance: cancelled and withdrawn decisions produce no notification.
func TestCancelledAndWithdrawnDecisionsDoNotNotify(t *testing.T) {
	for _, reason := range []string{"cancelled", "superseded", "expired"} {
		t.Run(reason, func(t *testing.T) {
			f, id := questionFixture(t)
			decision := openDecisionID(t, f, id, "question")
			if reason == "cancelled" {
				if status, body := requestCancellation(t, f, executionTaskID(t, f, id)); status != 204 {
					t.Fatalf("cancel=%d %s", status, body)
				}
			} else {
				execSQL(t, f, `UPDATE owner_decisions SET status='withdrawn',withdrawn_reason=? WHERE id=?`, reason, decision)
			}
			fake := &recordingNotifier{}
			d := notificationDispatcher(f, fake)
			if err := d.DispatchPending(context.Background()); err != nil || len(fake.recorded()) != 0 {
				t.Fatalf("withdrawal notified: %+v, %v", fake.recorded(), err)
			}
		})
	}
}

// Acceptance: succeeded includes only the first output label and remaining
// count. URLs and other output metadata do not even reach the transport.
func TestSucceededNotificationExcludesOutputURLs(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	attempt := runTo(t, f, token, id, "publish")
	finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "succeeded", map[string]any{
		"outputs": []any{
			map[string]any{"kind": "private_kind", "label": "Published release", "url": "https://private.example/secret-first"},
			map[string]any{"kind": "private_kind", "label": "PRIVATE-SECOND-LABEL", "url": "https://private.example/secret-second"},
		},
	})))
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := fake.recorded()
	if len(calls) != 1 || calls[0].Status != model.ExecutionSucceeded || !strings.Contains(calls[0].Body, "Published release") || !strings.Contains(calls[0].Body, "ほか1件") {
		t.Fatalf("success notification=%+v", calls)
	}
	raw, _ := jsonNotification(calls[0])
	for _, secret := range []string{"private.example", "secret-first", "secret-second", "PRIVATE-SECOND-LABEL", "private_kind"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("output metadata leaked %q: %s", secret, raw)
		}
	}
}

// Acceptance: decision bodies and transport arguments exclude question text,
// options, target, finding and stage summaries.
func TestDecisionNotificationExcludesPrivateContent(t *testing.T) {
	for _, kind := range []string{"question", "approval", "blocked"} {
		t.Run(kind, func(t *testing.T) {
			var f *authFixture
			var id string
			switch kind {
			case "question":
				f, id = questionFixture(t)
			case "approval":
				f, _, id, _, _ = waitingApprovalFixture(t)
			case "blocked":
				f, id = checkBlockedFixture(t)
			}
			decision := openDecisionID(t, f, id, kind)
			execSQL(t, f, `UPDATE owner_decisions SET detail='{"question":"PRIVATE-QUESTION","options":["PRIVATE-OPTION"],"target":"PRIVATE-TARGET","finding":"PRIVATE-FINDING","summary":"PRIVATE-SUMMARY"}' WHERE id=?`, decision)
			execSQL(t, f, `UPDATE executions SET error_message='PRIVATE-ERROR' WHERE id=?`, id)
			execSQL(t, f, `UPDATE check_results SET summary='PRIVATE-STAGE-SUMMARY',findings='[{"message":"PRIVATE-FINDING"}]' WHERE execution_id=?`, id)
			execSQL(t, f, `UPDATE approvals SET target='"PRIVATE-APPROVAL-TARGET"',summary='PRIVATE-APPROVAL-SUMMARY' WHERE execution_id=?`, id)
			fake := &recordingNotifier{}
			d := notificationDispatcher(f, fake)
			if err := d.DispatchPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			calls := fake.recorded()
			if len(calls) != 1 {
				t.Fatalf("notifications=%+v", calls)
			}
			raw, _ := jsonNotification(calls[0])
			if strings.Contains(raw, "PRIVATE-") {
				t.Fatalf("private decision content reached Notifier: %s", raw)
			}
		})
	}
}

func TestConcurrentDecisionDispatchersNotifyOnce(t *testing.T) {
	f, _ := questionFixture(t)
	fake := &recordingNotifier{}
	errs := make(chan error, 2)
	for range 2 {
		d := notificationDispatcher(f, fake)
		go func() { errs <- d.DispatchPending(context.Background()) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls := fake.recorded(); len(calls) != 1 {
		t.Fatalf("concurrent decisions=%+v", calls)
	}
}

func TestDecisionFailureFansOutAndDoesNotRetryWithinProcess(t *testing.T) {
	f, id := questionFixture(t)
	broken := &recordingNotifier{err: notification.ErrPermissionDenied}
	healthy := &recordingNotifier{}
	d := notification.Dispatcher{DB: f.db, Channels: []notification.Channel{{Name: "broken", Notifier: broken}, {Name: "healthy", Notifier: healthy}}}
	if err := d.DispatchPending(context.Background()); !errors.Is(err, broken.err) {
		t.Fatalf("error=%v", err)
	}
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broken.recorded()) != 1 || len(healthy.recorded()) != 1 {
		t.Fatal("decision fanout/retry counts differ")
	}
	requireState(t, f, id, "blocked", "blocked", "")
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id=? AND type='notification_failed'`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"channel":"broken","kind":"decision","reason":"permission_denied"}` {
		t.Fatalf("failure payload=%s", payload)
	}
}

// Spec 12 3.4/Q148 records success after sending. Without a specified durable
// pre-send reservation, a receipt write failure leaves a restart resend window.
// Make that limit observable in the daemon log for both receipt destinations.
func TestDecisionReceiptWriteFailureLogsAndMayResendAfterRestart(t *testing.T) {
	for _, kind := range []string{"question", "route"} {
		t.Run(kind, func(t *testing.T) {
			var f *authFixture
			var decision, receiptSQL, trigger string
			if kind == "route" {
				f, decision = routeNotificationFixture(t)
				receiptSQL = `SELECT count(*) FROM audit_logs WHERE action='notification.sent'
					AND target_type='owner_decision' AND target_id=?`
				trigger = `CREATE TRIGGER reject_decision_receipt BEFORE INSERT ON audit_logs
					WHEN NEW.action='notification.sent' AND NEW.target_type='owner_decision'
					BEGIN SELECT RAISE(ABORT, 'decision receipt unavailable'); END`
			} else {
				var id string
				f, id = questionFixture(t)
				decision = openDecisionID(t, f, id, kind)
				receiptSQL = `SELECT count(*) FROM execution_events WHERE type='notification_sent'
					AND json_extract(payload,'$.kind')='decision' AND json_extract(payload,'$.decisionId')=?`
				trigger = `CREATE TRIGGER reject_decision_receipt BEFORE INSERT ON execution_events
					WHEN NEW.type='notification_sent' AND json_extract(NEW.payload,'$.kind')='decision'
					BEGIN SELECT RAISE(ABORT, 'decision receipt unavailable'); END`
			}
			execSQL(t, f, trigger)
			fake := &recordingNotifier{}
			d := notificationDispatcher(f, fake)
			logs := make(notificationLogWriter, 10)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				d.Run(ctx, slog.New(slog.NewTextHandler(logs, nil)))
			}()
			func() {
				defer func() { cancel(); <-done }()
				select {
				case line := <-logs:
					for _, want := range []string{"level=ERROR", "notification dispatch failed", decision, "fake",
						"send_succeeded=true", "decision receipt unavailable"} {
						if !strings.Contains(line, want) {
							t.Errorf("receipt failure log=%q; missing %q", line, want)
						}
					}
				case <-time.After(5 * time.Second):
					t.Error("receipt write failure never reached Control Plane log")
				}
			}()
			if len(fake.recorded()) != 1 || count(t, f, receiptSQL, decision) != 0 {
				t.Fatalf("send/receipt counts=%d/%d; want 1/0", len(fake.recorded()), count(t, f, receiptSQL, decision))
			}
			if n := count(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open'`, decision); n != 1 {
				t.Fatal("receipt write failure changed the decision")
			}
			if err := d.DispatchPending(context.Background()); err != nil || len(fake.recorded()) != 1 {
				t.Fatalf("receipt failure caused a retry loop: %v, calls=%+v", err, fake.recorded())
			}
			// Restore storage, then reopen the DB and create an independent worker.
			// No receipt exists, so the specified send-then-record design resends.
			execSQL(t, f, `DROP TRIGGER reject_decision_receipt`)
			restartNotificationFixture(t, f)
			fresh := &recordingNotifier{}
			restarted := notificationDispatcher(f, fresh)
			if err := restarted.DispatchPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls := fresh.recorded(); len(calls) != 1 || calls[0].DecisionID != decision || count(t, f, receiptSQL, decision) != 1 {
				t.Fatalf("restart without receipt: calls=%+v, receipts=%d; want one resend and receipt", calls, count(t, f, receiptSQL, decision))
			}
			// Once a receipt commits, another restart must not resend it.
			restartNotificationFixture(t, f)
			restarted = notificationDispatcher(f, fresh)
			if err := restarted.DispatchPending(context.Background()); err != nil || len(fresh.recorded()) != 1 {
				t.Fatalf("persisted receipt did not suppress resend: %v, calls=%+v", err, fresh.recorded())
			}
		})
	}
}
