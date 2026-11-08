package linearintake

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const criteriaBody = "Fix the login.\n\n## Acceptance Criteria\n- login works\n- tests pass\n\n## Notes\nnone"

// startDB registers p1 (team-a, repo:one) and p2 (team-b, repo:two) with a
// Default Harness assignment, and p3 (team-a, repo:three) without one.
func startDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:two")
	addProject(t, db, "p3", "team-a", "repo:three")
	// addProject fills every column with "x"; the JSON columns need JSON.
	exec(t, db, `UPDATE projects SET copy_allowlist = '[]', allowed_operations = '{}',
		approval_policy = '{}', timeouts = '{}'`)
	exec(t, db, `INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url,
		supported_models, available_tools, tool_permissions, contract_version, created_at, updated_at)
		VALUES ('h1', 'Developer', 'developer', '["development"]', 'codex', '/repos/h', 'https://example.invalid/h.git',
		'["m"]', '[]', '{}', '1', 't', 't')`)
	exec(t, db, `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, definition,
		definition_hash, model_id, operation_status, registered_at)
		VALUES ('hp1', 'h1', '1', 'sha', 'v1', '{}', 'hash', 'm', 'provisional', 't')`)
	for _, project := range []string{"p1", "p2"} {
		// addProject fills default_role with "x".
		exec(t, db, `INSERT INTO project_harnesses (project_id, role, harness_id, profile_id, assigned_at)
			VALUES (?, 'x', 'h1', 'hp1', 't')`, project)
	}
	return db
}

func routedIssue(id string, labels ...string) linear.Issue {
	return linear.Issue{ID: id, Identifier: "ENG-" + id, URL: "https://linear.app/x/ENG-" + id,
		Title: "title " + id, Description: criteriaBody, TeamID: "team-a",
		Labels: append([]string{"ai-ready"}, labels...), StateType: "unstarted", UpdatedAt: base}
}

func start(t *testing.T, db *sql.DB, issue linear.Issue, seq int) (Decision, error) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	starter := &TaskStarter{Now: func() time.Time { return base }}
	d, err := starter.StartLinearTask(context.Background(), tx, issue, seq)
	if err != nil {
		return "", err
	}
	return d, tx.Commit()
}

type startedTask struct {
	id, status, criteria, shortName, title, body, source string
	project, cause                                       sql.NullString
	executions                                           int
}

func readStarted(t *testing.T, db *sql.DB, issueID string, seq int) startedTask {
	t.Helper()
	var s startedTask
	err := db.QueryRow(`SELECT t.id, t.status, t.acceptance_criteria, t.short_name, t.title, t.body, t.source,
		t.project_id, t.intake_block_cause, (SELECT count(*) FROM executions e WHERE e.task_id = t.id)
		FROM tasks t JOIN linear_issue_links l ON l.task_id = t.id
		WHERE l.linear_issue_id = ? AND l.attach_seq = ? AND t.idempotency_key = ?`,
		issueID, seq, "linear:"+issueID+":"+strconv.Itoa(seq)).Scan(&s.id, &s.status, &s.criteria, &s.shortName,
		&s.title, &s.body, &s.source, &s.project, &s.cause, &s.executions)
	if err != nil {
		t.Fatalf("read task of %s/%d: %v", issueID, seq, err)
	}
	return s
}

