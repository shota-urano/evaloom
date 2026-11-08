package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
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

// The Control Plane starts the Linear comment Worker (spec 16 3.4 step 2),
// which sends a pending linear_outbox row and records it as sent.
func TestRunStartsLinearFeedbackWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	const now = "2026-11-08T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
			VALUES ('r1', 'r1', 'h', '/w', 'online', '` + now + `', '` + now + `')`,
		`INSERT INTO projects (id, name, local_repo_path, git_remote_url, runner_id, base_branch,
			default_role, context_location, copy_allowlist, allowed_operations, approval_policy, timeouts, created_at, updated_at)
			VALUES ('p1', 'P', '/repos/p', 'https://example.test/p.git', 'r1', 'main',
			'developer', '.', '[]', '[]', '{}', '{}', '` + now + `', '` + now + `')`,
		`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
			collaboration_mode, status, idempotency_key, created_at, updated_at, source)
			VALUES ('t1', 'p1', 'T', 'B', 'ok', 't', 'development', 'solo', 'completed', 'k1', '` + now + `', '` + now + `', 'linear')`,
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
	newLinearFeedback = func(db *sql.DB, logger *slog.Logger) (*linearfeedback.Worker, error) {
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
	db, err = store.Open(context.Background(), path)
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
	worker, err := newLinearFeedback(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := worker.Client.(*linear.Client); !ok || worker.Interval != linearfeedback.DefaultInterval {
		t.Fatalf("worker = %#v", worker)
	}
}

// evaloomd with -data-dir installs the artifact reader for 作業概要 (spec 16
// 3.3): a transition to succeeded while it runs writes a comment body with
// the summary of the Execution's harness_output artifact.
func TestRunWiresHarnessSummaryIntoLinearCommentBody(t *testing.T) {
	dir := t.TempDir()
	path, dataDir := filepath.Join(dir, "evaloom.db"), filepath.Join(dir, "data")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const now = "2026-11-08T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
			VALUES ('r1', 'r1', 'h', '/w', 'online', '` + now + `', '` + now + `')`,
		`INSERT INTO projects (id, name, local_repo_path, git_remote_url, runner_id, base_branch,
			default_role, context_location, copy_allowlist, allowed_operations, approval_policy, timeouts, created_at, updated_at)
			VALUES ('p1', 'P', '/repos/p', 'https://example.test/p.git', 'r1', 'main',
			'developer', '.', '[]', '[]', '{}', '{}', '` + now + `', '` + now + `')`,
		`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
			collaboration_mode, status, idempotency_key, created_at, updated_at, source)
			VALUES ('t1', 'p1', 'T', 'B', 'ok', 't', 'development', 'solo', 'verifying', 'k1', '` + now + `', '` + now + `', 'linear')`,
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
	_, err = files.Save(context.Background(), "out-1", "e1", "harness_output", true, strings.NewReader(`{"summary":"SUMMARY-FROM-ARTIFACT"}`))
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
	newLinearFeedback = func(db *sql.DB, logger *slog.Logger) (*linearfeedback.Worker, error) {
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
			FromTask: "verifying", FromExecution: "publishing", ToExecution: "succeeded"})
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
