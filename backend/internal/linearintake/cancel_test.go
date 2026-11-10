package linearintake

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// cancelFixture runs the real Intake with TaskStarter and TaskCanceller on
// startDB (spec 15 3.7). team-a's first poll is done.
type cancelFixture struct {
	db      *sql.DB
	fetcher *stubFetcher
	intake  *Intake
}

func newCancelFixture(t *testing.T) *cancelFixture {
	t.Helper()
	f := &cancelFixture{db: startDB(t), fetcher: &stubFetcher{issues: map[string]linear.Issue{}, errs: map[string]error{}}}
	clock := func() time.Time { return base }
	starter := &TaskStarter{Now: clock}
	f.intake = &Intake{DB: f.db, Fetcher: f.fetcher, Starter: starter, Canceller: TaskCanceller{},
		Rechecker: starter, Now: clock}
	f.mustRound(t)
	return f
}

func (f *cancelFixture) round(t *testing.T, issues ...linear.Issue) error {
	t.Helper()
	teams, err := store.SyncLinearPollTeams(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, team := range teams {
		if team.TeamID == "team-a" {
			return f.intake.HandleTeamIssues(context.Background(), team, issues)
		}
	}
	t.Fatal("team-a not polled")
	return nil
}

func (f *cancelFixture) mustRound(t *testing.T, issues ...linear.Issue) {
	t.Helper()
	if err := f.round(t, issues...); err != nil {
		t.Fatal(err)
	}
}

// taskStates are the task states reached by a Linear task, with the latest
// Execution's status ("" for a held task, which has none).
var taskStates = map[string]string{
	"queued": "pending", "running": "running", "verifying": "verifying", "blocked": "blocked",
	"held": "", "completed": "succeeded", "failed": "failed", "cancelled": "cancelled", "timed_out": "timed_out",
}

// attach starts issue "1" and moves its task to state. The task is created
// by TaskStarter; the later states stand in for the Runner's progress.
func (f *cancelFixture) attach(t *testing.T, state string) (linear.Issue, string) {
	t.Helper()
	issue := routedIssue("1", "route:one")
	if state == "held" {
		issue = routedIssue("1", "route:unknown")
	}
	f.mustRound(t, issue)
	task := readStarted(t, f.db, "1", 1)
	if execution := taskStates[state]; state != "held" && state != "queued" {
		exec(t, f.db, `UPDATE executions SET status = ? WHERE task_id = ?`, execution, task.id)
		exec(t, f.db, `UPDATE tasks SET status = ? WHERE id = ?`, state, task.id)
	}
	return issue, task.id
}

type cancelledTask struct {
	task, execution, errorClass string
	requested, finished         bool
	cancelAudits                int
}

func readCancelled(t *testing.T, db *sql.DB, taskID string) cancelledTask {
	t.Helper()
	var c cancelledTask
	var execution, errorClass sql.NullString
	var requested, finished sql.NullBool
	if err := db.QueryRow(`SELECT t.status, e.status, e.error_class, e.cancel_requested_at IS NOT NULL,
		e.finished_at IS NOT NULL, (SELECT count(*) FROM audit_logs WHERE action = 'task.cancel' AND target_id = t.id)
		FROM tasks t LEFT JOIN executions e ON e.task_id = t.id WHERE t.id = ?`, taskID).
		Scan(&c.task, &execution, &errorClass, &requested, &finished, &c.cancelAudits); err != nil {
		t.Fatal(err)
	}
	c.execution, c.errorClass, c.requested, c.finished = execution.String, errorClass.String, requested.Bool, finished.Bool
	return c
}

func assertSystemAudit(t *testing.T, db *sql.DB, taskID string, reason CancelReason, requested bool) {
	t.Helper()
	var actor, gotReason string
	var gotRequested bool
	if err := db.QueryRow(`SELECT actor, json_extract(detail, '$.reason'), json_extract(detail, '$.cancelRequested')
		FROM audit_logs WHERE action = 'task.cancel' AND target_id = ?`, taskID).
		Scan(&actor, &gotReason, &gotRequested); err != nil {
		t.Fatal(err)
	}
	if actor != "system" || gotReason != string(reason) || gotRequested != requested {
		t.Fatalf("audit = %s %s requested=%v; want system %s requested=%v", actor, gotReason, gotRequested, reason, requested)
	}
}

// detachTriggers are the ways an attached Issue is stopped (spec 15 3.4).
var detachTriggers = []struct {
	name   string
	reason CancelReason
	run    func(t *testing.T, f *cancelFixture, issue linear.Issue)
}{
	{"label removed", ReasonLabelRemoved, func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.Labels = []string{"route:one"}
		f.fetcher.issues[issue.ID] = issue
		f.mustRound(t)
	}},
	{"cancelled in list", ReasonIssueCancelled, func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.StateType = "canceled"
		f.mustRound(t, issue)
	}},
	{"cancelled after leaving list", ReasonIssueCancelled, func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.StateType = "canceled"
		f.fetcher.issues[issue.ID] = issue
		f.mustRound(t)
	}},
	{"deleted", ReasonIssueRemoved, func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		f.mustRound(t) // the refetch finds nothing
	}},
	{"archived", ReasonIssueRemoved, func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.Archived = true
		f.fetcher.issues[issue.ID] = issue
		f.mustRound(t)
	}},
	{"moved to an unregistered Team", ReasonIssueMoved, func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.TeamID = "team-unregistered"
		f.fetcher.issues[issue.ID] = issue
		f.mustRound(t)
	}},
}

