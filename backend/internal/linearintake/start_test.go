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

// startDB registers three routed Projects on the same required Profile.
func startDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openDB(t)
	exec(t, db, `UPDATE harness_profiles SET definition = json_set(definition, '$.acceptanceCriteria', 'required') WHERE id = 'fixture-profile'`)
	addProject(t, db, "p1", "team-a", "route:one")
	addProject(t, db, "p2", "team-b", "route:two")
	addProject(t, db, "p3", "team-a", "route:three")

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
	err := db.QueryRow(`SELECT t.id, t.status, t.acceptance_criteria, '', t.title, t.body, t.source,
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
	issue := routedIssue("1", "route:one", "bug")
	d, err := start(t, db, issue, 2)
	if err != nil || d != DecisionStarted {
		t.Fatalf("decision = %q, err = %v", d, err)
	}
	got := readStarted(t, db, "1", 2)
	if got.status != "queued" || got.project.String != "p1" || got.cause.Valid || got.source != "linear" ||
		got.criteria != "- login works\n- tests pass" || got.shortName != "" ||
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

// 受け入れ基準: 振り分けのラベルが無い・routes に無い・2つ以上の Issue では、routing の
// タスクと queued の route_proposals ができ、Execution と保留の理由はできない（spec 15
// 3.6 step 3, 3.6.4 step 1）。
func TestStartLinearTaskRoutesUnroutableIssueToProposal(t *testing.T) {
	for _, test := range []struct {
		name    string
		issue   linear.Issue
		trigger RouteTrigger
		seen    string
	}{
		{"two routing labels", routedIssue("1", "route:one", "route:three"), TriggerMultipleLabels, `["route:one","route:three"]`},
		{"unregistered label", routedIssue("1", "route:unknown"), TriggerUnknownLabel, `["route:unknown"]`},
		{"no routing label", routedIssue("1", "bug"), TriggerNoLabel, `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := startDB(t)
			// A direct special-harness route without a default Profile is a
			// candidate too; its description is unknown (null).
			exec(t, db, `INSERT INTO routes VALUES ('route:direct',NULL,'fixture-harness','t','t')`)
			exec(t, db, `UPDATE harness_profiles SET definition = json_set(definition, '$.description', 'fixture desc') WHERE id = 'fixture-profile'`)
			d, err := start(t, db, test.issue, 1)
			if err != nil || d != DecisionRouting {
				t.Fatalf("decision = %q, err = %v", d, err)
			}
			got := readStarted(t, db, "1", 1)
			if got.status != "routing" || got.cause.Valid || got.executions != 0 || got.project.Valid ||
				got.criteria != "- login works\n- tests pass" {
				t.Fatalf("task = %+v", got)
			}
			var harness sql.NullString
			if err := db.QueryRow(`SELECT harness_id FROM tasks WHERE id = ?`, got.id).Scan(&harness); err != nil || harness.Valid {
				t.Fatalf("harness = %v, err = %v", harness, err)
			}
			var trigger, seen, candidates, status, created string
			var runner, decision sql.NullString
			if err := db.QueryRow(`SELECT trigger, seen_labels, candidates, status, runner_id, decision_id, created_at
				FROM route_proposals WHERE task_id = ?`, got.id).
				Scan(&trigger, &seen, &candidates, &status, &runner, &decision, &created); err != nil {
				t.Fatal(err)
			}
			wantCandidates := `[{"label":"route:direct","kind":"harness","id":"fixture-harness","description":null},` +
				`{"label":"route:one","kind":"project","id":"p1","description":"fixture desc"},` +
				`{"label":"route:three","kind":"project","id":"p3","description":"fixture desc"},` +
				`{"label":"route:two","kind":"project","id":"p2","description":"fixture desc"}]`
			if trigger != string(test.trigger) || seen != test.seen || candidates != wantCandidates ||
				status != "queued" || runner.Valid || decision.Valid || created != base.UTC().Format(time.RFC3339) {
				t.Fatalf("proposal = %s %s %s %s %v %v %s", trigger, seen, candidates, status, runner, decision, created)
			}
			// Routing is not a hold: no hold comment, no owner decision yet.
			if comments := outboxKeys(t, db, got.id); len(comments) != 0 {
				t.Fatalf("routing wrote comments: %v", comments)
			}
			if n := countRows(t, db, `SELECT count(*) FROM owner_decisions WHERE task_id = ?`, got.id); n != 0 {
				t.Fatalf("owner decisions = %d", n)
			}
			// A routing task without a Project must not break the task list.
			tasks, err := store.ListTasks(context.Background(), db, store.TaskFilter{}, 0, 10)
			if err != nil || len(tasks) != 1 || tasks[0].LatestExecution != nil || tasks[0].ProjectID != "" {
				t.Fatalf("list = %+v, err = %v", tasks, err)
			}
		})
	}
}

