package linearfeedback_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

const (
	testTime  = "2026-11-08T00:00:00Z"
	keyCanary = "lin_api_EVALOOM_YSK2_FAKE_KEY_9a8b7c"
)

// fakeLinear is an httptest stand-in for the Linear GraphQL API. commentCreate
// answers with commentStatus/commentBody; the Issue query with issueBody.
type fakeLinear struct {
	mu            sync.Mutex
	comments      []map[string]any // variables of each commentCreate request
	issueReads    int
	commentStatus int
	commentBody   string
	issueBody     string
	authHeaders   []string
}

func (f *fakeLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	switch {
	case strings.Contains(req.Query, "commentCreate"):
		f.comments = append(f.comments, req.Variables)
		w.WriteHeader(f.commentStatus)
		io.WriteString(w, f.commentBody)
	case strings.Contains(req.Query, "EvaloomIssue"):
		f.issueReads++
		io.WriteString(w, f.issueBody)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeLinear) commentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments)
}

const okComment = `{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-1"}}}}`

func issueJSON(archived bool) string {
	archivedAt := "null"
	if archived {
		archivedAt = `"2026-11-08T00:00:00.000Z"`
	}
	return `{"data":{"issue":{"id":"issue-1","identifier":"ENG-1","url":"https://linear.app/x/issue/ENG-1","title":"t",
"description":"d","updatedAt":"2026-11-08T00:00:00.000Z","archivedAt":` + archivedAt + `,"trashed":null,
"team":{"id":"team-a"},"state":{"type":"started"},"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)} }
func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// setup returns a DB whose Linear task "linear-1" (Issue "issue-1",
// Execution "linear-1-e1") has just succeeded, so one outbox row is
// pending, plus a Worker talking to fake through the real Linear client.
func setup(t *testing.T, fake *fakeLinear) (*sql.DB, *linearfeedback.Worker, *clock, *bytes.Buffer) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "feedback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('runner', 'Runner', 'hash', '/worktrees', 'online', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO projects (id, name, local_repo_path, git_remote_url, runner_id, base_branch,
		default_role, context_location, copy_allowlist, allowed_operations, approval_policy, timeouts, created_at, updated_at)
		VALUES ('project', 'Project', '/repos/project', 'https://example.test/repo.git', 'runner', 'main',
		'developer', '.', '[]', '[]', '{}', '{}', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name,
		task_type, collaboration_mode, status, idempotency_key, created_at, updated_at, source)
		VALUES ('linear-1', 'project', 'Title', 'Body', 'tests pass', 'test', 'development', 'solo', 'verifying',
		'key-linear-1', ?, ?, 'linear')`, testTime, testTime)
	mustExec(t, db, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('linear-1-e1', 'linear-1', 1, 'continue', 'publishing', ?)`, testTime)
	mustExec(t, db, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id,
		task_id, attach_seq, created_at) VALUES ('link-1', 'issue-1', 'ENG-1', 'https://linear.app/x/ENG-1',
		'team-a', 'linear-1', 1, ?)`, testTime)
	// The production path writes the outbox row (spec 16 3.4 step 1).
	if err := task.TransitionExecution(context.Background(), db, task.Transition{TaskID: "linear-1",
		ExecutionID: "linear-1-e1", FromTask: model.TaskVerifying, FromExecution: model.ExecutionPublishing,
		ToExecution: model.ExecutionSucceeded}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	keyPath := filepath.Join(t.TempDir(), linear.APIKeyFileName)
	if err := os.WriteFile(keyPath, []byte(keyCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	return db, &linearfeedback.Worker{DB: db, Client: client, Logger: logger, Now: c.now}, c, logs
}

type row struct {
	status        string
	attempts      int
	nextAttemptAt sql.NullString
	commentID     sql.NullString
	sentAt        sql.NullString
}

func outboxRow(t *testing.T, db *sql.DB, dedupeKey string) row {
	t.Helper()
	var r row
	if err := db.QueryRow(`SELECT status, attempts, next_attempt_at, linear_comment_id, sent_at
		FROM linear_outbox WHERE dedupe_key = ?`, dedupeKey).Scan(
		&r.status, &r.attempts, &r.nextAttemptAt, &r.commentID, &r.sentAt); err != nil {
		t.Fatal(err)
	}
	return r
}

type event struct {
	kind    string
	payload map[string]any
}

func notificationEvents(t *testing.T, db *sql.DB) []event {
	t.Helper()
	rows, err := db.Query(`SELECT type, payload FROM execution_events WHERE execution_id = 'linear-1-e1'
		AND type IN ('notification_sent', 'notification_failed') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []event
	for rows.Next() {
		var e event
		var raw string
		if err := rows.Scan(&e.kind, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &e.payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

type states struct {
	task, taskUpdated, execution string
}

func taskStates(t *testing.T, db *sql.DB) states {
	t.Helper()
	var s states
	if err := db.QueryRow(`SELECT t.status, t.updated_at, e.status FROM tasks t JOIN executions e ON e.task_id = t.id
		WHERE t.id = 'linear-1'`).Scan(&s.task, &s.taskUpdated, &s.execution); err != nil {
		t.Fatal(err)
	}
	return s
}

func sendDue(t *testing.T, w *linearfeedback.Worker) {
	t.Helper()
	if err := w.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

const dedupeKey = "linear-1:succeeded:linear-1-e1"

// 受け入れ基準: テスト用のサーバーにコメントが1回届き、status=sent とコメント ID が記録される。
func TestCommentDeliveredOnceAndRecordedSent(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, c, logs := setup(t, fake)

	sendDue(t, worker)
	c.advance(time.Hour)
	sendDue(t, worker) // A sent row is never sent again.

	if n := fake.commentCount(); n != 1 {
		t.Fatalf("commentCreate requests = %d; want 1", n)
	}
	got := fake.comments[0]
	if got["issueId"] != "issue-1" || !strings.HasSuffix(got["body"].(string), dedupeKey) {
		t.Fatalf("comment variables = %v", got)
	}
	if fake.authHeaders[0] != keyCanary {
		t.Fatalf("Authorization = %q; want the API key", fake.authHeaders[0])
	}
	r := outboxRow(t, db, dedupeKey)
	if r.status != "sent" || r.commentID.String != "comment-1" || r.attempts != 1 || !r.sentAt.Valid || r.nextAttemptAt.Valid {
		t.Fatalf("outbox row = %+v", r)
	}
	events := notificationEvents(t, db)
	if len(events) != 1 || events[0].kind != "notification_sent" || events[0].payload["channel"] != "linear" ||
		events[0].payload["kind"] != "succeeded" {
		t.Fatalf("events = %+v", events)
	}
	// No "status" field: it would suppress the macOS Dispatcher's request
	// for the same Execution (spec 12 3.4 dedupe).
	if _, ok := events[0].payload["status"]; ok {
		t.Fatalf("notification_sent payload has status: %v", events[0].payload)
	}
	if strings.Contains(logs.String(), keyCanary) {
		t.Fatal("API key leaked into the log")
	}
}

// 受け入れ基準: サーバーを失敗させると attempts が増え、上限で failed と
// notification_failed（channel=linear）が記録される。
// 受け入れ基準: 送信の失敗でタスクの状態が変わらない。
func TestFailingServerRetriesThenFails(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusInternalServerError, commentBody: `{}`, issueBody: issueJSON(false)}
	db, worker, c, _ := setup(t, fake)
	before := taskStates(t, db)

	// Spec 16 3.4 (Q80): 5 attempts, waiting 1, 2, 4 and 8 minutes.
	delays := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for attempt := 1; attempt <= 4; attempt++ {
		sendDue(t, worker)
		r := outboxRow(t, db, dedupeKey)
		want := c.t.Add(delays[attempt-1])
		next, err := time.Parse(time.RFC3339Nano, r.nextAttemptAt.String)
		if r.status != "pending" || r.attempts != attempt || err != nil || !next.Equal(want) {
			t.Fatalf("after attempt %d: row = %+v; want pending, next at %s", attempt, r, want)
		}
		// Not due yet: nothing is sent.
		c.advance(delays[attempt-1] - time.Second)
		sendDue(t, worker)
		if n := fake.commentCount(); n != attempt {
			t.Fatalf("requests before the delay = %d; want %d", n, attempt)
		}
		c.advance(time.Second)
		if len(notificationEvents(t, db)) != 0 {
			t.Fatal("event written before the last attempt")
		}
	}
	sendDue(t, worker)
	r := outboxRow(t, db, dedupeKey)
	if r.status != "failed" || r.attempts != 5 || r.nextAttemptAt.Valid || r.commentID.Valid || r.sentAt.Valid {
		t.Fatalf("final row = %+v; want failed after 5 attempts", r)
	}
	c.advance(time.Hour)
	sendDue(t, worker)
	if n := fake.commentCount(); n != 5 {
		t.Fatalf("commentCreate requests = %d; want 5", n)
	}
	events := notificationEvents(t, db)
	if len(events) != 1 || events[0].kind != "notification_failed" || events[0].payload["channel"] != "linear" ||
		events[0].payload["kind"] != "succeeded" || events[0].payload["reason"] != "other" || len(events[0].payload) != 3 {
		t.Fatalf("events = %+v", events)
	}
	// GET /api/notification-failures reads these events (spec 16 5).
	failures, err := store.ListNotificationFailures(context.Background(), db, 0, 10)
	if err != nil || len(failures) != 1 || failures[0].Channel != "linear" || failures[0].TaskID != "linear-1" {
		t.Fatalf("notification failures = %+v, %v", failures, err)
	}
	if after := taskStates(t, db); after != before || after.task != string(model.TaskCompleted) {
		t.Fatalf("task states changed: before %+v, after %+v", before, after)
	}
}

// 6章: an authentication error is not retried.
func TestAuthErrorFailsWithoutRetry(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusUnauthorized, commentBody: `{}`, issueBody: issueJSON(false)}
	db, worker, c, logs := setup(t, fake)
	before := taskStates(t, db)

	sendDue(t, worker)
	c.advance(time.Hour)
	sendDue(t, worker)

	if n := fake.commentCount(); n != 1 {
		t.Fatalf("commentCreate requests = %d; want 1", n)
	}
	if r := outboxRow(t, db, dedupeKey); r.status != "failed" || r.attempts != 1 {
		t.Fatalf("row = %+v; want failed after 1 attempt", r)
	}
	events := notificationEvents(t, db)
	if len(events) != 1 || events[0].kind != "notification_failed" || events[0].payload["channel"] != "linear" {
		t.Fatalf("events = %+v", events)
	}
	if after := taskStates(t, db); after != before {
		t.Fatalf("task states changed: before %+v, after %+v", before, after)
	}
	if strings.Contains(logs.String(), keyCanary) {
		t.Fatal("API key leaked into the log")
	}
}

// 6章: a deleted or archived Issue fails with reason=other and does not
// affect the task.
func TestRemovedIssueFailsWithReasonOther(t *testing.T) {
	gqlError := `{"errors":[{"message":"Entity not found","extensions":{"code":"INVALID_INPUT"}}]}`
	for name, issueBody := range map[string]string{
		"deleted":  `{"data":{"issue":null}}`,
		"archived": issueJSON(true),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: gqlError, issueBody: issueBody}
			db, worker, c, _ := setup(t, fake)
			before := taskStates(t, db)

			sendDue(t, worker)
			c.advance(time.Hour)
			sendDue(t, worker)

			if n := fake.commentCount(); n != 1 {
				t.Fatalf("commentCreate requests = %d; want 1", n)
			}
			if r := outboxRow(t, db, dedupeKey); r.status != "failed" || r.attempts != 1 {
				t.Fatalf("row = %+v; want failed after 1 attempt", r)
			}
			events := notificationEvents(t, db)
			if len(events) != 1 || events[0].kind != "notification_failed" || events[0].payload["reason"] != "other" ||
				events[0].payload["channel"] != "linear" {
				t.Fatalf("events = %+v", events)
			}
			if after := taskStates(t, db); after != before {
				t.Fatalf("task states changed: before %+v, after %+v", before, after)
			}
		})
	}
}

// An unusable key file sends nothing and leaves the row pending, like the
// Poller (spec 15 6章).
func TestUnusableKeyLeavesRowPending(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, _, _ := setup(t, fake)
	client, err := linear.NewClient(nil, "https://linear.invalid/graphql", filepath.Join(t.TempDir(), "missing"), nil)
	if err != nil {
		t.Fatal(err)
	}
	worker.Client = client
	sendDue(t, worker)
	if r := outboxRow(t, db, dedupeKey); r.status != "pending" || r.attempts != 0 {
		t.Fatalf("row = %+v; want untouched", r)
	}
}

// An intake hold has no Execution: the row is sent and recorded, and no
// execution event is written.
func TestIntakeHoldRowIsSentWithoutExecutionEvent(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, _, _ := setup(t, fake)
	mustExec(t, db, `UPDATE linear_outbox SET status = 'sent'`) // Set the succeeded row aside.
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
	sendDue(t, worker)
	if r := outboxRow(t, db, trigger.DedupeKey()); r.status != "sent" || r.commentID.String != "comment-1" {
		t.Fatalf("row = %+v", r)
	}
	if events := notificationEvents(t, db); len(events) != 0 {
		t.Fatalf("events = %+v; want none", events)
	}
}

// Pending rows are sent oldest first (3.4 step 2).
func TestRowsSentOldestFirst(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, _, _ := setup(t, fake)
	mustExec(t, db, `UPDATE linear_outbox SET created_at = '2026-11-08T00:00:02Z'`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := linearoutbox.EnqueueTx(context.Background(), tx, linearoutbox.Trigger{TaskID: "linear-1",
		Kind: linearoutbox.KindIntakeBlocked, TargetID: "routing"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE linear_outbox SET created_at = '2026-11-08T00:00:01Z' WHERE kind = 'intake_blocked'`)
	sendDue(t, worker)
	if len(fake.comments) != 2 || !strings.HasSuffix(fake.comments[0]["body"].(string), "linear-1:intake_blocked:routing") {
		t.Fatalf("comments = %v; want the older row first", fake.comments)
	}
}
