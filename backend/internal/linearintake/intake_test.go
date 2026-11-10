package linearintake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// stubFetcher answers the per-Issue refetch; no test reaches Linear.
type stubFetcher struct {
	issues map[string]linear.Issue
	errs   map[string]error
	calls  []string
}

func (f *stubFetcher) Issue(_ context.Context, id string) (linear.Issue, error) {
	f.calls = append(f.calls, id)
	if err := f.errs[id]; err != nil {
		return linear.Issue{}, err
	}
	issue, ok := f.issues[id]
	if !ok {
		return linear.Issue{}, linear.ErrIssueNotFound
	}
	return issue, nil
}

type startCall struct {
	issueID string
	seq     int
}

// dbStarter stands in for task creation (spec 15 3.6, a later task): it
// writes a task and its linear_issue_links row in the given transaction.
// fail makes it fail after writing, as if task creation broke midway.
type dbStarter struct {
	calls []startCall
	fail  func(issueID string, seq int) bool
}

func (s *dbStarter) StartLinearTask(ctx context.Context, tx *sql.Tx, issue linear.Issue, seq int) (Decision, error) {
	s.calls = append(s.calls, startCall{issue.ID, seq})
	taskID := fmt.Sprintf("task-%s-%d", issue.ID, seq)
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria,
		short_name, task_type, collaboration_mode, status, idempotency_key, created_at, updated_at, source)
		VALUES (?, 'p1', ?, ?, 'ac', ?, 'development', 'solo', 'queued', ?, 't', 't', 'linear')`,
		taskID, issue.Title, issue.Description, strings.ToLower(issue.Identifier),
		fmt.Sprintf("linear:%s:%d", issue.ID, seq)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier,
		issue_url, team_id, task_id, attach_seq, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 't')`,
		"link-"+taskID, issue.ID, issue.Identifier, issue.URL, issue.TeamID, taskID, seq); err != nil {
		return "", err
	}
	if s.fail != nil && s.fail(issue.ID, seq) {
		return "", errors.New("task creation failed midway")
	}
	return DecisionStarted, nil
}

type cancelCall struct {
	issueID string
	reason  CancelReason
}

type recordingCanceller struct{ calls []cancelCall }

func (c *recordingCanceller) CancelLinearTasks(_ context.Context, _ *sql.Tx, issueID string, reason CancelReason) error {
	c.calls = append(c.calls, cancelCall{issueID, reason})
	return nil
}

type recordingRechecker struct{ calls []string }

func (r *recordingRechecker) RecheckHeldTask(_ context.Context, _ *sql.Tx, issue linear.Issue) (Decision, error) {
	r.calls = append(r.calls, issue.ID)
	return "", nil
}

type intakeFixture struct {
	db        *sql.DB
	fetcher   *stubFetcher
	starter   *dbStarter
	canceller *recordingCanceller
	recheck   *recordingRechecker
	intake    *Intake
}

var base = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

func newIntakeFixture(t *testing.T) *intakeFixture {
	t.Helper()
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
	addProject(t, db, "p2", "team-b", "route:two")
	f := &intakeFixture{db: db, fetcher: &stubFetcher{issues: map[string]linear.Issue{}, errs: map[string]error{}},
		starter: &dbStarter{}, canceller: &recordingCanceller{}, recheck: &recordingRechecker{}}
	f.intake = &Intake{DB: db, Fetcher: f.fetcher, Starter: f.starter, Canceller: f.canceller,
		Rechecker: f.recheck, Now: func() time.Time { return base }}
	return f
}