// spec 15 3.6.4 steps 2 and 5: with routing.proposer.model null (also the
// default), the proposal is failed at once without a Runner (error_class
// NULL) and a route decision without a proposal is opened, with its
// "管理画面で判断待ち" comment that has no Execution (16 3.2, 3.4.2).
func TestStartLinearTaskWithoutProposerModelOpensDecision(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *sql.DB)
	}{
		{"model null", func(t *testing.T, db *sql.DB) { setProposerModel(t, db, "null") }},
		{"default setting", func(t *testing.T, db *sql.DB) { exec(t, db, `DELETE FROM settings WHERE key = 'routing.proposer'`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := startDB(t)
			test.setup(t, db)
			d, err := start(t, db, routedIssue("1", "bug"), 1)
			if err != nil || d != DecisionRouting {
				t.Fatalf("decision = %q, err = %v", d, err)
			}
			got := readStarted(t, db, "1", 1)
			if got.status != "routing" || got.executions != 0 {
				t.Fatalf("task = %+v", got)
			}
			var proposalID, decisionID string
			if err := db.QueryRow(`SELECT p.id, d.id FROM route_proposals p JOIN owner_decisions d ON d.id = p.decision_id
				WHERE p.task_id = ? AND p.status = 'failed' AND p.error_class IS NULL AND p.runner_id IS NULL
				AND p.proposed_label IS NULL AND p.finished_at IS NOT NULL
				AND d.kind = 'route' AND d.status = 'open' AND d.subject_id = p.id AND d.basis = p.id
				AND d.execution_id IS NULL`, got.id).Scan(&proposalID, &decisionID); err != nil {
				t.Fatalf("failed proposal with decision: %v", err)
			}
			comments := outboxKeys(t, db, got.id)
			if len(comments) != 1 || len(comments["decision_open"]) != 1 || comments["decision_open"][0] != got.id+":decision_open:"+decisionID {
				t.Fatalf("comments = %v", comments)
			}
			if n := countRows(t, db, `SELECT count(*) FROM linear_outbox WHERE task_id = ? AND execution_id IS NULL AND action = 'comment'`, got.id); n != 1 {
				t.Fatalf("decision_open row with an Execution: %d", n)
			}
		})
	}
}

// 受け入れ基準: Project にもスペシャルハーネスにも振り分けられる。A direct
// special-harness route queues a task whose destination is the harness.
func TestStartLinearTaskQueuesDirectHarnessRoute(t *testing.T) {
	db := startDB(t)
	exec(t, db, `INSERT INTO routes VALUES ('route:direct',NULL,'fixture-harness','t','t')`)
	exec(t, db, `UPDATE harnesses SET default_profile_id='fixture-profile' WHERE id='fixture-harness'`)
	d, err := start(t, db, routedIssue("1", "route:direct"), 1)
	if err != nil || d != DecisionStarted {
		t.Fatalf("decision = %q, err = %v", d, err)
	}
	got := readStarted(t, db, "1", 1)
	var harness sql.NullString
	var execStatus string
	if err := db.QueryRow(`SELECT t.harness_id, e.status FROM tasks t JOIN executions e ON e.task_id = t.id WHERE t.id = ?`,
		got.id).Scan(&harness, &execStatus); err != nil {
		t.Fatal(err)
	}
	if got.status != "queued" || got.project.Valid || harness.String != "fixture-harness" || got.executions != 1 ||
		execStatus != "pending" || got.cause.Valid {
		t.Fatalf("task = %+v harness = %v execution = %s", got, harness, execStatus)
	}
	if n := countRows(t, db, `SELECT count(*) FROM route_proposals`); n != 0 {
		t.Fatalf("route proposals = %d", n)
	}
}