// The routed Issue carries no Linear Project: linear.Issue has no Project
// field and routing uses only Team and label (spec 15 1章, requirements 6.1).
func TestStartLinearTaskQueuesRoutedIssue(t *testing.T) {
	db := startDB(t)
	issue := routedIssue("1", "repo:one", "bug")
	d, err := start(t, db, issue, 2)
	if err != nil || d != DecisionStarted {
		t.Fatalf("decision = %q, err = %v", d, err)
	}
	got := readStarted(t, db, "1", 2)
	if got.status != "queued" || got.project.String != "p1" || got.cause.Valid || got.source != "linear" ||
		got.criteria != "- login works\n- tests pass" || got.shortName != "eng-1" ||
		got.title != "title 1" || got.body != criteriaBody || got.executions != 1 {
		t.Fatalf("task = %+v", got)
	}
	var seq int
	var mode, status, events string
	if err := db.QueryRow(`SELECT seq, mode, status, (SELECT group_concat(payload) FROM execution_events
		WHERE execution_id = e.id AND type = 'status_changed') FROM executions e WHERE task_id = ?`, got.id).
		Scan(&seq, &mode, &status, &events); err != nil {
		t.Fatal(err)
	}
	if seq != 1 || mode != "continue" || status != "pending" || events != `{"from":"received","to":"queued","reason":"task_created"}` {
		t.Fatalf("execution = %d %s %s %s", seq, mode, status, events)
	}
	var url, team, actor string
	if err := db.QueryRow(`SELECT l.issue_url, l.team_id, a.actor FROM linear_issue_links l
		JOIN audit_logs a ON a.target_id = l.task_id AND a.action = 'task.create' WHERE l.task_id = ?`, got.id).
		Scan(&url, &team, &actor); err != nil {
		t.Fatal(err)
	}
	if url != issue.URL || team != "team-a" || actor != "system" {
		t.Fatalf("link/audit = %s %s %s", url, team, actor)
	}
	// The task is visible through the owner task list like a manual one.
	tasks, err := store.ListTasks(context.Background(), db, store.TaskFilter{}, 0, 10)
	if err != nil || len(tasks) != 1 || tasks[0].LatestExecution == nil || tasks[0].ProjectID != "p1" {
		t.Fatalf("list = %+v, err = %v", tasks, err)
	}
}

func TestStartLinearTaskHoldsUnroutableIssue(t *testing.T) {
	for _, test := range []struct {
		name   string
		issue  linear.Issue
		cause  BlockCause
		projID string
	}{
		{"two routing labels", routedIssue("1", "repo:one", "repo:three"), CauseRoutingMultipleLabels, ""},
		{"unregistered label", routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel, ""},
		{"no routing label", routedIssue("1", "bug"), CauseRoutingNoLabel, ""},
		{"team mismatch", routedIssue("1", "repo:two"), CauseRoutingTeamMismatch, ""},
		{"no assignment", routedIssue("1", "repo:three"), CauseConfigMissingAssignment, "p3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := startDB(t)
			d, err := start(t, db, test.issue, 1)
			if err != nil || d != DecisionHeld {
				t.Fatalf("decision = %q, err = %v", d, err)
			}
			got := readStarted(t, db, "1", 1)
			if got.status != "blocked" || got.cause.String != string(test.cause) || got.executions != 0 ||
				got.project.String != test.projID || got.project.Valid != (test.projID != "") ||
				got.criteria != "- login works\n- tests pass" {
				t.Fatalf("task = %+v", got)
			}
			// A held task without a Project must not break the task list.
			tasks, err := store.ListTasks(context.Background(), db, store.TaskFilter{}, 0, 10)
			if err != nil || len(tasks) != 1 || tasks[0].LatestExecution != nil || tasks[0].ProjectID != test.projID {
				t.Fatalf("list = %+v, err = %v", tasks, err)
			}
		})
	}
}

func TestStartLinearTaskHoldsIssueWithoutCriteria(t *testing.T) {
	for _, test := range []struct {
		name, body string
		labels     []string
		projID     string
	}{
		{"no heading", "Fix the login.\n\n## Notes\n- tests pass", []string{"repo:one"}, "p1"},
		{"blank section", "Fix it.\n## 受け入れ条件\n   \n\n## Notes\nx", []string{"repo:one"}, "p1"},
		{"empty body", "", []string{"repo:one"}, "p1"},
		// Both routing and criteria fail: only this cause permits the empty
		// acceptance_criteria the schema requires (spec 15 3.8).
		{"also unroutable", "no criteria", []string{"repo:unknown"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := startDB(t)
			issue := routedIssue("1", test.labels...)
			issue.Description = test.body
			d, err := start(t, db, issue, 1)
			if err != nil || d != DecisionHeld {
				t.Fatalf("decision = %q, err = %v", d, err)
			}
			got := readStarted(t, db, "1", 1)
			if got.status != "blocked" || got.cause.String != string(CauseMissingAcceptanceCriteria) ||
				got.criteria != "" || got.executions != 0 || got.project.String != test.projID || got.body != test.body {
				t.Fatalf("task = %+v", got)
			}
		})
	}
}

