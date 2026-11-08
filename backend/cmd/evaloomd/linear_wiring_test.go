package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// fakeLinear is an httptest stand-in for the Linear GraphQL API. It serves
// team-a's ai-ready list and per-Issue fetches from the current issues map.
type fakeLinear struct {
	mu     sync.Mutex
	issues map[string]map[string]any
}

func (l *fakeLinear) set(id string, issue map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.issues[id] = issue
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
			if issue["team"].(map[string]any)["id"] == req.Variables["teamId"] && hasLabel(issue, req.Variables["label"]) {
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