// 受け入れ基準: ai-ready は振り分けのラベルとして数えない（spec 15 3.6 step 2）. With a
// prefix the trigger label also matches, the trigger label is still not a
// routing label, so the single real routing label decides the destination.
func TestTriggerLabelIsNotARoutingLabel(t *testing.T) {
	db := startDB(t)
	exec(t, db, `DELETE FROM routes WHERE label = 'route:one'`)
	exec(t, db, `INSERT INTO routes VALUES ('ai-one','p1',NULL,'t','t')`)
	issue := routedIssue("1", "ai-one")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	starter := &TaskStarter{RoutingLabelPrefix: "ai-", TriggerLabel: "ai-ready", Now: func() time.Time { return base }}
	d, err := starter.StartLinearTask(context.Background(), tx, issue, 1)
	if err != nil || d != DecisionStarted {
		t.Fatalf("decision = %q, err = %v", d, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := readStarted(t, db, "1", 1); got.status != "queued" || got.project.String != "p1" {
		t.Fatalf("task = %+v", got)
	}
	// Only the trigger label: no routing label at all.
	if got := RoutingLabels([]string{"ai-ready", "bug"}, "ai-", "ai-ready"); len(got) != 0 {
		t.Fatalf("routing labels = %v", got)
	}
}

// 受け入れ基準: Team を振り分けの判定に使っていない。The same label routes Issues
// of any Team to the same destination, and the Team never makes routing fail.
func TestRoutingIgnoresTeam(t *testing.T) {
	for _, team := range []string{"team-a", "team-b", "team-unregistered", ""} {
		t.Run("team="+team, func(t *testing.T) {
			db := startDB(t)
			issue := routedIssue("1", "route:two")
			issue.TeamID = team
			d, err := start(t, db, issue, 1)
			if err != nil || d != DecisionStarted {
				t.Fatalf("decision = %q, err = %v", d, err)
			}
			if got := readStarted(t, db, "1", 1); got.status != "queued" || got.project.String != "p2" || got.executions != 1 {
				t.Fatalf("task = %+v", got)
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
		{"no heading", "Fix the login.\n\n## Notes\n- tests pass", []string{"route:one"}, "p1"},
		{"blank section", "Fix it.\n## 受け入れ条件\n   \n\n## Notes\nx", []string{"route:one"}, "p1"},
		{"empty body", "", []string{"route:one"}, "p1"},
		// Routing must be decided before the criteria requirement is checked.
		{"also unroutable", "no criteria", []string{"route:unknown"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := startDB(t)
			issue := routedIssue("1", test.labels...)
			issue.Description = test.body
			d, err := start(t, db, issue, 1)
			wantDecision := DecisionHeld
			if test.projID == "" {
				wantDecision = DecisionRouting
			}
			if err != nil || d != wantDecision {
				t.Fatalf("decision = %q, err = %v", d, err)
			}
			got := readStarted(t, db, "1", 1)
			wantStatus, wantCause := "blocked", "missing_acceptance_criteria"
			if test.projID == "" {
				wantStatus, wantCause = "routing", ""
			}
			if got.status != wantStatus || got.cause.String != wantCause ||
				got.criteria != "" || got.executions != 0 || got.project.String != test.projID || got.body != test.body {
				t.Fatalf("task = %+v", got)
			}
		})
	}
}

// The storage layer allows empty criteria; the Profile decides whether the
// intake/manual API may queue the task (spec 19 3.9).
func TestTaskStorageAllowsProfileDependentAcceptanceCriteria(t *testing.T) {
	db := startDB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task := model.Task{ProjectID: "p1", Title: "t", Body: "b", AcceptanceCriteria: "", ShortName: "task",
		TaskType: "development", CollaborationMode: "solo", Status: model.TaskReceived, IdempotencyKey: "k",
		CreatedAt: "t", UpdatedAt: "t"}
	if err := store.InsertTask(context.Background(), tx, &task); err != nil {
		t.Fatal(err)
	}
	// A Linear task can also persist an empty value without a hold.
	task.IdempotencyKey = "k2"
	if err := store.InsertLinearTask(context.Background(), tx, &task, ""); err != nil {
		t.Fatal(err)
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
	exec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES ('blocker', 'p1', 't', 'b', 'ac', 'solo', 'completed', 'linear:1:1', 't', 't')`)
	issue := routedIssue("1", "route:one")
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