// Manual tasks keep rejecting an empty acceptance_criteria (spec 15 3.8).
func TestManualTaskWithoutCriteriaIsRejected(t *testing.T) {
	db := startDB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task := model.Task{ProjectID: "p1", Title: "t", Body: "b", AcceptanceCriteria: "", ShortName: "task",
		TaskType: "development", CollaborationMode: "solo", Status: model.TaskReceived, IdempotencyKey: "k",
		CreatedAt: "t", UpdatedAt: "t"}
	if err := store.InsertTask(context.Background(), tx, &task); err == nil {
		t.Fatal("manual task without acceptance criteria was saved")
	}
	// A Linear task that is not held for missing criteria is rejected too.
	task.IdempotencyKey = "k2"
	if err := store.InsertLinearTask(context.Background(), tx, &task, ""); err == nil {
		t.Fatal("routed linear task without acceptance criteria was saved")
	}
}

// A failed task creation leaves linear_issue_states unchanged so the next
// poll decides again (spec 15 3.5, 6章), and no task is created twice.
func TestIntakeWithTaskStarterRetriesFailedCreation(t *testing.T) {
	db := startDB(t)
	canceller := &recordingCanceller{}
	in := &Intake{DB: db, Fetcher: &stubFetcher{}, Starter: &TaskStarter{Now: func() time.Time { return base }},
		Canceller: canceller, Now: func() time.Time { return base }}
	round := func(issues ...linear.Issue) error {
		teams, err := store.SyncLinearPollTeams(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		for _, team := range teams {
			if team.TeamID == "team-a" {
				return in.HandleTeamIssues(context.Background(), team, issues)
			}
		}
		t.Fatal("team-a not polled")
		return nil
	}
	if err := round(); err != nil {
		t.Fatal(err)
	}
	// Occupy the idempotency key so creation fails inside the transaction.
	exec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
		collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES ('blocker', 'p1', 't', 'b', 'ac', 's', 'development', 'solo', 'completed', 'linear:1:1', 't', 't')`)
	issue := routedIssue("1", "repo:one")
	if err := round(issue); err == nil {
		t.Fatal("failed creation was not reported")
	}
	if _, ok := readState(t, db, "1"); ok {
		t.Fatal("linear_issue_states changed by a failed creation")
	}
	if n := countLinks(t, db, "1"); n != 0 {
		t.Fatalf("links after failure = %d", n)
	}
	exec(t, db, `DELETE FROM tasks WHERE id = 'blocker'`)
	for range 2 {
		if err := round(issue); err != nil {
			t.Fatal(err)
		}
	}
	if s := mustState(t, db, "1"); s.seq != 1 || s.decision != string(DecisionStarted) {
		t.Fatalf("state = %+v", s)
	}
	if got := readStarted(t, db, "1", 1); got.status != "queued" || got.executions != 1 || countLinks(t, db, "1") != 1 {
		t.Fatalf("task = %+v", got)
	}
}

func countLinks(t *testing.T, db *sql.DB, issueID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM linear_issue_links WHERE linear_issue_id = ?`, issueID).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return n
}

func TestExtractAcceptanceCriteria(t *testing.T) {
	for _, test := range []struct {
		name, body, want string
		ok               bool
	}{
		{"english", criteriaBody, "- login works\n- tests pass", true},
		{"japanese to end", "本文\n### 受け入れ条件\n- 動く\r\n- 落ちない\r\n", "- 動く\n- 落ちない", true},
		{"case and closing hashes", "# ACCEPTANCE   criteria ##\nok\n# Next\nno", "ok", true},
		{"deeper heading stays inside", "## Acceptance Criteria\na\n### Detail\nb\n## Next\nc", "a\n### Detail\nb", true},
		{"higher heading ends", "### Acceptance Criteria\na\n# Top\nb", "a", true},
		{"heading in code fence ignored", "```\n## Acceptance Criteria\nx\n```\nbody", "", false},
		{"fence inside section kept", "## Acceptance Criteria\n```\n# not a heading\n```\n## End", "```\n# not a heading\n```", true},
		{"other heading text", "## Acceptance Criteria (draft)\nx", "", false},
		{"not a heading", "Acceptance Criteria\nx", "", false},
		{"blank", "## Acceptance Criteria\n \t\n## Next\nx", "", false},
		{"first heading wins", "## 受け入れ条件\na\n## Acceptance Criteria\nb", "a", true},
		{"no space after hashes", "##Acceptance Criteria\nx", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ExtractAcceptanceCriteria(test.body)
			if got != test.want || ok != test.ok {
				t.Fatalf("got %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}