// round runs one Team's decision with the Team row as the Poller sees it.
func (f *intakeFixture) round(t *testing.T, teamID string, issues ...linear.Issue) error {
	t.Helper()
	teams, err := store.SyncLinearPollTeams(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, team := range teams {
		if team.TeamID == teamID {
			return f.intake.HandleTeamIssues(context.Background(), team, issues)
		}
	}
	t.Fatalf("team %s not polled", teamID)
	return nil
}

func (f *intakeFixture) mustRound(t *testing.T, teamID string, issues ...linear.Issue) {
	t.Helper()
	if err := f.round(t, teamID, issues...); err != nil {
		t.Fatal(err)
	}
}

// baseline finishes team-a's first poll with an empty list.
func (f *intakeFixture) baseline(t *testing.T) { f.mustRound(t, "team-a") }

func readyIssue(id string, updated time.Time) linear.Issue {
	return linear.Issue{ID: id, Identifier: "ENG-" + id, URL: "https://linear.app/x/" + id, Title: "title " + id,
		Description: "body", TeamID: "team-a", Labels: []string{"ai-ready", "route:one"}, StateType: "unstarted",
		UpdatedAt: updated}
}

type stateRow struct {
	aiReady   bool
	seq       int
	teamID    string
	decision  string
	decidedAt string
	updatedAt string
	removedAt string
}

func readState(t *testing.T, db *sql.DB, id string) (stateRow, bool) {
	t.Helper()
	var s stateRow
	var decision, decidedAt, removedAt sql.NullString
	err := db.QueryRow(`SELECT ai_ready, attach_seq, team_id, last_decision, last_decision_at, issue_updated_at, removed_at
		FROM linear_issue_states WHERE linear_issue_id = ?`, id).Scan(&s.aiReady, &s.seq, &s.teamID, &decision, &decidedAt, &s.updatedAt, &removedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false
	}
	if err != nil {
		t.Fatal(err)
	}
	s.decision, s.decidedAt, s.removedAt = decision.String, decidedAt.String, removedAt.String
	return s, true
}

func mustState(t *testing.T, db *sql.DB, id string) stateRow {
	t.Helper()
	s, ok := readState(t, db, id)
	if !ok {
		t.Fatalf("no linear_issue_states row for %s", id)
	}
	return s
}

func countTasks(t *testing.T, db *sql.DB, issueID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks t JOIN linear_issue_links l ON l.task_id = t.id
		WHERE l.linear_issue_id = ?`, issueID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var all int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE idempotency_key LIKE ?`, "linear:"+issueID+":%").Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != n {
		t.Fatalf("issue %s: %d tasks but %d links", issueID, all, n)
	}
	return n
}

func baselineDone(t *testing.T, db *sql.DB, teamID string) bool {
	t.Helper()
	var done bool
	if err := db.QueryRow(`SELECT baseline_done_at IS NOT NULL FROM linear_poll_teams WHERE team_id = ?`, teamID).Scan(&done); err != nil {
		t.Fatal(err)
	}
	return done
}

// 受け入れ基準: Team の最初の照会では、一覧の Issue を記録するだけでタスクが作られない。
func TestFirstPollOnlyRecords(t *testing.T) {
	f := newIntakeFixture(t)
	cancelled := readyIssue("c", base)
	cancelled.StateType = "canceled"
	f.mustRound(t, "team-a", readyIssue("a", base), readyIssue("b", base), cancelled)
	if len(f.starter.calls) != 0 || len(f.canceller.calls) != 0 {
		t.Fatalf("first poll acted: starts=%v cancels=%v", f.starter.calls, f.canceller.calls)
	}
	for _, id := range []string{"a", "b", "c"} {
		s := mustState(t, f.db, id)
		if !s.aiReady || s.seq != 0 || s.decision != string(DecisionSkippedInitial) {
			t.Fatalf("%s: %+v, want ai_ready=1 seq=0 skipped_initial", id, s)
		}
		if countTasks(t, f.db, id) != 0 {
			t.Fatalf("%s: task created on first poll", id)
		}
	}
	if !baselineDone(t, f.db, "team-a") {
		t.Fatal("baseline_done_at not set after the first poll")
	}

	// The next poll treats them as still attached, not as new attaches.
	f.mustRound(t, "team-a", readyIssue("a", base), readyIssue("b", base))
	if len(f.starter.calls) != 0 {
		t.Fatalf("issues known since the first poll were started: %v", f.starter.calls)
	}
}

