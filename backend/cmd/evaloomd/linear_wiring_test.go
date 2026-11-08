package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// fakeLinear is an httptest stand-in for the Linear GraphQL API. It serves
// team-a's ai-ready list and per-Issue fetches from the current issues map.
type fakeLinear struct {
	mu       sync.Mutex
	issues   map[string]map[string]any
	comments []map[string]any // commentCreate variables, in order
}

func (l *fakeLinear) set(id string, issue map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.issues[id] = issue
}

func (l *fakeLinear) remove(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.issues, id)
}

func (l *fakeLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var data any
	switch {
	case strings.Contains(req.Query, "EvaloomReadyIssues"):
		nodes := []any{}
		for _, issue := range l.issues {
			if issue["team"].(map[string]any)["id"] == req.Variables["teamId"] && hasLabel(issue, req.Variables["label"]) &&
				issue["archivedAt"] == nil {
				nodes = append(nodes, issue)
			}
		}
		data = map[string]any{"issues": map[string]any{"nodes": nodes,
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}
	case strings.Contains(req.Query, "EvaloomIssue"):
		issue, ok := l.issues[req.Variables["id"].(string)]
		if !ok {
			data = map[string]any{"issue": nil}
		} else {
			data = map[string]any{"issue": issue}
		}
	case strings.Contains(req.Query, "EvaloomCommentCreate"):
		l.comments = append(l.comments, req.Variables)
		data = map[string]any{"commentCreate": map[string]any{"success": true,
			"comment": map[string]any{"id": fmt.Sprintf("comment-%d", len(l.comments))}}}
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func hasLabel(issue map[string]any, label any) bool {
	for _, node := range issue["labels"].(map[string]any)["nodes"].([]any) {
		if node.(map[string]any)["name"] == label {
			return true
		}
	}
	return false
}

func linearIssueJSON(id, body string, updated time.Time, labels ...string) map[string]any {
	nodes := []any{}
	for _, label := range labels {
		nodes = append(nodes, map[string]any{"name": label})
	}
	return map[string]any{"id": id, "identifier": "ENG-1", "url": "https://linear.app/x/ENG-1", "title": "Fix login",
		"description": body, "updatedAt": updated.Format(time.RFC3339Nano), "archivedAt": nil, "trashed": nil,
		"team": map[string]any{"id": "team-a"}, "state": map[string]any{"type": "unstarted"},
		"labels": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}
}

// setupLinearProject registers p1 for team-a and repo:p with a Default
// Harness assignment, so a routed Issue can be queued.
func setupLinearProject(t *testing.T, db *sql.DB) {
	t.Helper()
	execAll := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execAll(`INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('r1', 'r1', 'h', '/w', 'online', '2026-11-08T00:00:00Z', '2026-11-08T00:00:00Z')`)
	rows, err := db.Query(`SELECT name FROM pragma_table_info('projects') WHERE "notnull" = 1 AND dflt_value IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	columns := []string{"linear_team_id", "linear_routing_label"}
	values := []any{"team-a", "repo:p"}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		value := map[string]string{"id": "p1", "local_repo_path": "/repos/p1", "runner_id": "r1",
			"copy_allowlist": "[]", "allowed_operations": "{}", "approval_policy": "{}", "timeouts": "{}"}[column]
		if value == "" {
			value = "x"
		}
		columns = append(columns, column)
		values = append(values, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	execAll(`INSERT INTO projects (`+strings.Join(columns, ", ")+`) VALUES (?`+
		strings.Repeat(", ?", len(columns)-1)+`)`, values...)
	execAll(`INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url,
		supported_models, available_tools, tool_permissions, contract_version, created_at, updated_at)
		VALUES ('h1', 'Developer', 'developer', '["development"]', 'codex', '/repos/h', 'https://example.invalid/h.git',
		'["m"]', '[]', '{}', '1', 't', 't')`)
	execAll(`INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, definition,
		definition_hash, model_id, operation_status, registered_at)
		VALUES ('hp1', 'h1', '1', 'sha', 'v1', '{}', 'hash', 'm', 'provisional', 't')`)
	// default_role was filled with "x" above.
	execAll(`INSERT INTO project_harnesses (project_id, role, harness_id, profile_id, assigned_at)
		VALUES ('p1', 'x', 'h1', 'hp1', 't')`)
}

// The production wiring (linearPoller) carries one Issue through the
// Poller, Intake, TaskStarter (hold and recheck) and TaskCanceller against
// an httptest Linear API: held without acceptance criteria, queued after the
// Issue is fixed, then cancelled with actor system when ai-ready is removed.
func TestLinearPollerWiringStartsRechecksAndCancels(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setupLinearProject(t, db)

	fake := &fakeLinear{issues: map[string]map[string]any{}}
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	keyPath := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	poller := linearPoller(db, client, logger)
	poll := func() {
		t.Helper()
		if _, err := poller.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type taskRow struct {
		id, status string
		cause      sql.NullString
		executions int
	}
	readTask := func() taskRow {
		t.Helper()
		var row taskRow
		if err := db.QueryRow(`SELECT t.id, t.status, t.intake_block_cause,
			(SELECT count(*) FROM executions WHERE task_id = t.id)
			FROM tasks t JOIN linear_issue_links l ON l.task_id = t.id WHERE l.linear_issue_id = 'issue-1'`).
			Scan(&row.id, &row.status, &row.cause, &row.executions); err != nil {
			t.Fatal(err)
		}
		return row
	}

	poll() // first poll of team-a: baseline only
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	fake.set("issue-1", linearIssueJSON("issue-1", "no criteria", at, "ai-ready", "repo:p"))
	poll()
	if got := readTask(); got.status != "blocked" || got.cause.String != "missing_acceptance_criteria" || got.executions != 0 {
		t.Fatalf("held task = %+v", got)
	}
	fake.set("issue-1", linearIssueJSON("issue-1", "## Acceptance Criteria\n- login works", at.Add(time.Minute), "ai-ready", "repo:p"))
	poll()
	queued := readTask()
	if queued.status != "queued" || queued.cause.Valid || queued.executions != 1 {
		t.Fatalf("rechecked task = %+v", queued)
	}
	fake.set("issue-1", linearIssueJSON("issue-1", "## Acceptance Criteria\n- login works", at.Add(2*time.Minute), "repo:p"))
	poll()
	if got := readTask(); got.id != queued.id || got.status != "cancelled" {
		t.Fatalf("cancelled task = %+v", got)
	}
	var actor, reason string
	if err := db.QueryRow(`SELECT actor, json_extract(detail, '$.reason') FROM audit_logs
		WHERE action = 'task.cancel' AND target_id = ?`, queued.id).Scan(&actor, &reason); err != nil {
		t.Fatal(err)
	}
	if actor != "system" || reason != "linear_label_removed" {
		t.Fatalf("cancel audit = %s %s", actor, reason)
	}
}

// The production wiring keeps a deleted Issue's linear_issue_states row
// (spec 15 3.4, 3.7, Q78): deleting cancels the task and marks the row,
// restoring records it as skipped_restored without a task, and removing and
// re-adding ai-ready starts a task with a new idempotency key.
func TestLinearPollerWiringKeepsRemovedIssueRecord(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setupLinearProject(t, db)

	fake := &fakeLinear{issues: map[string]map[string]any{}}
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	keyPath := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	poller := linearPoller(db, client, logger)
	poll := func() {
		t.Helper()
		if _, err := poller.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type stateRow struct {
		aiReady   bool
		seq       int
		decision  string
		removedAt sql.NullString
	}
	readState := func() stateRow {
		t.Helper()
		var row stateRow
		if err := db.QueryRow(`SELECT ai_ready, attach_seq, last_decision, removed_at FROM linear_issue_states
			WHERE linear_issue_id = 'issue-1'`).Scan(&row.aiReady, &row.seq, &row.decision, &row.removedAt); err != nil {
			t.Fatal(err)
		}
		return row
	}
	counts := func() (tasks, executions int) {
		t.Helper()
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM executions)`).
			Scan(&tasks, &executions); err != nil {
			t.Fatal(err)
		}
		return tasks, executions
	}
	taskStatus := func(key string) string {
		t.Helper()
		var status string
		if err := db.QueryRow(`SELECT status FROM tasks WHERE idempotency_key = ?`, key).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	poll() // first poll of team-a: baseline only
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	body := "## Acceptance Criteria\n- login works"
	fake.set("issue-1", linearIssueJSON("issue-1", body, at, "ai-ready", "repo:p"))
	poll()
	if got := taskStatus("linear:issue-1:1"); got != "queued" {
		t.Fatalf("first task = %s", got)
	}

	fake.remove("issue-1") // deleted in Linear
	poll()
	if got := taskStatus("linear:issue-1:1"); got != "cancelled" {
		t.Fatalf("task after delete = %s", got)
	}
	if got := readState(); got.aiReady || got.seq != 1 || !got.removedAt.Valid {
		t.Fatalf("row after delete = %+v, want ai_ready=0 attach_seq 1 removed_at set", got)
	}
	tasks, executions := counts()

	fake.set("issue-1", linearIssueJSON("issue-1", body, at.Add(time.Minute), "ai-ready", "repo:p")) // restored
	poll()
	if got := readState(); !got.aiReady || got.seq != 1 || got.removedAt.Valid || got.decision != "skipped_restored" {
		t.Fatalf("row after restore = %+v", got)
	}
	if gotTasks, gotExecutions := counts(); gotTasks != tasks || gotExecutions != executions {
		t.Fatalf("restore created tasks/executions: %d/%d, want %d/%d", gotTasks, gotExecutions, tasks, executions)
	}

	fake.set("issue-1", linearIssueJSON("issue-1", body, at.Add(2*time.Minute), "repo:p"))
	poll()
	if got := readState(); got.aiReady || got.seq != 1 {
		t.Fatalf("row after removing ai-ready = %+v", got)
	}
	fake.set("issue-1", linearIssueJSON("issue-1", body, at.Add(3*time.Minute), "ai-ready", "repo:p"))
	poll()
	if got := readState(); !got.aiReady || got.seq != 2 || got.decision != "started" {
		t.Fatalf("row after re-adding ai-ready = %+v", got)
	}
	if got := taskStatus("linear:issue-1:2"); got != "queued" {
		t.Fatalf("new task = %s", got)
	}
}

// 受け入れ基準: missing_acceptance_criteria で保留したタスクの Issue に記入
// 依頼のコメントが1回付く。The production Poller holds the Issue, the
// feedback Worker sends the request to an httptest Linear API once, and
// later edits that still lack the criteria (rechecks with the same cause,
// spec 16 3.5 rule 3) send no second comment.
func TestLinearPollerWiringAsksForAcceptanceCriteriaOnce(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setupLinearProject(t, db)

	fake := &fakeLinear{issues: map[string]map[string]any{}}
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	keyPath := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	poller := linearPoller(db, client, logger)
	worker := &linearfeedback.Worker{DB: db, Client: client, Logger: logger}
	step := func() {
		t.Helper()
		if _, err := poller.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := worker.SendDue(ctx); err != nil {
			t.Fatal(err)
		}
	}

	step() // first poll of team-a: baseline only
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	fake.set("issue-1", linearIssueJSON("issue-1", "no criteria", at, "ai-ready", "repo:p"))
	step()
	for i := 1; i <= 2; i++ {
		fake.set("issue-1", linearIssueJSON("issue-1", fmt.Sprintf("still no criteria %d", i),
			at.Add(time.Duration(i)*time.Minute), "ai-ready", "repo:p"))
		step()
	}

	fake.mu.Lock()
	comments := fake.comments
	fake.mu.Unlock()
	if len(comments) != 1 || comments[0]["issueId"] != "issue-1" {
		t.Fatalf("comments = %v", comments)
	}
	body, _ := comments[0]["body"].(string)
	for _, want := range []string{"受け入れ条件の記入をお願いします", "Issue の本文に受け入れ条件が書かれていない",
		"「受け入れ条件」または「Acceptance Criteria」という見出し"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	var status, commentID string
	if err := db.QueryRow(`SELECT status, linear_comment_id FROM linear_outbox
		WHERE kind = 'missing_acceptance_criteria'`).Scan(&status, &commentID); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || commentID != "comment-1" {
		t.Fatalf("outbox = %s %s", status, commentID)
	}
}
