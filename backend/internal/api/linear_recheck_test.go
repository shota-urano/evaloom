package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
)

// Spec 15 3.6.3 (Q81): an Evaloom setting change through the admin API marks
// the Issues of the held tasks it may release, and the next successful poll
// redoes the 3.6 decision without the Issue being updated. These tests drive
// the real API handlers and the real Poller, Intake and TaskStarter.

const recheckBody = "Do it.\n\n## Acceptance Criteria\n- login works\n"

var recheckIssueTime = time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)

// recheckClient answers ListReadyIssues from issues, or fails with err.
type recheckClient struct {
	issues map[string][]linear.Issue
	err    error
}

func (c *recheckClient) CheckKey() error { return nil }

func (c *recheckClient) ListReadyIssues(_ context.Context, teamID, _ string) ([]linear.Issue, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.issues[teamID], nil
}

type recheckFetcher struct{}

func (recheckFetcher) Issue(context.Context, string) (linear.Issue, error) {
	return linear.Issue{}, linear.ErrIssueNotFound
}

type recheckFixture struct {
	*authFixture
	client *recheckClient
	poller *linearintake.Poller
}

// newRecheckFixture registers project-a (team-a, route:a) and project-b
// (no Linear route), both with default role developer, harness-a with
// Profiles 1.2.0 and 2.0.0, and finishes team-a's first poll.
func newRecheckFixture(t *testing.T) *recheckFixture {
	t.Helper()
	f, _, _ := assignmentFixture(t)
	if _, err := f.db.Exec(`INSERT INTO routes VALUES ('route:a','project-a',NULL,'t','t'); INSERT INTO settings VALUES ('linear.teamId','"team-a"','t')`); err != nil {
		t.Fatal(err)
	}

	starter := &linearintake.TaskStarter{}
	intake := &linearintake.Intake{DB: f.db, Fetcher: recheckFetcher{}, Starter: starter,
		Canceller: linearintake.TaskCanceller{}, Rechecker: starter}
	rf := &recheckFixture{authFixture: f, client: &recheckClient{issues: map[string][]linear.Issue{}}}
	rf.poller = &linearintake.Poller{DB: f.db, Client: rf.client, Handler: intake}
	rf.poll(t) // first poll: record only (spec 15 3.4, Q64)
	return rf
}

func (f *recheckFixture) poll(t *testing.T, issues ...linear.Issue) {
	t.Helper()
	f.client.issues = map[string][]linear.Issue{}
	for _, issue := range issues {
		f.client.issues[issue.TeamID] = append(f.client.issues[issue.TeamID], issue)
	}
	if _, err := f.poller.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *recheckFixture) assign(t *testing.T, project, role, version string) {
	t.Helper()
	status, body := postAssignment(t, f.authFixture, project, role, "harness-a", version)
	if status != 200 && status != 201 {
		t.Fatalf("assignment status=%d body=%s", status, body)
	}
}

func (f *recheckFixture) patch(t *testing.T, project string, values map[string]any) {
	t.Helper()
	status, body := projectRequest(t, f.authFixture, "PATCH", "/api/projects/"+project, values)
	requireProject(t, status, body, 200)
}

func recheckIssue(id, team string, labels ...string) linear.Issue {
	return linear.Issue{ID: id, Identifier: "ENG-" + id, URL: "https://linear.app/x/ENG-" + id,
		Title: "title " + id, Description: recheckBody, TeamID: team,
		Labels: append([]string{"ai-ready"}, labels...), StateType: "unstarted", UpdatedAt: recheckIssueTime}
}

type recheckTask struct {
	id, status, decision, issueUpdatedAt string
	project, cause, mark                 sql.NullString
	executions, tasks, comments          int
}

func (f *recheckFixture) task(t *testing.T, issueID string) recheckTask {
	t.Helper()
	var r recheckTask
	err := f.db.QueryRow(`SELECT t.id, t.status, t.project_id, t.intake_block_cause,
		(SELECT count(*) FROM executions e WHERE e.task_id = t.id),
		(SELECT count(*) FROM linear_issue_links WHERE linear_issue_id = s.linear_issue_id),
		(SELECT count(*) FROM linear_outbox o WHERE o.task_id = t.id),
		s.last_decision, s.issue_updated_at, s.recheck_requested_at
		FROM linear_issue_states s JOIN linear_issue_links l ON l.linear_issue_id = s.linear_issue_id
		JOIN tasks t ON t.id = l.task_id WHERE s.linear_issue_id = ?`, issueID).
		Scan(&r.id, &r.status, &r.project, &r.cause, &r.executions, &r.tasks, &r.comments,
			&r.decision, &r.issueUpdatedAt, &r.mark)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *recheckFixture) hold(t *testing.T, issue linear.Issue, cause linearintake.BlockCause) recheckTask {
	t.Helper()
	f.poll(t, issue)
	held := f.task(t, issue.ID)
	if held.status != "blocked" || held.cause.String != string(cause) || held.executions != 0 || held.mark.Valid {
		t.Fatalf("held task = %+v; want blocked by %s", held, cause)
	}
	return held
}