// 受け入れ基準: 前回に無く今回ある Issue で起動になり attach_seq が1増える。
func TestAttachStartsAndIncrementsSeq(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	f.mustRound(t, "team-a", readyIssue("a", base))
	if !slices.Equal(f.starter.calls, []startCall{{"a", 1}}) {
		t.Fatalf("starts = %v, want a seq 1", f.starter.calls)
	}
	s := mustState(t, f.db, "a")
	if !s.aiReady || s.seq != 1 || s.decision != string(DecisionStarted) {
		t.Fatalf("state = %+v", s)
	}
	if countTasks(t, f.db, "a") != 1 {
		t.Fatal("task not created")
	}
	// A row with ai_ready=0 attaches the same way.
	f.mustRound(t, "team-a", readyIssue("a", base))
	exec(t, f.db, `INSERT INTO linear_issue_states (linear_issue_id, issue_identifier, team_id, ai_ready,
		attach_seq, issue_updated_at, first_seen_at, last_seen_at) VALUES ('z', 'ENG-z', 'team-a', 0, 4, 't', 't', 't')`)
	f.mustRound(t, "team-a", readyIssue("a", base), readyIssue("z", base))
	if got := f.starter.calls[len(f.starter.calls)-1]; got != (startCall{"z", 5}) {
		t.Fatalf("last start = %v, want z seq 5", got)
	}
}

// 受け入れ基準: 付いたまま本文やラベルを変えた Issue で何も起きない。
func TestStillAttachedEditsDoNothing(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	f.mustRound(t, "team-a", readyIssue("a", base))
	before := mustState(t, f.db, "a")

	edited := readyIssue("a", base.Add(time.Hour))
	edited.Title, edited.Description = "changed", "changed body"
	edited.Labels = []string{"ai-ready", "route:two", "bug"}
	edited.StateType = "started"
	f.intake.Now = func() time.Time { return base.Add(2 * time.Hour) }
	f.mustRound(t, "team-a", edited)

	if len(f.starter.calls) != 1 || len(f.canceller.calls) != 0 {
		t.Fatalf("edit acted: starts=%v cancels=%v", f.starter.calls, f.canceller.calls)
	}
	after := mustState(t, f.db, "a")
	if after.seq != before.seq || after.decision != before.decision || after.decidedAt != before.decidedAt {
		t.Fatalf("decision changed: %+v -> %+v", before, after)
	}
	if countTasks(t, f.db, "a") != 1 {
		t.Fatal("second task created")
	}
	// Only the held-task recheck (3.6.3) sees a newer updatedAt, and only once.
	if !slices.Equal(f.recheck.calls, []string{"a"}) {
		t.Fatalf("recheck calls = %v, want [a]", f.recheck.calls)
	}
	f.mustRound(t, "team-a", edited)
	if len(f.recheck.calls) != 1 {
		t.Fatalf("recheck without a newer updatedAt: %v", f.recheck.calls)
	}
}

