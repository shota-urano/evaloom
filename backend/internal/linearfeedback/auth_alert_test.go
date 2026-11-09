package linearfeedback_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
)

const unauthorized = `{"errors":[{"message":"Authentication required"}]}`

// recordingAuthAlerter records the Worker's authentication alerts and the
// Poller's alerts on one timeline, standing in for the macOS helper.
type recordingAuthAlerter struct {
	mu     sync.Mutex
	events []string // "comment" or "poll:<class>"
	err    error
}

func (a *recordingAuthAlerter) AlertLinearCommentAuth(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "comment")
	return a.err
}

func (a *recordingAuthAlerter) AlertLinearPollFailing(_ context.Context, alert linearintake.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "poll:"+string(alert.Class))
	return a.err
}

func (a *recordingAuthAlerter) want(t *testing.T, want ...string) {
	t.Helper()
	a.mu.Lock()
	got := append([]string(nil), a.events...)
	a.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("alerts = %q; want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("alerts = %q; want %q", got, want)
		}
	}
}

// answer switches the fake's commentCreate response.
func (f *fakeLinear) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentStatus, f.commentBody = status, body
}

// queueRows adds n pending rows for issue-1 without an Execution, so one
// SendDue call sends several rows.
func queueRows(t *testing.T, db *sql.DB, prefix string, n int) {
	t.Helper()
	for i := range n {
		id := prefix + "-" + strconv.Itoa(i)
		mustExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body, status,
			attempts, created_at, outcome_unknown) VALUES (?, 'linear-1', 'issue-1', 'intake_blocked', ?, 'body', 'pending',
			0, ?, 0)`, id, "linear-1:intake_blocked:"+id, testTime)
	}
}

func countStatus(t *testing.T, db *sql.DB, status, reason string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM linear_outbox WHERE status = ? AND coalesce(failure_reason, '') = ?`,
		status, reason).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pollClient stands in for Linear in the Poller: every query fails with
// class, or succeeds when class is "".
type pollClient struct {
	mu     sync.Mutex
	class  linear.ErrorClass
	keyErr error
}

func (c *pollClient) set(class linear.ErrorClass) { c.mu.Lock(); c.class = class; c.mu.Unlock() }

func (c *pollClient) CheckKey() error { c.mu.Lock(); defer c.mu.Unlock(); return c.keyErr }

func (c *pollClient) ListReadyIssues(context.Context, string, string) ([]linear.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.class == "" {
		return nil, nil
	}
	return nil, &linear.Error{Op: "list issues", Class: c.class}
}

// sharedPoller returns a Poller of team-a (registered on the setup Project)
// using gate and alerter.
func sharedPoller(t *testing.T, db *sql.DB, client linearintake.Client, gate *linear.AuthAlertGate,
	alerter linearintake.Alerter) *linearintake.Poller {
	t.Helper()
	mustExec(t, db, `UPDATE projects SET linear_team_id = 'team-a'`)
	return &linearintake.Poller{DB: db, Client: client, AuthGate: gate, Alerter: alerter, TriggerLabel: "ai-ready"}
}