// 受け入れ基準: ai-ready を外した Issue の照会で queued・running・blocked のタスクが中止の経路に入る;
// Issue のキャンセル、削除・アーカイブでも同じになる; audit_logs の actor が system で、理由が 3.4 の表の値になる。
func TestLinearCancellationTakesOwnerPath(t *testing.T) {
	for _, trigger := range detachTriggers {
		for _, state := range []string{"queued", "running", "verifying", "blocked", "held"} {
			t.Run(trigger.name+"/"+state, func(t *testing.T) {
				f := newCancelFixture(t)
				issue, taskID := f.attach(t, state)
				trigger.run(t, f, issue)
				got := readCancelled(t, f.db, taskID)
				// The same results as the owner's cancellation (spec 04 3.5).
				switch state {
				case "running", "verifying":
					// The Runner stops it on its next heartbeat.
					if got.task != state || got.execution != state || !got.requested || got.finished {
						t.Fatalf("task = %+v; want a cancel request", got)
					}
				case "held":
					if got.task != "cancelled" || got.execution != "" {
						t.Fatalf("task = %+v; want cancelled without an Execution", got)
					}
				default:
					if got.task != "cancelled" || got.execution != "cancelled" || got.errorClass != "cancelled" ||
						got.requested || !got.finished {
						t.Fatalf("task = %+v; want cancelled", got)
					}
				}
				if got.cancelAudits != 1 {
					t.Fatalf("cancel audits = %d", got.cancelAudits)
				}
				assertSystemAudit(t, f.db, taskID, trigger.reason, state == "running" || state == "verifying")
				if n := countRows(t, f.db, `SELECT count(*) FROM notification_requests`); n != 0 {
					t.Fatalf("cancellation requested %d notifications", n)
				}
				// A later round does not cancel again.
				f.mustRound(t)
				if again := readCancelled(t, f.db, taskID); again != got {
					t.Fatalf("second round changed the task: %+v -> %+v", got, again)
				}
			})
		}
	}
}

// 受け入れ基準: completed のタスクは変わらない。終端状態のタスクには何もしない（3.7）。
func TestLinearCancellationLeavesTerminalTasks(t *testing.T) {
	for _, trigger := range detachTriggers {
		for _, state := range []string{"completed", "failed", "cancelled", "timed_out"} {
			t.Run(trigger.name+"/"+state, func(t *testing.T) {
				f := newCancelFixture(t)
				issue, taskID := f.attach(t, state)
				before := readCancelled(t, f.db, taskID)
				updated := taskUpdatedAt(t, f.db, taskID)
				trigger.run(t, f, issue)
				if after := readCancelled(t, f.db, taskID); after != before || after.cancelAudits != 0 {
					t.Fatalf("terminal task changed: %+v -> %+v", before, after)
				}
				if taskUpdatedAt(t, f.db, taskID) != updated {
					t.Fatal("terminal task updated_at changed")
				}
			})
		}
	}
}

// Only the non-terminal task of a reattached Issue is stopped; the earlier
// completed one stays (spec 15 3.5 rule 3, 3.7).
func TestLinearCancellationStopsOnlyActiveTaskOfIssue(t *testing.T) {
	f := newCancelFixture(t)
	issue, first := f.attach(t, "completed")
	detached := issue
	detached.Labels = []string{"route:one"}
	f.fetcher.issues[issue.ID] = detached
	f.mustRound(t)
	f.mustRound(t, issue) // attached again: a second task
	second := readStarted(t, f.db, "1", 2)
	if second.status != "queued" {
		t.Fatalf("second task = %+v", second)
	}
	f.mustRound(t) // removed again
	if got := readCancelled(t, f.db, first); got.task != "completed" || got.cancelAudits != 0 {
		t.Fatalf("completed task changed: %+v", got)
	}
	if got := readCancelled(t, f.db, second.id); got.task != "cancelled" {
		t.Fatalf("active task = %+v", got)
	}
	assertSystemAudit(t, f.db, second.id, ReasonLabelRemoved, false)
}

// 6章: a failed cancellation rolls back with the Issue's record, so the next
// round decides it again instead of losing the detach.
func TestLinearCancellationFailureIsRetried(t *testing.T) {
	f := newCancelFixture(t)
	issue, taskID := f.attach(t, "queued")
	// Task and Execution disagree: the owner path refuses with a conflict.
	exec(t, f.db, `UPDATE tasks SET status = 'running' WHERE id = ?`, taskID)
	before := mustState(t, f.db, "1")
	detached := issue
	detached.Labels = []string{"route:one"}
	f.fetcher.issues[issue.ID] = detached
	if err := f.round(t); err == nil {
		t.Fatal("round succeeded with a conflicting task")
	}
	if after := mustState(t, f.db, "1"); after != before {
		t.Fatalf("failed cancellation changed the record: %+v -> %+v", before, after)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM audit_logs WHERE action = 'task.cancel'`); n != 0 {
		t.Fatalf("failed cancellation left %d audits", n)
	}
	exec(t, f.db, `UPDATE tasks SET status = 'queued' WHERE id = ?`, taskID)
	f.mustRound(t)
	if got := readCancelled(t, f.db, taskID); got.task != "cancelled" {
		t.Fatalf("retried cancellation: %+v", got)
	}
	if s := mustState(t, f.db, "1"); s.aiReady || s.decision != string(DecisionCancelled) {
		t.Fatalf("state = %+v", s)
	}
	assertSystemAudit(t, f.db, taskID, ReasonLabelRemoved, false)
}