// 受け入れ基準: 一覧から消えた Issue を取り直し、ラベルが外れていれば中止、キャンセルなら中止になる。
// 3.4 の2つ目の表の残りの行（削除・アーカイブ、Team の移動、取り直しの失敗）も確かめる。
func TestMissingIssuesAreRefetched(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	ids := []string{"label", "cancel", "deleted", "archived", "moved", "moved-reg", "fail"}
	var all []linear.Issue
	for _, id := range ids {
		all = append(all, readyIssue(id, base))
	}
	f.mustRound(t, "team-a", all...)

	unlabelled := readyIssue("label", base.Add(time.Hour))
	unlabelled.Labels = []string{"route:one"}
	f.fetcher.issues["label"] = unlabelled
	cancelled := readyIssue("cancel", base.Add(time.Hour))
	cancelled.StateType = "canceled"
	f.fetcher.issues["cancel"] = cancelled
	archived := readyIssue("archived", base)
	archived.Archived = true
	f.fetcher.issues["archived"] = archived
	moved := readyIssue("moved", base)
	moved.TeamID = "team-unregistered"
	f.fetcher.issues["moved"] = moved
	movedReg := readyIssue("moved-reg", base)
	movedReg.TeamID = "team-b"
	f.fetcher.issues["moved-reg"] = movedReg
	f.fetcher.errs["fail"] = &linear.Error{Op: "get issue", Class: linear.ClassNetwork}
	beforeFail := mustState(t, f.db, "fail")

	f.mustRound(t, "team-a")

	want := []cancelCall{
		{"archived", ReasonIssueRemoved},
		{"cancel", ReasonIssueCancelled},
		{"deleted", ReasonIssueRemoved},
		{"label", ReasonLabelRemoved},
		{"moved", ReasonIssueMoved},
		{"moved-reg", ReasonIssueMoved},
	}
	if !slices.Equal(f.canceller.calls, want) {
		t.Fatalf("cancels = %v\nwant %v", f.canceller.calls, want)
	}
	if s := mustState(t, f.db, "label"); s.aiReady || s.decision != string(DecisionCancelled) {
		t.Fatalf("label removed: %+v, want ai_ready=0 cancelled", s)
	}
	if s := mustState(t, f.db, "cancel"); s.decision != string(DecisionCancelled) {
		t.Fatalf("cancelled: %+v", s)
	}
	for _, id := range []string{"deleted", "archived"} {
		if s := mustState(t, f.db, id); s.aiReady || s.removedAt != base.Format(time.RFC3339Nano) || s.seq != 1 {
			t.Fatalf("%s: %+v, want ai_ready=0, removed_at set, attach_seq kept", id, s)
		}
	}
	if s := mustState(t, f.db, "moved"); s.aiReady || s.teamID != "team-unregistered" {
		t.Fatalf("moved: %+v, want ai_ready=0 on the new Team", s)
	}
	if s := mustState(t, f.db, "moved-reg"); s.aiReady || s.decision != string(DecisionCancelled) || s.teamID != "team-b" {
		t.Fatalf("moved to a registered Team changed: %+v", s)
	}
	if after := mustState(t, f.db, "fail"); after != beforeFail {
		t.Fatalf("refetch failure changed the row: %+v -> %+v", beforeFail, after)
	}

	// The failed refetch is checked again next round; the others are not.
	f.fetcher.calls = nil
	delete(f.fetcher.errs, "fail")
	f.fetcher.issues["fail"] = linear.Issue{ID: "fail", Identifier: "ENG-fail", TeamID: "team-a", Labels: []string{"route:one"}}
	f.mustRound(t, "team-a")
	if !slices.Contains(f.fetcher.calls, "fail") || slices.Contains(f.fetcher.calls, "label") {
		t.Fatalf("refetches = %v", f.fetcher.calls)
	}
	if last := f.canceller.calls[len(f.canceller.calls)-1]; last != (cancelCall{"fail", ReasonLabelRemoved}) {
		t.Fatalf("retried refetch: last cancel = %v", last)
	}
}

