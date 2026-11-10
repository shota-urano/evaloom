package linearintake

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// recheckFixture runs the real Intake with TaskStarter as both Starter and
// HeldRechecker (spec 15 3.6.3) on startDB. team-a's first poll is done.
type recheckFixture struct {
	db     *sql.DB
	intake *Intake
	now    time.Time
}

func newRecheckFixture(t *testing.T) *recheckFixture {
	t.Helper()
	f := &recheckFixture{db: startDB(t), now: base}
	clock := func() time.Time { return f.now }
	starter := &TaskStarter{Now: clock}
	f.intake = &Intake{DB: f.db, Fetcher: &stubFetcher{}, Starter: starter, Canceller: &recordingCanceller{},
		Rechecker: starter, Now: clock}
	f.mustRound(t)
	return f
}

func (f *recheckFixture) round(t *testing.T, issues ...linear.Issue) error {
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

func (f *recheckFixture) mustRound(t *testing.T, issues ...linear.Issue) {
	t.Helper()
	if err := f.round(t, issues...); err != nil {
		t.Fatal(err)
	}
}

// hold attaches issue "1" with labels so it is held, and returns the task.
func (f *recheckFixture) hold(t *testing.T, issue linear.Issue, cause BlockCause) startedTask {
	t.Helper()
	f.mustRound(t, issue)
	held := readStarted(t, f.db, "1", 1)
	if held.status != "blocked" || held.cause.String != string(cause) || held.executions != 0 {
		t.Fatalf("held task = %+v", held)
	}
	if s := mustState(t, f.db, "1"); s.decision != string(DecisionHeld) {
		t.Fatalf("state = %+v", s)
	}
	return held
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func taskUpdatedAt(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var at string
	if err := db.QueryRow(`SELECT updated_at FROM tasks WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func recheckAudits(t *testing.T, db *sql.DB, taskID string) int {
	t.Helper()
	return countRows(t, db, `SELECT count(*) FROM audit_logs WHERE action = 'task.intake_recheck' AND target_id = ?`, taskID)
}

// 受け入れ基準: 保留中のタスクの Issue で実行先ラベルを直し、更新時刻が進んだ状態で照会すると、
// 同じタスクが queued になり Execution が1つできる。
func TestRecheckQueuesFixedRouting(t *testing.T) {
	for _, test := range []struct {
		name  string
		issue linear.Issue
		cause BlockCause
	}{
		{"unknown label", routedIssue("1", "route:unknown"), CauseRoutingUnknownLabel},
		{"two labels", routedIssue("1", "route:one", "route:three"), CauseRoutingMultipleLabels},
		{"no label", routedIssue("1", "bug"), CauseRoutingNoLabel},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			held := f.hold(t, test.issue, test.cause)

			fixed := routedIssue("1", "route:one")
			fixed.UpdatedAt = base.Add(time.Minute)
			f.now = base.Add(2 * time.Minute)
			f.mustRound(t, fixed)

			got := readStarted(t, f.db, "1", 1)
			if got.id != held.id || got.status != "queued" || got.project.String != "p1" || got.cause.Valid ||
				got.criteria != "- login works\n- tests pass" || got.executions != 1 {
				t.Fatalf("task = %+v", got)
			}
			if n := countTasks(t, f.db, "1"); n != 1 {
				t.Fatalf("tasks = %d; recheck must not create a task", n)
			}
			var seq int
			var mode, status, event string
			if err := f.db.QueryRow(`SELECT seq, mode, status, (SELECT payload FROM execution_events
				WHERE execution_id = e.id AND type = 'status_changed') FROM executions e WHERE task_id = ?`, got.id).
				Scan(&seq, &mode, &status, &event); err != nil {
				t.Fatal(err)
			}
			if seq != 1 || mode != "continue" || status != "pending" ||
				event != `{"from":"blocked","to":"queued","reason":"intake_recheck"}` {
				t.Fatalf("execution = %d %s %s %s", seq, mode, status, event)
			}
			s := mustState(t, f.db, "1")
			if s.decision != string(DecisionStarted) || s.seq != 1 || s.updatedAt != stamp(fixed.UpdatedAt) {
				t.Fatalf("state = %+v", s)
			}
			if recheckAudits(t, f.db, got.id) != 1 {
				t.Fatal("recheck audit log missing")
			}
			// The queued task is not rechecked again by a later edit.
			fixed.UpdatedAt = base.Add(time.Hour)
			fixed.Labels = []string{"ai-ready", "route:unknown"}
			f.mustRound(t, fixed)
			if again := readStarted(t, f.db, "1", 1); again.status != "queued" || again.executions != 1 || again.cause.Valid {
				t.Fatalf("queued task changed: %+v", again)
			}
		})
	}
}

// A task held for missing acceptance criteria starts once the heading is
// written; the criteria are taken from the new body (spec 15 3.6.3).
func TestRecheckQueuesWrittenCriteria(t *testing.T) {
	f := newRecheckFixture(t)
	issue := routedIssue("1", "route:one")
	issue.Description = "no criteria"
	held := f.hold(t, issue, CauseMissingAcceptanceCriteria)

	issue.Description = "## 受け入れ条件\n- 動く"
	issue.UpdatedAt = base.Add(time.Minute)
	f.mustRound(t, issue)
	got := readStarted(t, f.db, "1", 1)
	if got.id != held.id || got.status != "queued" || got.criteria != "- 動く" || got.cause.Valid ||
		got.project.String != "p1" || got.executions != 1 {
		t.Fatalf("task = %+v", got)
	}
}

// 受け入れ基準: 更新時刻が変わらない照会では再判定しない。
func TestRecheckNeedsNewerUpdatedAt(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "route:unknown"), CauseRoutingUnknownLabel)

	fixed := routedIssue("1", "route:one") // same updatedAt as recorded
	f.now = base.Add(time.Minute)
	f.mustRound(t, fixed)
	got := readStarted(t, f.db, "1", 1)
	if got.status != "blocked" || got.cause.String != string(CauseRoutingUnknownLabel) || got.executions != 0 ||
		recheckAudits(t, f.db, held.id) != 0 {
		t.Fatalf("rechecked without a newer updatedAt: %+v", got)
	}
	// An older updatedAt is not newer either.
	fixed.UpdatedAt = base.Add(-time.Minute)
	f.mustRound(t, fixed)
	if got := readStarted(t, f.db, "1", 1); got.status != "blocked" {
		t.Fatalf("rechecked with an older updatedAt: %+v", got)
	}
}

// 受け入れ基準: 保留の理由が変わらなければ何も起きない。
// A different cause is written to the task, which stays held.
func TestRecheckSameCauseDoesNothing(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "route:unknown"), CauseRoutingUnknownLabel)
	before := mustState(t, f.db, "1")
	heldAt := taskUpdatedAt(t, f.db, held.id)

	edited := routedIssue("1", "route:other", "bug")
	edited.Title = "edited"
	edited.UpdatedAt = base.Add(time.Minute)
	f.now = base.Add(2 * time.Minute)
	f.mustRound(t, edited)
	got := readStarted(t, f.db, "1", 1)
	after := mustState(t, f.db, "1")
	if got.status != "blocked" || got.cause.String != string(CauseRoutingUnknownLabel) || got.executions != 0 ||
		got.title != held.title || taskUpdatedAt(t, f.db, held.id) != heldAt || recheckAudits(t, f.db, held.id) != 0 {
		t.Fatalf("same cause changed the task: %+v", got)
	}
	if after.decision != before.decision || after.decidedAt != before.decidedAt {
		t.Fatalf("decision changed: %+v -> %+v", before, after)
	}

	// The cause changes: written, still held, no Execution.
	edited.Labels = []string{"ai-ready", "route:one", "route:three"}
	edited.UpdatedAt = base.Add(3 * time.Minute)
	f.mustRound(t, edited)
	got = readStarted(t, f.db, "1", 1)
	if got.status != "blocked" || got.cause.String != string(CauseRoutingMultipleLabels) || got.executions != 0 ||
		recheckAudits(t, f.db, held.id) != 1 {
		t.Fatalf("changed cause = %+v", got)
	}
	if s := mustState(t, f.db, "1"); s.decision != string(DecisionHeld) || s.decidedAt != stamp(f.now) {
		t.Fatalf("state = %+v", s)
	}
	if n := countTasks(t, f.db, "1"); n != 1 {
		t.Fatalf("tasks = %d", n)
	}
}

// 受け入れ基準: 保留していないタスクの Issue の更新では何も起きない。
func TestRecheckIgnoresTaskNotHeld(t *testing.T) {
	f := newRecheckFixture(t)
	issue := routedIssue("1", "route:one")
	f.mustRound(t, issue)
	started := readStarted(t, f.db, "1", 1)
	if started.status != "queued" {
		t.Fatalf("task = %+v", started)
	}
	startedAt := taskUpdatedAt(t, f.db, started.id)
	before := mustState(t, f.db, "1")

	// Even an edit that would be held at creation does nothing.
	issue.Labels = []string{"ai-ready", "route:unknown"}
	issue.Description = "no criteria"
	issue.UpdatedAt = base.Add(time.Minute)
	f.now = base.Add(2 * time.Minute)
	f.mustRound(t, issue)
	got := readStarted(t, f.db, "1", 1)
	if got.status != "queued" || got.cause.Valid || got.executions != 1 || got.criteria != started.criteria ||
		taskUpdatedAt(t, f.db, started.id) != startedAt || recheckAudits(t, f.db, started.id) != 0 {
		t.Fatalf("task changed: %+v", got)
	}
	if after := mustState(t, f.db, "1"); after.decision != before.decision || after.decidedAt != before.decidedAt {
		t.Fatalf("decision changed: %+v -> %+v", before, after)
	}
	// A held task that the owner already cancelled is not held any more.
	exec(t, f.db, `UPDATE tasks SET status = 'cancelled' WHERE id = ?`, got.id)
	issue.UpdatedAt = base.Add(time.Hour)
	f.mustRound(t, issue)
	if n := countTasks(t, f.db, "1"); n != 1 {
		t.Fatalf("tasks = %d", n)
	}
}

// 6章「タスク作成のトランザクションが失敗」: a failed recheck leaves the
// task and linear_issue_states unchanged, so the next poll rechecks again.
func TestRecheckFailureIsRetried(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "route:unknown"), CauseRoutingUnknownLabel)
	exec(t, f.db, `CREATE TRIGGER fail_execution BEFORE INSERT ON executions
		BEGIN SELECT RAISE(ABORT, 'injected execution failure'); END`)

	fixed := routedIssue("1", "route:one")
	fixed.UpdatedAt = base.Add(time.Minute)
	if err := f.round(t, fixed); err == nil {
		t.Fatal("failed recheck was not reported")
	}
	got := readStarted(t, f.db, "1", 1)
	if got.status != "blocked" || got.cause.String != string(CauseRoutingUnknownLabel) || got.project.Valid ||
		got.executions != 0 || recheckAudits(t, f.db, held.id) != 0 {
		t.Fatalf("task after failure = %+v", got)
	}
	if s := mustState(t, f.db, "1"); s.updatedAt != stamp(base) || s.decision != string(DecisionHeld) {
		t.Fatalf("state after failure = %+v", s)
	}

	exec(t, f.db, `DROP TRIGGER fail_execution`)
	f.mustRound(t, fixed)
	if got := readStarted(t, f.db, "1", 1); got.status != "queued" || got.executions != 1 {
		t.Fatalf("task after retry = %+v", got)
	}
}

// QueueHeldTaskTx accepts only a held task whose sole Execution is the new
// pending seq 1; UpdateHeldLinearTask touches only a held task.
func TestHeldTaskGuards(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "route:unknown"), CauseRoutingUnknownLabel)
	ctx := context.Background()
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := task.QueueHeldTaskTx(ctx, tx, held.id, "missing"); !errors.Is(err, task.ErrConflict) {
		t.Fatalf("missing execution: %v", err)
	}
	if err := store.UpdateHeldLinearTask(ctx, tx, held.id, "p1", "ac", "", "t"); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{1, 2} {
		if _, err := tx.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
			VALUES (?, ?, ?, 'continue', 'pending', 't')`, "e"+string(rune('0'+seq)), held.id, seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := task.QueueHeldTaskTx(ctx, tx, held.id, "e1"); !errors.Is(err, task.ErrConflict) {
		t.Fatalf("two executions: %v", err)
	}
	// The hold is released, so the task is no longer held.
	if err := store.UpdateHeldLinearTask(ctx, tx, held.id, "p1", "ac", "", "t"); err == nil {
		t.Fatal("updated a task that is not held")
	}
}

// spec 15 3.6.3 (Q81): a marked Issue that left the list is decided by 3.4
// and 3.7, and the recheck mark is cleared with that decision.
func TestRecheckMarkClearedWhenIssueLeavesList(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "route:unknown"), CauseRoutingUnknownLabel)
	if _, err := f.db.Exec(`UPDATE linear_issue_states SET recheck_requested_at = ? WHERE linear_issue_id = '1'`,
		stamp(base)); err != nil {
		t.Fatal(err)
	}
	f.mustRound(t) // the Issue is gone: deleted (stubFetcher answers not found)
	var mark, removed sql.NullString
	var decision string
	if err := f.db.QueryRow(`SELECT recheck_requested_at, removed_at, last_decision
		FROM linear_issue_states WHERE linear_issue_id = '1'`).Scan(&mark, &removed, &decision); err != nil {
		t.Fatal(err)
	}
	if mark.Valid || !removed.Valid || decision != string(DecisionCancelled) {
		t.Fatalf("mark = %+v, removed = %+v, decision = %s; want NULL, removed, cancelled", mark, removed, decision)
	}
	if n := recheckAudits(t, f.db, held.id); n != 0 {
		t.Fatalf("recheck audits = %d; a removed Issue is not rechecked", n)
	}
}