func requireMarked(t *testing.T, got recheckTask, want bool) {
	t.Helper()
	if got.mark.Valid != want {
		t.Fatalf("recheck_requested_at = %+v; want marked=%v", got.mark, want)
	}
	if want {
		if _, err := time.Parse(time.RFC3339Nano, got.mark.String); err != nil {
			t.Fatalf("recheck_requested_at = %q: %v", got.mark.String, err)
		}
	}
}

// requireStarted checks the same held task was queued by the recheck with
// one Execution, the Issue's updatedAt unchanged, and the mark cleared.
func requireStarted(t *testing.T, got, held recheckTask, project string) {
	t.Helper()
	if got.id != held.id || got.tasks != 1 || got.status != "queued" || got.executions != 1 ||
		got.project.String != project || got.cause.Valid || got.decision != "started" {
		t.Fatalf("task after recheck = %+v; want held task %s queued in %s", got, held.id, project)
	}
	if got.issueUpdatedAt != held.issueUpdatedAt {
		t.Fatalf("issue_updated_at = %s; want unchanged %s", got.issueUpdatedAt, held.issueUpdatedAt)
	}
	requireMarked(t, got, false)
}

// 受け入れ基準: routing_unknown_label で保留したタスクの実行先ラベルを PATCH /api/projects/{id} で
// Project に登録すると、Issue の更新時刻が変わらないまま次の照会で同じタスクが queued になり
// Execution が1つできる。再判定の後に印が NULL に戻る。

func TestLinearRecheckIssueOnlyCausesAreNotMarked(t *testing.T) {
	f := newRecheckFixture(t)
	noCriteria := recheckIssue("3", "team-a", "route:a")
	noCriteria.Description = "no criteria here"
	issues := []linear.Issue{
		recheckIssue("1", "team-a", "route:a", "route:b"),
		recheckIssue("2", "team-a", "bug"),
		noCriteria,
		recheckIssue("4", "team-a", "route:b"),
	}
	f.poll(t, issues...)
	for id, cause := range map[string]linearintake.BlockCause{
		"1": linearintake.CauseRoutingMultipleLabels, "2": linearintake.CauseRoutingNoLabel,
		"3": linearintake.CauseMissingAcceptanceCriteria, "4": linearintake.CauseRoutingUnknownLabel,
	} {
		if got := f.task(t, id); got.cause.String != string(cause) {
			t.Fatalf("issue %s held by %+v; want %s", id, got.cause, cause)
		}
	}

	f.patch(t, "project-a", map[string]any{"name": "Renamed"})
	f.assign(t, "project-a", "reviewer", "1.2.0")
	for _, id := range []string{"1", "2", "3"} {
		requireMarked(t, f.task(t, id), false)
	}
	requireMarked(t, f.task(t, "4"), false) // the same change marks a routing hold
}

// 受け入れ基準: 値が変わらない PATCH では印が付かない（同じ Profile の割り当ての上書きも同じ）。

func TestLinearRecheckSameCauseChangesNothing(t *testing.T) {
	f := newRecheckFixture(t)
	issue := recheckIssue("1", "team-a", "route:b")
	held := f.hold(t, issue, linearintake.CauseRoutingUnknownLabel)
	var updatedAt string
	if err := f.db.QueryRow(`SELECT updated_at FROM tasks WHERE id = ?`, held.id).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	audits := projectCount(t, f.authFixture, "audit_logs")

	f.patch(t, "project-b", map[string]any{"name": "Renamed"})
	if _, err := f.db.Exec(`UPDATE linear_issue_states SET recheck_requested_at='2026-11-10T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	requireMarked(t, f.task(t, "1"), true)
	audits++ // the PATCH itself
	f.poll(t, issue)

	got := f.task(t, "1")
	requireMarked(t, got, false)
	if got.id != held.id || got.status != "blocked" || got.cause != held.cause || got.executions != 0 ||
		got.comments != held.comments || got.decision != held.decision || got.tasks != 1 {
		t.Fatalf("task after same-cause recheck = %+v; want unchanged %+v", got, held)
	}
	var after string
	if err := f.db.QueryRow(`SELECT updated_at FROM tasks WHERE id = ?`, held.id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != updatedAt || projectCount(t, f.authFixture, "audit_logs") != audits {
		t.Fatalf("same-cause recheck wrote: updated_at %s -> %s, audit_logs %d", updatedAt, after,
			projectCount(t, f.authFixture, "audit_logs"))
	}
}

// 受け入れ基準: 照会が失敗した回は印が残り、次に成功した回で再判定する。