// 3.4 の1つ目の表のキャンセルの2行。
func TestCancelledIssueInList(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	f.mustRound(t, "team-a", readyIssue("a", base))
	cancelledA := readyIssue("a", base.Add(time.Hour))
	cancelledA.StateType = "canceled"
	newCancelled := readyIssue("n", base)
	newCancelled.StateType = "canceled"
	f.mustRound(t, "team-a", cancelledA, newCancelled)
	if !slices.Equal(f.canceller.calls, []cancelCall{{"a", ReasonIssueCancelled}}) {
		t.Fatalf("cancels = %v", f.canceller.calls)
	}
	if s := mustState(t, f.db, "n"); !s.aiReady || s.seq != 0 || s.decision != string(DecisionSkippedCancelled) {
		t.Fatalf("new cancelled issue: %+v", s)
	}
	if len(f.starter.calls) != 1 {
		t.Fatalf("cancelled issue started: %v", f.starter.calls)
	}
	// Staying cancelled does not cancel again every round.
	f.mustRound(t, "team-a", cancelledA, newCancelled)
	if len(f.canceller.calls) != 1 {
		t.Fatalf("cancelled again: %v", f.canceller.calls)
	}
}

// 受け入れ基準: 外して付け直した Issue で、前のタスクが終端なら新しいタスクができ、終端でなければできない。
func TestReattach(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	f.mustRound(t, "team-a", readyIssue("a", base))
	detached := readyIssue("a", base)
	detached.Labels = []string{"route:one"}
	f.fetcher.issues["a"] = detached

	// The first task is not terminal (the stub canceller leaves it queued).
	f.mustRound(t, "team-a")
	f.mustRound(t, "team-a", readyIssue("a", base))
	if len(f.starter.calls) != 1 {
		t.Fatalf("started while a non-terminal task exists: %v", f.starter.calls)
	}
	if s := mustState(t, f.db, "a"); !s.aiReady || s.seq != 2 || s.decision != string(DecisionSkippedActiveTask) {
		t.Fatalf("state = %+v, want seq 2 skipped_active_task", s)
	}
	if countTasks(t, f.db, "a") != 1 {
		t.Fatal("second task created while the first is active")
	}

	for _, status := range []string{"completed", "failed", "cancelled", "timed_out"} {
		exec(t, f.db, `UPDATE tasks SET status = ? WHERE source = 'linear'`, status)
		f.mustRound(t, "team-a")
		f.mustRound(t, "team-a", readyIssue("a", base))
		last := f.starter.calls[len(f.starter.calls)-1]
		if last.issueID != "a" || last.seq != mustState(t, f.db, "a").seq {
			t.Fatalf("%s: last start = %v", status, last)
		}
		if s := mustState(t, f.db, "a"); s.decision != string(DecisionStarted) {
			t.Fatalf("%s: state = %+v", status, s)
		}
	}
	if n := countTasks(t, f.db, "a"); n != 5 {
		t.Fatalf("tasks = %d, want 5 (one per terminal reattach plus the first)", n)
	}
}

// 受け入れ基準: 判定の途中で失敗させても、次の照会でタスクが二重にできない。
func TestFailureMidDecisionDoesNotDuplicate(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	failing := true
	f.starter.fail = func(id string, _ int) bool { return id == "b" && failing }

	err := f.round(t, "team-a", readyIssue("a", base), readyIssue("b", base), readyIssue("c", base))
	if err == nil || !strings.Contains(err.Error(), "issue b") {
		t.Fatalf("error = %v, want the failure of issue b", err)
	}
	if _, ok := readState(t, f.db, "b"); ok {
		t.Fatal("failed issue's record was written")
	}
	if countTasks(t, f.db, "b") != 0 {
		t.Fatal("failed issue's task was kept")
	}
	for _, id := range []string{"a", "c"} {
		if countTasks(t, f.db, id) != 1 {
			t.Fatalf("%s: other issues did not proceed", id)
		}
	}

	failing = false
	f.mustRound(t, "team-a", readyIssue("a", base), readyIssue("b", base), readyIssue("c", base))
	f.mustRound(t, "team-a", readyIssue("a", base), readyIssue("b", base), readyIssue("c", base))
	for _, id := range []string{"a", "b", "c"} {
		if n := countTasks(t, f.db, id); n != 1 {
			t.Fatalf("%s: %d tasks, want 1", id, n)
		}
		if s := mustState(t, f.db, id); s.seq != 1 {
			t.Fatalf("%s: attach_seq = %d, want 1", id, s.seq)
		}
	}
	if want := []startCall{{"a", 1}, {"b", 1}, {"c", 1}, {"b", 1}}; !slices.Equal(f.starter.calls, want) {
		t.Fatalf("starts = %v, want %v (b retried with the same seq)", f.starter.calls, want)
	}
}

