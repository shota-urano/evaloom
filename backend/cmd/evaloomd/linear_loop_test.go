package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// loopLinear is an httptest Linear GraphQL API that behaves like Linear for
// the circular-trigger checks (spec 16 3.5): creating a comment or changing
// Workflow state advances updatedAt without changing the labels or body.
// It also records the query text of every request, in order.
type loopLinear struct {
	mu           sync.Mutex
	issues       map[string]map[string]any
	comments     []map[string]any // commentCreate variables, in order
	stateUpdates []map[string]any // issueUpdate variables, in order
	queries      []string
}

var loopWorkflowStates = []linear.WorkflowState{
	{ID: "todo", Name: "Todo", Type: "unstarted"},
	{ID: "in-progress", Name: "In Progress", Type: "started"},
	{ID: "pr-review", Name: "PR Review", Type: "started"},
	{ID: "done", Name: "Done", Type: "completed"},
	{ID: "canceled", Name: "Canceled", Type: "canceled"},
}

func (l *loopLinear) set(id string, issue map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Intake only needs the type; state delivery also reads the ID and name.
	if state := issue["state"].(map[string]any); state["id"] == nil {
		for _, candidate := range loopWorkflowStates {
			if candidate.Type == state["type"] {
				issue["state"] = map[string]any{"id": candidate.ID, "name": candidate.Name, "type": candidate.Type}
				break
			}
		}
	}
	l.issues[id] = issue
}

func (l *loopLinear) updatedAt(id string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.issues[id]["updatedAt"].(string)
}

func (l *loopLinear) snapshot() (comments []map[string]any, queries []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.comments...), append([]string(nil), l.queries...)
}

func (l *loopLinear) stateSnapshot(id string) (linear.WorkflowState, []map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.issues[id]["state"].(map[string]any)
	return linear.WorkflowState{ID: s["id"].(string), Name: s["name"].(string), Type: s["type"].(string)},
		append([]map[string]any(nil), l.stateUpdates...)
}

// Called under mu for both comment and state mutations.
func (l *loopLinear) advanceIssue(id string) (map[string]any, error) {
	issue := l.issues[id]
	updated, err := time.Parse(time.RFC3339Nano, issue["updatedAt"].(string))
	if err != nil {
		return nil, err
	}
	next := map[string]any{}
	for k, v := range issue {
		next[k] = v
	}
	next["updatedAt"] = updated.Add(time.Minute).Format(time.RFC3339Nano)
	l.issues[id] = next
	return next, nil
}

