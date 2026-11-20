package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/notification"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// Spec 12 3.4: notification failures must preserve the committed lifecycle
// result, including when every registered transport fails. Only classified
// fields may reach execution_events; transport details belong in the log.
func TestNotificationFailureRecordsEachTerminalState(t *testing.T) {
	for _, target := range []model.ExecutionStatus{
		model.ExecutionSucceeded, model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut,
	} {
		for _, failure := range []struct {
			reason string
			err    error
		}{
			{"permission_denied", notification.ErrPermissionDenied},
			{"launch_failed", notification.ErrLaunchFailed},
			{"timeout", notification.ErrTimeout},
			{"timeout_deadline", context.DeadlineExceeded},
			{"other", errors.New("permission_denied launch_failed timeout")},
		} {
			t.Run(string(target)+"/"+failure.reason, func(t *testing.T) {
				from := model.ExecutionRunning
				if target == model.ExecutionSucceeded {
					from = model.ExecutionPublishing
				}
				f, _, id, _ := resultFixture(t, string(from))
				if err := task.TransitionExecution(context.Background(), f.db, task.Transition{
					TaskID: executionTaskID(t, f, id), ExecutionID: id,
					FromTask: model.TaskRunning, FromExecution: from, ToExecution: target,
				}); err != nil {
					t.Fatal(err)
				}
				before := readResultSnapshot(t, f, id)
				transportErr := fmt.Errorf("PRIVATE-TRANSPORT-DETAIL: %w", failure.err)
				calls := 0
				d := notification.Dispatcher{DB: f.db, Channels: []notification.Channel{{
					Name: "macos", Notifier: notificationFunc(func(ctx context.Context, n notification.Notification) error {
						calls++
						if n.ExecutionID != id || n.Status != target {
							t.Fatalf("notification=%+v", n)
						}
						// Sending must happen before any durable attempt record.
						var attempted sql.NullString
						if err := f.db.QueryRow(`SELECT attempted_at FROM notification_requests
							WHERE execution_id=? AND status=?`, id, target).Scan(&attempted); err != nil {
							t.Fatal(err)
						}
						if attempted.Valid || readResultSnapshot(t, f, id) != before {
							t.Fatal("delivery was recorded before Notifier returned")
						}
						return transportErr
					}),
				}}}
				if err := d.DispatchPending(context.Background()); !errors.Is(err, transportErr) {
					t.Fatalf("dispatch error=%v; want transport error for Control Plane log", err)
				}
				before.Events++
				if after := readResultSnapshot(t, f, id); after != before || calls != 1 {
					t.Fatalf("notification changed state: got=%+v want=%+v calls=%d", after, before, calls)
				}
				var payload []byte
				if err := f.db.QueryRow(`SELECT payload FROM execution_events
					WHERE execution_id=? AND type='notification_failed'`, id).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				var fields map[string]string
				if err := json.Unmarshal(payload, &fields); err != nil {
					t.Fatal(err)
				}
				wantReason := failure.reason
				if wantReason == "timeout_deadline" {
					wantReason = "timeout"
				}
				if len(fields) != 3 || fields["kind"] != string(target) || fields["channel"] != "macos" || fields["reason"] != wantReason {
					t.Fatalf("failure payload=%s", payload)
				}
				// A saved failure is an attempted send, never a successful receipt.
				restartNotificationFixture(t, f)
				fresh := &recordingNotifier{}
				restarted := notificationDispatcher(f, fresh)
				if err := restarted.DispatchPending(context.Background()); err != nil || len(fresh.recorded()) != 0 {
					t.Fatalf("recorded failure retried: %v %v", fresh.recorded(), err)
				}
				if after := readResultSnapshot(t, f, id); after != before {
					t.Fatalf("restart changed state or recorded a success: got=%+v want=%+v", after, before)
				}
			})
		}
	}
}

func TestLinearFailureDoesNotSuppressDesktopNotification(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	status, body := postResult(t, f, token, id, resultBody("working", "failed"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
	if _, err := store.AppendExecutionEvent(context.Background(), f.db, id, model.EventNotificationFailed,
		json.RawMessage(`{"kind":"failed","channel":"linear","reason":"auth"}`)); err != nil {
		t.Fatal(err)
	}
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err != nil || len(fake.recorded()) != 1 {
		t.Fatalf("Linear failure suppressed desktop delivery: calls=%v error=%v", fake.recorded(), err)
	}
}
