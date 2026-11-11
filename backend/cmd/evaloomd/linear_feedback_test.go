package main

import (
	"context"
	"database/sql"
	"github.com/shota-urano/evaloom/backend/internal/testregistry"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// commentClient stands in for Linear: it records created comments.
type commentClient struct{ created chan [2]string }

func (c commentClient) CheckKey() error { return nil }

func (c commentClient) CreateComment(_ context.Context, issueID, body string) (string, error) {
	c.created <- [2]string{issueID, body}
	return "comment-1", nil
}

func (c commentClient) Issue(context.Context, string) (linear.Issue, error) {
	return linear.Issue{}, nil
}

func (c commentClient) IssueComments(context.Context, string) ([]linear.Comment, error) {
	return nil, nil
}

func (c commentClient) IssueState(context.Context, string) (linear.IssueState, error) {
	return linear.IssueState{TeamID: "team-a", State: linear.WorkflowState{ID: "done", Name: "Done", Type: "completed"}}, nil
}

func (c commentClient) TeamStates(context.Context, string) ([]linear.WorkflowState, error) {
	return nil, nil
}

func (c commentClient) SetIssueState(context.Context, string, string) error { return nil }

// The Control Plane starts the Linear comment Worker (spec 16 3.4 step 2),
// which sends a pending linear_outbox row and records it as sent.
func TestRunStartsLinearFeedbackWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := testregistry.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	const now = "2026-11-08T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO runners (id, name, token_hash, status, last_heartbeat_at, created_at)
			VALUES ('r1', 'r1', 'h', 'online', '` + now + `', '` + now + `')`,
		`INSERT INTO projects (id, name, runner_id, harness_id, profile_id, created_at, updated_at)
			VALUES ('p1', 'P', 'r1', 'fixture-harness', 'fixture-profile', '` + now + `', '` + now + `')`,
		`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at, source)
			VALUES ('t1', 'p1', 'T', 'B', 'ok', 'solo', 'completed', 'k1', '` + now + `', '` + now + `', 'linear')`,
		`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
			VALUES ('e1', 't1', 1, 'continue', 'succeeded', '` + now + `')`,
		`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
			VALUES ('l1', 'issue-1', 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', 't1', 1, '` + now + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := linearoutbox.EnqueueTx(context.Background(), tx, linearoutbox.Trigger{TaskID: "t1",
		Kind: linearoutbox.KindSucceeded, TargetID: "e1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	originalPoller, originalFeedback := newLinearPoller, newLinearFeedback
	t.Cleanup(func() { newLinearPoller, newLinearFeedback = originalPoller, originalFeedback })
	newLinearPoller = func(db *sql.DB, logger *slog.Logger, _ int) (*linearintake.Poller, error) {
		return &linearintake.Poller{DB: db, Client: startupPollClient{make(chan string, 1)}, Logger: logger, Interval: time.Hour}, nil
	}
	created := make(chan [2]string, 1)
	newLinearFeedback = func(db *sql.DB, logger *slog.Logger, _ int) (*linearfeedback.Worker, error) {
		return &linearfeedback.Worker{DB: db, Client: commentClient{created}, Logger: logger, Interval: 10 * time.Millisecond}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, io.Discard) }()
	select {
	case got := <-created:
		if got[0] != "issue-1" {
			t.Fatalf("comment issue = %q", got[0])
		}
	case <-ctx.Done():
		t.Fatal("Control Plane did not send the pending Linear comment")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	db, err = testregistry.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status, commentID string
	if err := db.QueryRow(`SELECT status, linear_comment_id FROM linear_outbox`).Scan(&status, &commentID); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || commentID != "comment-1" {
		t.Fatalf("outbox row = %s/%s; want sent/comment-1", status, commentID)
	}
}

// Production builds the Worker with the real Linear client and the default
// interval; the key file is read only when a row is due.
func TestDefaultLinearFeedbackUsesProductionConfiguration(t *testing.T) {
	worker, err := newLinearFeedback(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 8080)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := worker.Client.(*linear.Client); !ok || worker.Interval != linearfeedback.DefaultInterval {
		t.Fatalf("worker = %#v", worker)
	}
}

// evaloomd with -data-dir installs the artifact reader for 作業概要 (spec 16
// 3.3): a transition to succeeded while it runs writes a comment body with
// the summary of the Execution's work_summary artifact.
func TestRunWiresHarnessSummaryIntoLinearCommentBody(t *testing.T) {
	dir := t.TempDir()
	path, dataDir := filepath.Join(dir, "evaloom.db"), filepath.Join(dir, "data")
	db, err := testregistry.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const now = "2026-11-08T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO runners (id, name, token_hash, status, last_heartbeat_at, created_at)
			VALUES ('r1', 'r1', 'h', 'online', '` + now + `', '` + now + `')`,
		`INSERT INTO projects (id, name, runner_id, harness_id, profile_id, created_at, updated_at)
			VALUES ('p1', 'P', 'r1', 'fixture-harness', 'fixture-profile', '` + now + `', '` + now + `')`,
		`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at, source)
			VALUES ('t1', 'p1', 'T', 'B', 'ok', 'solo', 'running', 'k1', '` + now + `', '` + now + `', 'linear')`,
		`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
			VALUES ('e1', 't1', 1, 'continue', 'publishing', '` + now + `')`,
		`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
			VALUES ('l1', 'issue-1', 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', 't1', 1, '` + now + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	files, err := store.NewArtifactStore(db, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = files.Save(context.Background(), store.Artifact{ID: "out-1", ExecutionID: "e1", Stage: "work", Kind: "work_summary", Name: "summary.md", Redacted: true}, strings.NewReader("SUMMARY-FROM-ARTIFACT"))
	files.Close()
	if err != nil {
		t.Fatal(err)
	}

	originalPoller, originalFeedback := newLinearPoller, newLinearFeedback
	t.Cleanup(func() { newLinearPoller, newLinearFeedback = originalPoller, originalFeedback })
	newLinearPoller = func(db *sql.DB, logger *slog.Logger, _ int) (*linearintake.Poller, error) {
		return &linearintake.Poller{DB: db, Client: startupPollClient{make(chan string, 1)}, Logger: logger, Interval: time.Hour}, nil
	}
	started := make(chan struct{})
	created := make(chan [2]string, 1)
	newLinearFeedback = func(db *sql.DB, logger *slog.Logger, _ int) (*linearfeedback.Worker, error) {
		close(started)
		return &linearfeedback.Worker{DB: db, Client: commentClient{created}, Logger: logger, Interval: 10 * time.Millisecond}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-db", path, "-port", "0", "-data-dir", dataDir, "-artifact-max-bytes", "1048576"}, io.Discard)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Control Plane did not start")
	}
	// The running Control Plane also writes this database (startup recovery,
	// Workers); retry while SQLite reports it busy.
	for {
		err := task.TransitionExecution(ctx, db, task.Transition{TaskID: "t1", ExecutionID: "e1",
			FromTask: "running", FromExecution: "publishing", ToExecution: "succeeded"})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") || ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case got := <-created:
		if !strings.Contains(got[1], "\n作業概要:\nSUMMARY-FROM-ARTIFACT\n") {
			t.Fatalf("comment body = %q", got[1])
		}
	case <-ctx.Done():
		t.Fatal("Control Plane did not send the Linear comment")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

// On macOS the production Worker alerts an authentication error through the
// macOS helper (spec 16 3.4.3); elsewhere it only logs.
func TestDefaultLinearFeedbackAlertsThroughMacOS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worker, err := newLinearFeedback(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 49123)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		if _, ok := worker.Alerter.(macOSLinearAlerter); !ok {
			t.Fatalf("alerter = %T; want macOSLinearAlerter", worker.Alerter)
		}
	} else if worker.Alerter != nil {
		t.Fatalf("alerter = %T; want nil off darwin", worker.Alerter)
	}
	alerter, ok := linearCommentAlerter("darwin", "/Users/owner", 49123).(macOSLinearAlerter)
	if !ok || alerter.macOS == nil {
		t.Fatalf("darwin alerter = %#v; want macOSLinearAlerter", alerter)
	}
	for _, goos := range []string{"linux", "windows"} {
		if got := linearCommentAlerter(goos, "/unused", 8080); got != nil {
			t.Fatalf("%s alerter = %#v; want nil", goos, got)
		}
	}
}

