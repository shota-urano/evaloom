package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/notification"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

type recordingNotifier struct {
	mu    sync.Mutex
	calls []notification.Notification
	err   error
}

func (n *recordingNotifier) Notify(ctx context.Context, value notification.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, value)
	return n.err
}

func (n *recordingNotifier) recorded() []notification.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notification.Notification(nil), n.calls...)
}

func notificationDispatcher(f *authFixture, fake *recordingNotifier) notification.Dispatcher {
	return notification.Dispatcher{DB: f.db, Channels: []notification.Channel{{Name: "fake", Notifier: fake}}}
}

func restartNotificationFixture(t *testing.T, f *authFixture) {
	t.Helper()
	var seq int
	var name, filename string
	if err := f.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &filename); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = store.Open(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.server = httptest.NewUnstartedServer(nil)
	f.auth, err = NewOwnerAuth(f.db, nil, f.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.Handler = NewHandler(f.db, nil, f.auth)
	f.server.Start()
	t.Cleanup(f.server.Close)
}

func TestLifecycleNotifiesOncePerStateAcrossRestart(t *testing.T) {
	for _, target := range []string{"succeeded", "failed", "timed_out", "blocked", "cancelled", "interrupted-boot", "interrupted-startup"} {
		t.Run(target, func(t *testing.T) {
			from := "running"
			if target == "succeeded" {
				from = "publishing"
			}
			f, token, id, _ := resultFixture(t, from)
			want := target
			input := resultBody(from, target)
			switch target {
			case "interrupted-boot":
				status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
				requireHeartbeat(t, status, body)
				reboot := heartbeatInput()
				reboot["bootId"] = "rebooted"
				status, body = postHeartbeat(t, f, token, "claim-runner", reboot)
				requireHeartbeat(t, status, body)
				want = "interrupted"
			case "interrupted-startup":
				started := time.Now().UTC().Add(time.Minute)
				if err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
					t.Fatal(err)
				}
				want = "interrupted"
			default:
				input["prUrl"] = "https://example.test/pull/1"
				status, body := postResult(t, f, token, id, input)
				if status != 204 {
					t.Fatalf("result status=%d body=%s", status, body)
				}
			}
			// No configured transports must not consume or falsely mark delivery.
			empty := notification.Dispatcher{DB: f.db}
			if err := empty.DispatchPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			fake := &recordingNotifier{}
			d := notificationDispatcher(f, fake)
			for range 2 {
				if err := d.DispatchPending(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			calls := fake.recorded()
			count := 1
			if target == "cancelled" {
				count = 0
			}
			if len(calls) != count {
				t.Fatalf("Notify calls=%+v; want %d", calls, count)
			}
			if count != 0 {
				n := calls[0]
				var title string
				if err := f.db.QueryRow(`SELECT title FROM tasks WHERE id = ?`, executionTaskID(t, f, id)).Scan(&title); err != nil {
					t.Fatal(err)
				}
				if n.ExecutionID != id || n.TaskID != executionTaskID(t, f, id) || n.Title != title || string(n.Status) != want {
					t.Fatalf("notification=%+v", n)
				}
				switch want {
				case "succeeded":
					if n.PRURL != input["prUrl"] {
						t.Fatalf("missing PR: %+v", n)
					}
				case "failed":
					if n.ErrorClass != model.ErrorExecutionFailed || n.Reason != "Harness exited" {
						t.Fatalf("missing failure detail: %+v", n)
					}
				case "timed_out":
					if n.Stage != model.ExecutionRunning {
						t.Fatalf("missing timeout stage: %+v", n)
					}
				case "blocked":
					if n.BlockCause != model.BlockGitState {
						t.Fatalf("missing block cause: %+v", n)
					}
				case "interrupted":
					if n.ErrorClass != model.ErrorInterrupted || n.Reason == "" {
						t.Fatalf("missing recovery detail: %+v", n)
					}
				}
			}
			var sent int
			if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'notification_sent'
				AND json_extract(payload, '$.status') = ? AND json_extract(payload, '$.kind') = ?
				AND json_extract(payload, '$.channel') = 'fake'`, id, want, want).Scan(&sent); err != nil || sent != count {
				t.Fatalf("notification_sent=%d err=%v; want %d", sent, err, count)
			}
			// Recreate the DB connection, auth/routing, Dispatcher AND Notifier.
			restartNotificationFixture(t, f)
			if target == want {
				status, body := postResult(t, f, token, id, input)
				assertAuthError(t, status, body, 409, Conflict)
			}
			fresh := &recordingNotifier{}
			restarted := notificationDispatcher(f, fresh)
			if err := restarted.DispatchPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls := fresh.recorded(); len(calls) != 0 {
				t.Fatalf("restart resent notifications: %+v", calls)
			}
		})
	}
}

func TestContinuedExecutionNotifiesEachStateOnlyOnce(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	block := func() {
		t.Helper()
		status, body := postResult(t, f, token, id, resultBody("running", "blocked"))
		if status != 204 {
			t.Fatalf("block status=%d body=%s", status, body)
		}
		if err := d.DispatchPending(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	block()
	restartNotificationFixture(t, f)
	d.DB = f.db
	resume := func() {
		t.Helper()
		for _, edge := range [][2]model.ExecutionStatus{{"blocked", "pending"}, {"pending", "claimed"}, {"claimed", "preparing"}, {"preparing", "running"}} {
			fromTask, _ := task.TaskStatusForExecution(edge[0])
			if err := task.TransitionExecution(context.Background(), f.db, task.Transition{TaskID: executionTaskID(t, f, id),
				ExecutionID: id, FromTask: fromTask, FromExecution: edge[0], ToExecution: edge[1]}); err != nil {
				t.Fatal(err)
			}
		}
	}
	resume()
	block() // A second blocked episode of the same Execution must not resend.
	resume()
	status, body := postResult(t, f, token, id, resultBody("running", "failed"))
	if status != 204 {
		t.Fatalf("failure status=%d body=%s", status, body)
	}
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := fake.recorded()
	if len(calls) != 2 || calls[0].Status != "blocked" || calls[1].Status != "failed" {
		t.Fatalf("per-state notifications=%+v", calls)
	}
}

func TestNotificationFailureDoesNotRollbackOrRetryAndStillFansOut(t *testing.T) {
	for _, tc := range []struct {
		reason string
		err    error
	}{
		{"permission_denied", notification.ErrPermissionDenied},
		{"launch_failed", notification.ErrLaunchFailed},
		{"timeout", notification.ErrTimeout},
		{"other", errors.New("permission_denied launch_failed timeout")},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "running")
			status, body := postResult(t, f, token, id, resultBody("running", "failed"))
			if status != 204 {
				t.Fatalf("result status=%d body=%s", status, body)
			}
			before := readResultSnapshot(t, f, id)
			broken, healthy := &recordingNotifier{err: fmt.Errorf("PRIVATE-ERROR-DETAIL: %w", tc.err)}, &recordingNotifier{}
			d := notification.Dispatcher{DB: f.db, Channels: []notification.Channel{{Name: "broken", Notifier: broken}, {Name: "healthy", Notifier: healthy}}}
			if err := d.DispatchPending(context.Background()); !errors.Is(err, broken.err) {
				t.Fatalf("delivery error=%v", err)
			}
			after := readResultSnapshot(t, f, id)
			before.Events += 2 // One failure event and one successful delivery receipt.
			if after != before || len(broken.recorded()) != 1 || len(healthy.recorded()) != 1 {
				t.Fatalf("state=%+v want=%+v broken=%v healthy=%v", after, before, broken.recorded(), healthy.recorded())
			}
			events, err := store.ListExecutionEvents(context.Background(), f.db, id, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var failed, sent int
			for _, event := range events {
				switch event.Type {
				case model.EventNotificationFailed:
					failed++
					var payload map[string]string
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if len(payload) != 3 || payload["kind"] != "failed" || payload["channel"] != "broken" || payload["reason"] != tc.reason {
						t.Fatalf("unsafe or incorrect failure payload: %s", event.Payload)
					}
				case model.EventNotificationSent:
					sent++
					var payload map[string]string
					if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["channel"] != "healthy" {
						t.Fatalf("failed transport recorded as sent: %s, %v", event.Payload, err)
					}
				}
			}
			if failed != 1 || sent != 1 {
				t.Fatalf("failed=%d sent=%d; want one of each", failed, sent)
			}
			restartNotificationFixture(t, f)
			fresh := &recordingNotifier{}
			restarted := notificationDispatcher(f, fresh)
			if err := restarted.DispatchPending(context.Background()); err != nil || len(fresh.recorded()) != 0 {
				t.Fatalf("failed transport retried: %v %v", fresh.recorded(), err)
			}
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("restart changed state or duplicated events: %+v want %+v", after, before)
			}
		})
	}
}

type notificationLogWriter chan string

func (w notificationLogWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

type notificationFunc func(context.Context, notification.Notification) error

func (f notificationFunc) Notify(ctx context.Context, n notification.Notification) error {
	return f(ctx, n)
}

func TestNotificationWorkerRecordsAndLogsFailureDuringShutdown(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	status, body := postResult(t, f, token, id, resultBody("running", "failed"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
	before := readResultSnapshot(t, f, id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := notification.Dispatcher{DB: f.db, Channels: []notification.Channel{{Name: "fake", Notifier: notificationFunc(
		func(ctx context.Context, _ notification.Notification) error {
			cancel()
			return fmt.Errorf("PRIVATE-CANCELLATION-DETAIL: %w", ctx.Err())
		})}}}
	logs := make(notificationLogWriter, 10)
	d.Run(ctx, slog.New(slog.NewTextHandler(logs, nil)))
	select {
	case line := <-logs:
		if !strings.Contains(line, "PRIVATE-CANCELLATION-DETAIL") {
			t.Fatalf("shutdown failure detail missing from log: %s", line)
		}
	default:
		t.Fatal("shutdown failure was not logged")
	}
	before.Events++
	if after := readResultSnapshot(t, f, id); after != before {
		t.Fatalf("cancellation changed state or lost failure event: %+v want %+v", after, before)
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id = ? AND type = 'notification_failed'`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"channel":"fake","kind":"failed","reason":"other"}` {
		t.Fatalf("unsafe cancellation payload: %s", payload)
	}
}

func TestNotificationFailureEventWriteErrorStillFansOutWithoutRetry(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	status, body := postResult(t, f, token, id, resultBody("running", "failed"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
	before := readResultSnapshot(t, f, id)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_failure BEFORE INSERT ON execution_events
		WHEN NEW.type = 'notification_failed' BEGIN SELECT RAISE(ABORT, 'failure receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	broken, healthy := &recordingNotifier{err: errors.New("transport unavailable")}, &recordingNotifier{}
	d := notification.Dispatcher{DB: f.db, Channels: []notification.Channel{{Name: "broken", Notifier: broken}, {Name: "healthy", Notifier: healthy}}}
	if err := d.DispatchPending(context.Background()); !errors.Is(err, broken.err) || !strings.Contains(err.Error(), "failure receipt unavailable") {
		t.Fatalf("missing transport or persistence error: %v", err)
	}
	before.Events++ // Healthy channel's receipt survives the failed event write.
	if after := readResultSnapshot(t, f, id); after != before || len(healthy.recorded()) != 1 {
		t.Fatalf("event write error changed state or stopped fanout: %+v want %+v, calls=%v", after, before, healthy.recorded())
	}
	restartNotificationFixture(t, f)
	fresh := &recordingNotifier{}
	restarted := notificationDispatcher(f, fresh)
	if err := restarted.DispatchPending(context.Background()); err != nil || len(fresh.recorded()) != 0 {
		t.Fatalf("failed event write caused retry: %v %v", fresh.recorded(), err)
	}
}

func TestNotificationWorkerLogsTransportFailureWithoutChangingState(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	status, body := postResult(t, f, token, id, resultBody("publishing", "succeeded"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
	before := readResultSnapshot(t, f, id)
	broken := &recordingNotifier{err: errors.New("transport unavailable")}
	d := notificationDispatcher(f, broken)
	logs := make(notificationLogWriter, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	defer func() { cancel(); <-done }()
	select {
	case line := <-logs:
		for _, want := range []string{"level=ERROR", "notification dispatch failed", id, "succeeded", "fake", "transport unavailable"} {
			if !strings.Contains(line, want) {
				t.Fatalf("Control Plane log=%q; missing %q", line, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification failure never reached Control Plane log")
	}
	after := readResultSnapshot(t, f, id)
	before.Events++ // The failure event must also be recorded for succeeded.
	if after != before || after.Task != "completed" || after.Execution != "succeeded" || len(broken.recorded()) != 1 {
		t.Fatalf("state=%+v want=%+v calls=%v", after, before, broken.recorded())
	}
}

func TestUncommittedAndRolledBackTransitionsNeverNotify(t *testing.T) {
	f, _, id, _ := resultFixture(t, "running")
	var seq int
	var name, path string
	if err := f.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	observer, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	taskID := executionTaskID(t, f, id)
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := task.TransitionExecutionTx(context.Background(), tx, task.Transition{TaskID: taskID, ExecutionID: id,
		FromTask: "running", FromExecution: "running", ToExecution: "failed"}); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := observer.QueryRow(`SELECT count(*) FROM notification_requests`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("uncommitted request became visible: %d %v", pending, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err != nil || len(fake.recorded()) != 0 {
		t.Fatalf("rolled back transition delivered: %v %v", fake.recorded(), err)
	}
}

func TestNotificationWorkerDeliversPendingRequestAfterRestart(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	status, body := postResult(t, f, token, id, resultBody("running", "failed"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
	restartNotificationFixture(t, f) // Unsent requests must survive, too.
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() { cancel(); <-done }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("worker never delivered pending notification")
		case <-tick.C:
			if len(fake.recorded()) == 1 {
				return
			}
		}
	}
}

func TestContextBuilderStopsAlsoRequestNotifications(t *testing.T) {
	for _, target := range []string{"blocked", "failed"} {
		t.Run(target, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			wantStatus := 422
			if target == "blocked" {
				input.Project[0].SourceCommitSHA = strings.Repeat("c", 40)
				wantStatus = 409
			} else {
				input.Harness = input.Harness[:0] // A selected required file is missing.
			}
			status, body := postContext(t, f, token, contextPath, input)
			if status != wantStatus {
				t.Fatalf("context status=%d body=%s", status, body)
			}
			fake := &recordingNotifier{}
			d := notificationDispatcher(f, fake)
			if err := d.DispatchPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			calls := fake.recorded()
			if len(calls) != 1 || string(calls[0].Status) != target || calls[0].ExecutionID != otherExecution {
				t.Fatalf("context notifications=%+v", calls)
			}
			if target == "blocked" && calls[0].BlockCause != model.BlockContextConflict ||
				target == "failed" && (calls[0].ErrorClass != model.ErrorConfig || calls[0].Reason == "") {
				t.Fatalf("missing committed Context Builder metadata: %+v", calls[0])
			}
		})
	}
}

func TestConcurrentDispatchersNeverDeliverSameRequestTwice(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	status, body := postResult(t, f, token, id, resultBody("running", "failed"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
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
		t.Fatalf("concurrent deliveries=%+v", calls)
	}
}

func TestReceiptWriteFailureNeverCausesResendAfterRestart(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	status, body := postResult(t, f, token, id, resultBody("running", "failed"))
	if status != 204 {
		t.Fatalf("result status=%d body=%s", status, body)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON execution_events
		WHEN NEW.type = 'notification_sent' BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	fake := &recordingNotifier{}
	d := notificationDispatcher(f, fake)
	if err := d.DispatchPending(context.Background()); err == nil || !strings.Contains(err.Error(), "receipt unavailable") {
		t.Fatalf("receipt failure=%v", err)
	}
	if len(fake.recorded()) != 1 {
		t.Fatal("transport was not called")
	}
	restartNotificationFixture(t, f)
	fresh := &recordingNotifier{}
	restarted := notificationDispatcher(f, fresh)
	if err := restarted.DispatchPending(context.Background()); err != nil || len(fresh.recorded()) != 0 {
		t.Fatalf("receipt failure resent notification: %v %v", fresh.recorded(), err)
	}
}