// A failure while recording the first poll keeps the Team in its first poll,
// so the unrecorded Issue is not started next round.
func TestFirstPollFailureKeepsBaseline(t *testing.T) {
	f := newIntakeFixture(t)
	exec(t, f.db, `CREATE TRIGGER fail_b BEFORE INSERT ON linear_issue_states
		WHEN NEW.linear_issue_id = 'b' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	if err := f.round(t, "team-a", readyIssue("a", base), readyIssue("b", base)); err == nil {
		t.Fatal("injected failure not reported")
	}
	if baselineDone(t, f.db, "team-a") {
		t.Fatal("first poll marked done despite a failed Issue")
	}
	exec(t, f.db, `DROP TRIGGER fail_b`)
	f.mustRound(t, "team-a", readyIssue("a", base), readyIssue("b", base))
	if len(f.starter.calls) != 0 || !baselineDone(t, f.db, "team-a") {
		t.Fatalf("starts = %v baseline=%v", f.starter.calls, baselineDone(t, f.db, "team-a"))
	}
	if s := mustState(t, f.db, "b"); s.decision != string(DecisionSkippedInitial) {
		t.Fatalf("b: %+v", s)
	}
}

// A failure after the start (while saving the record) rolls the task back
// too, so the retry with the same attach_seq does not hit the unique key.
func TestFailureAfterStartRollsBackTask(t *testing.T) {
	f := newIntakeFixture(t)
	f.baseline(t)
	exec(t, f.db, `CREATE TRIGGER fail_a BEFORE INSERT ON linear_issue_states
		WHEN NEW.linear_issue_id = 'a' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	if err := f.round(t, "team-a", readyIssue("a", base)); err == nil {
		t.Fatal("injected failure not reported")
	}
	if countTasks(t, f.db, "a") != 0 {
		t.Fatal("task kept although the record was not saved")
	}
	exec(t, f.db, `DROP TRIGGER fail_a`)
	f.mustRound(t, "team-a", readyIssue("a", base))
	f.mustRound(t, "team-a", readyIssue("a", base))
	if n := countTasks(t, f.db, "a"); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}
}

// The Poller passes each Team's state, so the first poll and later attaches
// are decided per Team through the Poller as well.
func TestPollerWithIntake(t *testing.T) {
	f := newIntakeFixture(t)
	lists := map[string][]linear.Issue{"team-a": {readyIssue("a", base)}}
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) { return lists[teamID], nil }}
	p := newPoller(f.db, client, nil)
	p.Handler = f.intake
	pollOnce(t, p)
	if len(f.starter.calls) != 0 {
		t.Fatalf("first poll started: %v", f.starter.calls)
	}
	lists["team-a"] = append(lists["team-a"], readyIssue("b", base))
	pollOnce(t, p)
	pollOnce(t, p)
	if !slices.Equal(f.starter.calls, []startCall{{"b", 1}}) {
		t.Fatalf("starts = %v, want b seq 1 once", f.starter.calls)
	}
}

func TestIntakeRequiresStarterAndCanceller(t *testing.T) {
	f := newIntakeFixture(t)
	in := &Intake{DB: f.db, Fetcher: f.fetcher}
	if err := in.HandleTeamIssues(context.Background(), store.LinearPollTeam{TeamID: "team-a"}, []linear.Issue{readyIssue("a", base)}); err == nil {
		t.Fatal("unconfigured intake accepted a list")
	}
	if _, ok := readState(t, f.db, "a"); ok {
		t.Fatal("unconfigured intake recorded an attach")
	}
}