// 受け入れ基準: 認証エラーの通知の抑止を Linear Poller と送信 Worker で共通の
// 1つにする（spec 16 3.4.3、Q84）。The Control Plane gives both the same gate.
func TestRunSharesOneAuthAlertGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	originalPoller, originalFeedback := newLinearPoller, newLinearFeedback
	t.Cleanup(func() { newLinearPoller, newLinearFeedback = originalPoller, originalFeedback })
	var poller *linearintake.Poller
	var worker *linearfeedback.Worker
	newLinearPoller = func(db *sql.DB, logger *slog.Logger, _ int) (*linearintake.Poller, error) {
		poller = &linearintake.Poller{DB: db, Client: startupPollClient{make(chan string, 1)}, Logger: logger, Interval: time.Hour}
		return poller, nil
	}
	started := make(chan struct{})
	newLinearFeedback = func(db *sql.DB, logger *slog.Logger, _ int) (*linearfeedback.Worker, error) {
		worker = &linearfeedback.Worker{DB: db, Client: commentClient{make(chan [2]string, 1)}, Logger: logger, Interval: time.Hour}
		close(started)
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, io.Discard) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Control Plane did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if poller.AuthGate == nil || poller.AuthGate != worker.AuthGate {
		t.Fatalf("poller gate %p, worker gate %p; want one shared gate", poller.AuthGate, worker.AuthGate)
	}
}