func (l *loopLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	l.queries = append(l.queries, req.Query)
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
	case strings.Contains(req.Query, "EvaloomIssueComments"):
		nodes := []any{}
		for i, c := range l.comments {
			if c["issueId"] == req.Variables["id"] {
				nodes = append(nodes, map[string]any{"id": fmt.Sprintf("comment-%d", i+1), "body": c["body"]})
			}
		}
		data = map[string]any{"issue": map[string]any{"id": req.Variables["id"], "comments": map[string]any{
			"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}}
	case strings.Contains(req.Query, "EvaloomTeamStates"):
		data = map[string]any{"team": map[string]any{"id": req.Variables["id"], "states": map[string]any{
			"nodes": loopWorkflowStates, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}}
	case strings.Contains(req.Query, "EvaloomIssueSetState"):
		id, _ := req.Variables["id"].(string)
		if _, ok := l.issues[id]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, state := range loopWorkflowStates {
			if state.ID != req.Variables["stateId"] {
				continue
			}
			issue, err := l.advanceIssue(id)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			issue["state"] = map[string]any{"id": state.ID, "name": state.Name, "type": state.Type}
			l.stateUpdates = append(l.stateUpdates, req.Variables)
			data = map[string]any{"issueUpdate": map[string]any{"success": true}}
			break
		}
		if data == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	case strings.Contains(req.Query, "EvaloomIssue"):
		if issue, ok := l.issues[req.Variables["id"].(string)]; ok {
			data = map[string]any{"issue": issue}
		} else {
			data = map[string]any{"issue": nil}
		}
	case strings.Contains(req.Query, "EvaloomCommentCreate"):
		id, _ := req.Variables["issueId"].(string)
		_, ok := l.issues[id]
		if !ok {
			data = map[string]any{"commentCreate": nil}
			break
		}
		l.comments = append(l.comments, req.Variables)
		// Linear moves the Issue's updatedAt when a comment is added.
		if _, err := l.advanceIssue(id); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data = map[string]any{"commentCreate": map[string]any{"success": true,
			"comment": map[string]any{"id": fmt.Sprintf("comment-%d", len(l.comments))}}}
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// loopHarness wires the production Poller (linearPoller) and the feedback
// Worker to one real linear.Client pointed at a loopLinear server.
type loopHarness struct {
	db     *sql.DB
	fake   *loopLinear
	client *linear.Client
	poller *linearintake.Poller
	worker *linearfeedback.Worker
}

func newLoopHarness(t *testing.T) *loopHarness {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setupLinearProject(t, db)
	fake := &loopLinear{issues: map[string]map[string]any{}}
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	keyPath := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	return &loopHarness{db: db, fake: fake, client: client, poller: linearPoller(db, client, logger),
		worker: &linearfeedback.Worker{DB: db, Client: client, Logger: logger}}
}

func (h *loopHarness) poll(t *testing.T) {
	t.Helper()
	if _, err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *loopHarness) send(t *testing.T) {
	t.Helper()
	if err := h.worker.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type loopCounts struct{ tasks, executions, outbox, recheckAudits int }

func (h *loopHarness) counts(t *testing.T) loopCounts {
	t.Helper()
	var c loopCounts
	if err := h.db.QueryRow(`SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM executions),
		(SELECT count(*) FROM linear_outbox),
		(SELECT count(*) FROM audit_logs WHERE action = 'task.intake_recheck')`).
		Scan(&c.tasks, &c.executions, &c.outbox, &c.recheckAudits); err != nil {
		t.Fatal(err)
	}
	return c
}

type loopState struct {
	aiReady   bool
	seq       int
	decision  string
	updatedAt string
}

func (h *loopHarness) state(t *testing.T, issueID string) loopState {
	t.Helper()
	var s loopState
	if err := h.db.QueryRow(`SELECT ai_ready, attach_seq, last_decision, issue_updated_at
		FROM linear_issue_states WHERE linear_issue_id = ?`, issueID).
		Scan(&s.aiReady, &s.seq, &s.decision, &s.updatedAt); err != nil {
		t.Fatal(err)
	}
	return s
}

func sameInstant(t *testing.T, a, b string) bool {
	t.Helper()
	ta, err := time.Parse(time.RFC3339Nano, a)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		t.Fatal(err)
	}
	return ta.Equal(tb)
}

// 受け入れ基準: ai-ready が付いたままの Issue に Evaloom がコメントを付け、
// 更新時刻が進んだ後の照会でタスクが作られない (spec 16 3.5 rule 1-2).
// The task finishes (terminal, so the "one non-terminal task" guard of spec
// 15 3.5 rule 3 does not apply), the Worker posts the succeeded comment, the
// fake Linear advances updatedAt, and the next polls start nothing.
func TestLinearCommentOnAttachedIssueDoesNotRestart(t *testing.T) {
	h := newLoopHarness(t)
	ctx := context.Background()
	h.poll(t) // first poll of team-a: baseline only
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	h.fake.set("issue-1", linearIssueJSON("issue-1", "## Acceptance Criteria\n- login works", at, "ai-ready", "route:p"))
	h.poll(t)

	var taskID, executionID string
	if err := h.db.QueryRow(`SELECT t.id, e.id FROM tasks t JOIN executions e ON e.task_id = t.id
		WHERE t.idempotency_key = 'linear:issue-1:1' AND t.status = 'queued'`).Scan(&taskID, &executionID); err != nil {
		t.Fatalf("started task: %v", err)
	}
	// Run the Execution to the end through the production transition, which
	// writes the succeeded row of linear_outbox in the same transaction.
	if _, err := h.db.Exec(`UPDATE executions SET status = 'publishing' WHERE id = ?`, executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE tasks SET status = 'verifying' WHERE id = ?`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := task.TransitionExecution(ctx, h.db, task.Transition{TaskID: taskID, ExecutionID: executionID,
		FromTask: "verifying", FromExecution: "publishing", ToExecution: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	var taskStatus string
	if err := h.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "completed" {
		t.Fatalf("task status = %s, want completed (terminal)", taskStatus)
	}
	before := h.state(t, "issue-1")

	h.send(t)
	comments, _ := h.fake.snapshot()
	if len(comments) != 1 || comments[0]["issueId"] != "issue-1" {
		t.Fatalf("comments = %v, want the succeeded comment on issue-1", comments)
	}
	advanced := h.fake.updatedAt("issue-1")
	if sameInstant(t, advanced, before.updatedAt) {
		t.Fatalf("fake Linear did not advance updatedAt (%s)", advanced)
	}
	afterComment := h.counts(t)

	for i := 0; i < 2; i++ {
		h.poll(t)
		h.send(t)
	}
	got := h.state(t, "issue-1")
	if !sameInstant(t, got.updatedAt, advanced) {
		t.Fatalf("poll did not see the advanced updatedAt: state %s, Linear %s", got.updatedAt, advanced)
	}
	if !got.aiReady || got.seq != before.seq || got.decision != "started" {
		t.Fatalf("issue state after comment = %+v, before = %+v", got, before)
	}
	if c := h.counts(t); c != afterComment || c.tasks != 1 || c.executions != 1 {
		t.Fatalf("counts after polls = %+v, want %+v with 1 task and 1 execution", c, afterComment)
	}
	if comments, _ := h.fake.snapshot(); len(comments) != 1 {
		t.Fatalf("comments after polls = %d, want 1", len(comments))
	}
}

// 受け入れ基準: 保留中のタスクの Issue に Evaloom が保留のコメントを付けた後の
// 照会で、同じ理由のコメントがもう1回作られない (spec 16 3.5 rule 3). Both
// hold kinds are covered: missing_acceptance_criteria and a routing hold.
// The hold comment advances updatedAt, so the next poll does recheck the
// held task (spec 15 3.6.3), gets the same cause and writes nothing.
func TestLinearHoldCommentDoesNotRepeatOnRecheck(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind, cause string
		labels                  []string
	}{
		{name: "missing acceptance criteria", body: "no criteria", kind: "missing_acceptance_criteria",
			cause: "missing_acceptance_criteria", labels: []string{"ai-ready", "route:p"}},
		{name: "routing", body: "## Acceptance Criteria\n- login works", kind: "intake_blocked",
			labels: []string{"ai-ready", "route:unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t)
			h.poll(t) // first poll of team-a: baseline only
			at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
			h.fake.set("issue-1", linearIssueJSON("issue-1", tc.body, at, tc.labels...))
			h.poll(t)
			var status, cause string
			if err := h.db.QueryRow(`SELECT status, intake_block_cause FROM tasks
				WHERE idempotency_key = 'linear:issue-1:1'`).Scan(&status, &cause); err != nil {
				t.Fatal(err)
			}
			if status != "blocked" || (tc.cause != "" && cause != tc.cause) {
				t.Fatalf("held task = %s/%s", status, cause)
			}
			before := h.state(t, "issue-1")

			h.send(t) // the hold comment
			comments, _ := h.fake.snapshot()
			if len(comments) != 1 || comments[0]["issueId"] != "issue-1" {
				t.Fatalf("hold comments = %v", comments)
			}
			advanced := h.fake.updatedAt("issue-1")
			if sameInstant(t, advanced, before.updatedAt) {
				t.Fatalf("fake Linear did not advance updatedAt (%s)", advanced)
			}
			afterComment := h.counts(t)

			for i := 0; i < 2; i++ {
				h.poll(t)
				h.send(t)
			}
			got := h.state(t, "issue-1")
			if !sameInstant(t, got.updatedAt, advanced) {
				t.Fatalf("poll did not see the advanced updatedAt: state %s, Linear %s", got.updatedAt, advanced)
			}
			if got.seq != before.seq || got.decision != "held" {
				t.Fatalf("issue state after comment = %+v, before = %+v", got, before)
			}
			if c := h.counts(t); c != afterComment || c.tasks != 1 || c.executions != 0 || c.recheckAudits != 0 {
				t.Fatalf("counts after polls = %+v, want %+v (1 task, 0 executions, no recheck change)", c, afterComment)
			}
			var rows int
			if err := h.db.QueryRow(`SELECT count(*) FROM linear_outbox WHERE kind = ?`, tc.kind).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("%s outbox rows = %d, want 1", tc.kind, rows)
			}
			if comments, _ := h.fake.snapshot(); len(comments) != 1 {
				t.Fatalf("comments after polls = %d, want 1", len(comments))
			}
		})
	}
}

// 受け入れ基準: Linear クライアントの照会がコメントを要求しない (spec 15 3.2,
// spec 16 3.5 rule 2). Every GraphQL request the production Poller sends —
// the ai-ready list and the refetch of an Issue that left the list — is
// checked for a comment selection. The reconciliation read IssueComments
// (spec 16 3.4) is the feedback Worker's path, not polling; it is called
// here only to show the same check does catch a comment request.
func TestLinearPollQueriesDoNotRequestComments(t *testing.T) {
	h := newLoopHarness(t)
	h.poll(t) // baseline
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	h.fake.set("issue-1", linearIssueJSON("issue-1", "## Acceptance Criteria\n- login works", at, "ai-ready", "route:p"))
	h.poll(t) // list: attach
	h.fake.set("issue-1", linearIssueJSON("issue-1", "## Acceptance Criteria\n- login works", at.Add(time.Minute), "route:p"))
	h.poll(t) // list + per-Issue refetch: detach
	_, queries := h.fake.snapshot()

	requestsComments := func(query string) bool { return strings.Contains(strings.ToLower(query), "comment") }
	var lists, refetches int
	for _, q := range queries {
		switch {
		case strings.Contains(q, "query EvaloomReadyIssues("):
			lists++
		case strings.Contains(q, "query EvaloomIssue("):
			refetches++
		default:
			t.Errorf("poll sent an unexpected request:\n%s", q)
		}
		if requestsComments(q) {
			t.Errorf("poll query requests comments:\n%s", q)
		}
	}
	if lists != 3 || refetches != 1 {
		t.Fatalf("poll requests: %d lists, %d refetches; want 3 and 1", lists, refetches)
	}

	if _, err := h.client.IssueComments(context.Background(), "issue-1"); err != nil {
		t.Fatal(err)
	}
	_, queries = h.fake.snapshot()
	if last := queries[len(queries)-1]; !requestsComments(last) {
		t.Fatalf("check does not detect the comment read:\n%s", last)
	}
}