func pollOnce(t *testing.T, p *linearintake.Poller) {
	t.Helper()
	if _, err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 受け入れ基準: 送信が認証エラーで failed になると通知が1回出る。1回の処理で
// 複数の行が認証エラーになっても通知は1回。認証エラーが続く間はもう出ず、送信が
// 1回成功した後にまた認証エラーになると、もう一度出る（spec 16 3.4.3）。
func TestAuthFailureAlertsOncePerBatchAndAgainAfterSuccess(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
	db, worker, _, _ := setup(t, fake)
	alerter := &recordingAuthAlerter{}
	worker.Alerter = alerter
	queueRows(t, db, "a", 2) // three rows in one SendDue

	sendDue(t, worker)
	if n := countStatus(t, db, "failed", "auth"); n != 3 {
		t.Fatalf("auth-failed rows = %d; want 3", n)
	}
	alerter.want(t, "comment")

	// The key is still invalid: more auth failures stay quiet.
	queueRows(t, db, "b", 2)
	sendDue(t, worker)
	sendDue(t, worker)
	alerter.want(t, "comment")

	// One successful send lifts the suppression.
	fake.answer(http.StatusOK, okComment)
	queueRows(t, db, "c", 1)
	sendDue(t, worker)
	if n := countStatus(t, db, "sent", ""); n != 1 {
		t.Fatalf("sent rows = %d; want 1", n)
	}
	alerter.want(t, "comment")
	fake.answer(http.StatusUnauthorized, unauthorized)
	queueRows(t, db, "d", 2)
	sendDue(t, worker)
	alerter.want(t, "comment", "comment")
}

// 受け入れ基準: Linear が認証を通したうえで返した失敗（issue_removed など）では
// 抑止が解けない（spec 16 3.4.3 step 5）。
func TestAuthenticatedFailureDoesNotLiftSuppression(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
	db, worker, _, _ := setup(t, fake)
	alerter := &recordingAuthAlerter{}
	worker.Alerter = alerter
	sendDue(t, worker)
	alerter.want(t, "comment")

	// The Issue was removed: Linear authenticated the request, but the row
	// fails (issue_removed), which is not a success.
	fake.answer(http.StatusOK, `{"errors":[{"message":"Entity not found","extensions":{"code":"INVALID_INPUT"}}]}`)
	fake.mu.Lock()
	fake.issueBody = `{"data":{"issue":null}}`
	fake.mu.Unlock()
	queueRows(t, db, "removed", 1)
	sendDue(t, worker)
	if n := countStatus(t, db, "failed", "issue_removed"); n != 1 {
		t.Fatalf("issue_removed rows = %d; want 1", n)
	}
	// A temporary failure (retry scheduled) is not a success either.
	fake.answer(http.StatusInternalServerError, `{}`)
	fake.mu.Lock()
	fake.issueBody = issueJSON(false)
	fake.mu.Unlock()
	queueRows(t, db, "temp", 1)
	sendDue(t, worker)
	if r := outboxRow(t, db, "linear-1:intake_blocked:temp-0"); r.status != "pending" || r.attempts != 1 {
		t.Fatalf("temporary failure row = %+v", r)
	}

	fake.answer(http.StatusUnauthorized, unauthorized)
	queueRows(t, db, "auth", 1)
	sendDue(t, worker)
	alerter.want(t, "comment")
}

// 受け入れ基準: 通知の送信に失敗してもタスクと outbox の行は変わらず、
// execution_events に notification_sent / notification_failed が書かれない。
func TestAlertFailureChangesNoTaskRowOrEvent(t *testing.T) {
	run := func(t *testing.T, alertErr error) (row, failureRecord, int) {
		t.Helper()
		fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
		db, worker, _, _ := setup(t, fake)
		holdKey := enqueueHold(t, db) // a row without an Execution
		alerter := &recordingAuthAlerter{err: alertErr}
		worker.Alerter = alerter
		before := countAllNotificationEvents(t, db)
		beforeStates := taskStates(t, db)
		sendDue(t, worker)
		alerter.want(t, "comment")
		// Compare within this DB: separate setups can have different updated_at values.
		if afterStates := taskStates(t, db); afterStates != beforeStates || afterStates.task != "completed" {
			t.Fatalf("task changed (alert error %v): before %+v, after %+v", alertErr, beforeStates, afterStates)
		}
		return outboxRow(t, db, holdKey), readFailureRecord(t, db, holdKey),
			countAllNotificationEvents(t, db) - before
	}
	okRow, okRecord, okEvents := run(t, nil)
	badRow, badRecord, badEvents := run(t, errors.New("notification: macOS helper: registration_failed"))
	if badRow != okRow || badRecord.status != "failed" || badRecord.failureReason.String != "auth" ||
		badRecord.failureReason != okRecord.failureReason || badRecord.executionID != okRecord.executionID {
		t.Fatalf("outbox row differs: ok %+v %+v, alert failed %+v %+v", okRow, okRecord, badRow, badRecord)
	}
	// The hold row has no Execution and the alert itself is never recorded.
	if okEvents != 0 || badEvents != 0 {
		t.Fatalf("notification events written: ok %d, alert failed %d; want 0", okEvents, badEvents)
	}
}

// 受け入れ基準: 照会と送信の両方が認証エラーになっても、通知は先に検出した側の
// 1回だけ出る（spec 16 3.4.3 step 2、Q84）。
func TestFirstDetectorAlertsForBoth(t *testing.T) {
	t.Run("poller first", func(t *testing.T) {
		fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
		db, worker, _, _ := setup(t, fake)
		gate, alerter := &linear.AuthAlertGate{}, &recordingAuthAlerter{}
		worker.AuthGate, worker.Alerter = gate, alerter
		poller := sharedPoller(t, db, &pollClient{class: linear.ClassAuth}, gate, alerter)
		pollOnce(t, poller)
		sendDue(t, worker)
		pollOnce(t, poller)
		if n := countStatus(t, db, "failed", "auth"); n != 1 {
			t.Fatalf("auth-failed rows = %d; want 1", n)
		}
		alerter.want(t, "poll:auth")
	})
	t.Run("worker first", func(t *testing.T) {
		fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
		db, worker, _, _ := setup(t, fake)
		gate, alerter := &linear.AuthAlertGate{}, &recordingAuthAlerter{}
		worker.AuthGate, worker.Alerter = gate, alerter
		poller := sharedPoller(t, db, &pollClient{class: linear.ClassAuth}, gate, alerter)
		sendDue(t, worker)
		for range linearintake.FailureAlertThreshold + 1 {
			pollOnce(t, poller) // no "5 failures" alert either: the class is auth (Q86)
		}
		alerter.want(t, "comment")
	})
}

// 受け入れ基準: 照会（どれか1つの Team）か送信のどちらかが1回成功した後にまた
// 認証エラーになると、もう一度出る。照会の成功でも送信の抑止が解ける。
func TestPollSuccessLiftsWorkerSuppression(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
	db, worker, _, _ := setup(t, fake)
	gate, alerter := &linear.AuthAlertGate{}, &recordingAuthAlerter{}
	worker.AuthGate, worker.Alerter = gate, alerter
	client := &pollClient{class: linear.ClassAuth}
	poller := sharedPoller(t, db, client, gate, alerter)
	sendDue(t, worker)
	pollOnce(t, poller)
	alerter.want(t, "comment")
	client.set("")
	pollOnce(t, poller)
	queueRows(t, db, "again", 1)
	sendDue(t, worker)
	alerter.want(t, "comment", "comment")
	// And a send success lifts the Poller's suppression.
	fake.answer(http.StatusOK, okComment)
	queueRows(t, db, "ok", 1)
	sendDue(t, worker)
	client.set(linear.ClassAuth)
	pollOnce(t, poller)
	alerter.want(t, "comment", "comment", "poll:auth")
}

// 受け入れ基準: 照会と送信が同時に認証エラーを検出しても通知は1回。
func TestConcurrentDetectionAlertsOnce(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
	db, worker, _, _ := setup(t, fake)
	gate := &linear.AuthAlertGate{}
	worker.AuthGate = gate
	client := &pollClient{class: linear.ClassAuth}
	for i := range 20 {
		alerter := &recordingAuthAlerter{}
		worker.Alerter = alerter
		poller := sharedPoller(t, db, client, gate, alerter)
		queueRows(t, db, "c"+strconv.Itoa(i), 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, errs[0] = poller.PollOnce(context.Background()) }()
		go func() { defer wg.Done(); <-start; errs[1] = worker.SendDue(context.Background()) }()
		close(start)
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		alerter.mu.Lock()
		n := len(alerter.events)
		alerter.mu.Unlock()
		if n != 1 {
			t.Fatalf("iteration %d: alerts = %q; want exactly 1", i, alerter.events)
		}
		gate.Release() // as if the key worked again before the next iteration
	}
}

// 受け入れ基準: API キーのファイルが無い・読めない・権限が 0600 でないときも
// 認証エラーとして共通の抑止で通知が1回出て、その後に照会か送信が認証エラーに
// なっても、どちらかが1回成功するまで通知は出ない（Q86）。
func TestKeyFileProblemJoinsSharedSuppression(t *testing.T) {
	t.Run("poller detects the key file", func(t *testing.T) {
		fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: unauthorized, issueBody: issueJSON(false)}
		db, worker, _, _ := setup(t, fake)
		gate, alerter := &linear.AuthAlertGate{}, &recordingAuthAlerter{}
		worker.AuthGate, worker.Alerter = gate, alerter
		// The real client with a missing key file.
		missing, err := linear.NewClient(nil, "https://linear.invalid/graphql", filepath.Join(t.TempDir(), "missing"), nil)
		if err != nil {
			t.Fatal(err)
		}
		poller := sharedPoller(t, db, missing, gate, alerter)
		pollOnce(t, poller)
		alerter.want(t, "poll:auth")
		// The key is fixed for sending but still rejected; a poll with
		// another auth error: still one alert.
		sendDue(t, worker)
		poller.Client = &pollClient{class: linear.ClassAuth}
		pollOnce(t, poller)
		alerter.want(t, "poll:auth")
		// One success, then auth again: alert again.
		poller.Client = &pollClient{}
		pollOnce(t, poller)
		queueRows(t, db, "after", 1)
		sendDue(t, worker)
		alerter.want(t, "poll:auth", "comment")
	})
	cases := map[string]func(t *testing.T) string{
		"missing": func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
		"0644": func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), linear.APIKeyFileName)
			mustWrite(t, path, 0o644)
			return path
		},
	}
	for name, keyPath := range cases {
		t.Run("worker detects the key file "+name, func(t *testing.T) {
			fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
			db, worker, _, _ := setup(t, fake)
			gate, alerter := &linear.AuthAlertGate{}, &recordingAuthAlerter{}
			bad, err := linear.NewClient(nil, "https://linear.invalid/graphql", keyPath(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			good := worker.Client
			worker.Client, worker.AuthGate, worker.Alerter = bad, gate, alerter
			sendDue(t, worker)
			sendDue(t, worker)
			// The row stays pending (as before); one alert.
			if r := outboxRow(t, db, dedupeKey); r.status != "pending" || r.attempts != 0 {
				t.Fatalf("row = %+v; want untouched", r)
			}
			alerter.want(t, "comment")
			// A poll auth error afterwards stays quiet.
			poller := sharedPoller(t, db, &pollClient{class: linear.ClassAuth}, gate, alerter)
			pollOnce(t, poller)
			alerter.want(t, "comment")
			// The key works again: the send succeeds and lifts the gate.
			worker.Client = good
			sendDue(t, worker)
			if r := outboxRow(t, db, dedupeKey); r.status != "sent" {
				t.Fatalf("row = %+v; want sent", r)
			}
			pollOnce(t, poller)
			alerter.want(t, "comment", "poll:auth")
		})
	}
}

var _ linearfeedback.AuthAlerter = (*recordingAuthAlerter)(nil)

func mustWrite(t *testing.T, path string, mode uint32) {
	t.Helper()
	if err := writeFileMode(path, []byte(keyCanary), mode); err != nil {
		t.Fatal(err)
	}
}

func writeFileMode(path string, data []byte, mode uint32) error {
	if err := os.WriteFile(path, data, os.FileMode(mode)); err != nil {
		return err
	}
	return os.Chmod(path, os.FileMode(mode))
}
