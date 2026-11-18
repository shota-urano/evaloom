package linearintake

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// heldForCriteria attaches Issue "1" with route:one and a body without
// acceptance criteria, so the task is held (missing_acceptance_criteria).
func (f *recheckFixture) heldForCriteria(t *testing.T) (linear.Issue, startedTask) {
	t.Helper()
	issue := routedIssue("1", "route:one")
	issue.Description = "no criteria"
	return issue, f.hold(t, issue, string(CauseMissingAcceptanceCriteria))
}

// unrouted removes the routing label from issue and moves updatedAt forward.
func unrouted(issue linear.Issue, at time.Time) linear.Issue {
	issue.Labels = []string{"ai-ready", "bug"}
	issue.UpdatedAt = at
	return issue
}

func proposalOf(t *testing.T, db *sql.DB, taskID string) (id, status, trigger, seen string) {
	t.Helper()
	if err := db.QueryRow(`SELECT id, status, trigger, seen_labels FROM route_proposals
		WHERE task_id = ? ORDER BY id DESC LIMIT 1`, taskID).Scan(&id, &status, &trigger, &seen); err != nil {
		t.Fatal(err)
	}
	return id, status, trigger, seen
}

func reportProposal(t *testing.T, db *sql.DB, in store.RouteProposalResult) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReportRouteProposalResultTx(context.Background(), tx, in); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func addRunner(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT OR IGNORE INTO runners (id, name, token_hash, status, created_at) VALUES ('r1', 'r1', 'h', 'online', 't')`)
}

// openRouteDecisions counts the task's open kind=route decisions whose
// subject and basis are the proposal (spec 15 3.6.4 step 5).
func openRouteDecisions(t *testing.T, db *sql.DB, taskID, proposalID string) int {
	t.Helper()
	return countRows(t, db, `SELECT count(*) FROM owner_decisions d JOIN route_proposals rp ON rp.decision_id = d.id
		WHERE d.task_id = ? AND d.kind = 'route' AND d.status = 'open' AND d.subject_id = ? AND d.basis = ? AND rp.id = ?`,
		taskID, proposalID, proposalID, proposalID)
}

// 受け入れ基準: 保留中のタスクの Issue から振り分けのラベルを外して更新時刻が進むと、
// タスクが routing になり、queued の route_proposals と、提案が proposed か failed に
// なった後の kind=route の判断待ちができる（spec 15 3.6.3、3.6.4、Q219）。
func TestRecheckHeldTaskBackToRoutingRemakesProposal(t *testing.T) {
	t.Run("proposed by a Runner", func(t *testing.T) {
		f := newRecheckFixture(t)
		issue, held := f.heldForCriteria(t)
		if n := countRows(t, f.db, `SELECT count(*) FROM route_proposals WHERE task_id = ?`, held.id); n != 0 {
			t.Fatalf("held task has %d proposals", n)
		}
		f.now = base.Add(2 * time.Minute)
		f.mustRound(t, unrouted(issue, base.Add(time.Minute)))

		got := readStarted(t, f.db, "1", 1)
		if got.id != held.id || got.status != "routing" || got.cause.Valid || got.project.Valid || got.executions != 0 {
			t.Fatalf("task = %+v", got)
		}
		if n := countTasks(t, f.db, "1"); n != 1 {
			t.Fatalf("tasks = %d; recheck must not create a task", n)
		}
		if s := mustState(t, f.db, "1"); s.decision != string(DecisionRouting) {
			t.Fatalf("state = %+v", s)
		}
		id, status, trigger, seen := proposalOf(t, f.db, held.id)
		if status != "queued" || trigger != string(TriggerNoLabel) || seen != "[]" {
			t.Fatalf("proposal = %s %s %s", status, trigger, seen)
		}
		if candidates := countRows(t, f.db, `SELECT json_array_length(candidates) FROM route_proposals WHERE id = ?`, id); candidates == 0 {
			t.Fatal("proposal has no candidates")
		}
		// No decision before the proposal is proposed or failed.
		if n := countRows(t, f.db, `SELECT count(*) FROM owner_decisions WHERE task_id = ?`, held.id); n != 0 {
			t.Fatalf("decisions before the result = %d", n)
		}
		if n := countRows(t, f.db, `SELECT count(*) FROM audit_logs WHERE action = 'task.intake_recheck' AND target_id = ?
			AND json_extract(detail, '$.routeProposalId') = ? AND json_extract(detail, '$.to') = 'routing'`, held.id, id); n != 1 {
			t.Fatalf("recheck audits with the proposal = %d", n)
		}

		addRunner(t, f.db)
		exec(t, f.db, `UPDATE route_proposals SET status = 'running', runner_id = 'r1' WHERE id = ?`, id)
		label, reason := "route:three", "fits"
		reportProposal(t, f.db, store.RouteProposalResult{ProposalID: id, RunnerID: "r1", Label: &label, Reason: &reason})
		if _, status, _, _ := proposalOf(t, f.db, held.id); status != "proposed" {
			t.Fatalf("proposal status = %s", status)
		}
		if n := openRouteDecisions(t, f.db, held.id, id); n != 1 {
			t.Fatalf("open route decisions = %d", n)
		}
		if keys := outboxKeys(t, f.db, held.id); len(keys["decision_open"]) != 1 {
			t.Fatalf("outbox = %v; want one decision_open comment", keys)
		}
	})
	t.Run("failed without a proposer model", func(t *testing.T) {
		f := newRecheckFixture(t)
		setProposerModel(t, f.db, "null")
		issue, held := f.heldForCriteria(t)
		f.mustRound(t, unrouted(issue, base.Add(time.Minute)))
		if got := readStarted(t, f.db, "1", 1); got.status != "routing" || got.cause.Valid {
			t.Fatalf("task = %+v", got)
		}
		id, status, trigger, _ := proposalOf(t, f.db, held.id)
		if status != "failed" || trigger != string(TriggerNoLabel) {
			t.Fatalf("proposal = %s %s", status, trigger)
		}
		if n := countRows(t, f.db, `SELECT count(*) FROM route_proposals WHERE id = ? AND error_class IS NULL AND runner_id IS NULL`, id); n != 1 {
			t.Fatal("failed proposal has an error class or a Runner")
		}
		if n := openRouteDecisions(t, f.db, held.id, id); n != 1 {
			t.Fatalf("open route decisions = %d", n)
		}
	})
	t.Run("unknown label", func(t *testing.T) {
		f := newRecheckFixture(t)
		issue, held := f.heldForCriteria(t)
		issue.Labels = []string{"ai-ready", "route:unknown"}
		issue.UpdatedAt = base.Add(time.Minute)
		f.mustRound(t, issue)
		if _, status, trigger, seen := proposalOf(t, f.db, held.id); status != "queued" ||
			trigger != string(TriggerUnknownLabel) || seen != `["route:unknown"]` {
			t.Fatalf("proposal = %s %s %s", status, trigger, seen)
		}
	})
}

// 受け入れ基準: 同じ Issue でラベルが変わらない照会では提案が増えない。
func TestRecheckBackToRoutingMakesOneProposal(t *testing.T) {
	f := newRecheckFixture(t)
	issue, held := f.heldForCriteria(t)
	issue = unrouted(issue, base.Add(time.Minute))
	f.mustRound(t, issue)
	if n := countRows(t, f.db, `SELECT count(*) FROM route_proposals WHERE task_id = ?`, held.id); n != 1 {
		t.Fatalf("proposals = %d", n)
	}
	// The same updatedAt: no recheck.
	f.mustRound(t, issue)
	// A newer updatedAt with the same labels (a body edit): still routing for
	// the same reason, so no new proposal (Q159).
	issue.Description = "edited"
	issue.UpdatedAt = base.Add(time.Hour)
	f.mustRound(t, issue)
	// A recheck mark from an Evaloom setting change with the same labels.
	exec(t, f.db, `UPDATE linear_issue_states SET recheck_requested_at = ? WHERE linear_issue_id = '1'`, stamp(base))
	f.mustRound(t, issue)
	if n := countRows(t, f.db, `SELECT count(*) FROM route_proposals WHERE task_id = ?`, held.id); n != 1 {
		t.Fatalf("proposals = %d; want 1", n)
	}
	if _, status, _, _ := proposalOf(t, f.db, held.id); status != "queued" {
		t.Fatalf("proposal status = %s", status)
	}
	if got := readStarted(t, f.db, "1", 1); got.status != "routing" || countTasks(t, f.db, "1") != 1 {
		t.Fatalf("task = %+v", got)
	}
}

// 受け入れ基準: running の提案を superseded にしても Runner に中止を求めず、
// 後から届いた結果は記録だけ残ってタスクに使われない。
// A routing task's running proposal is superseded when a label routes it
// to a destination that holds it; the label is then removed, so a new
// proposal is made. The old Runner keeps running, and its late result is
// only recorded: the task, the new proposal and the decisions are untouched.
func TestRecheckSupersededRunningProposalIsNotStopped(t *testing.T) {
	f := newRecheckFixture(t)
	issue := routedIssue("1", "bug")
	issue.Description = "no criteria"
	f.mustRound(t, issue)
	task := readStarted(t, f.db, "1", 1)
	oldID, _, _, _ := proposalOf(t, f.db, task.id)
	addRunner(t, f.db)
	exec(t, f.db, `UPDATE route_proposals SET status = 'running', runner_id = 'r1' WHERE id = ?`, oldID)

	labelled := issue
	labelled.Labels = []string{"ai-ready", "route:one"}
	labelled.UpdatedAt = base.Add(time.Minute)
	f.mustRound(t, labelled)
	if got := readStarted(t, f.db, "1", 1); got.status != "blocked" || got.cause.String != string(CauseMissingAcceptanceCriteria) {
		t.Fatalf("task = %+v", got)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM route_proposals WHERE id = ? AND status = 'superseded'`, oldID); n != 1 {
		t.Fatal("running proposal not superseded")
	}
	requireNoProposalCancel(t, f.db, oldID)

	f.now = base.Add(3 * time.Minute) // the new proposal's ULID sorts after the old one
	f.mustRound(t, unrouted(labelled, base.Add(2*time.Minute)))
	newID, status, _, _ := proposalOf(t, f.db, task.id)
	if newID == oldID || status != "queued" {
		t.Fatalf("new proposal = %s %s", newID, status)
	}
	requireNoProposalCancel(t, f.db, oldID)

	label, reason := "route:one", "late"
	reportProposal(t, f.db, store.RouteProposalResult{ProposalID: oldID, RunnerID: "r1", Label: &label, Reason: &reason})
	if n := countRows(t, f.db, `SELECT count(*) FROM route_proposals WHERE id = ? AND status = 'superseded'
		AND proposed_label = 'route:one' AND reason = 'late' AND decision_id IS NULL`, oldID); n != 1 {
		t.Fatal("late result was not only recorded")
	}
	if got := readStarted(t, f.db, "1", 1); got.status != "routing" || got.project.Valid || got.executions != 0 {
		t.Fatalf("late result changed the task: %+v", got)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM owner_decisions WHERE task_id = ?`, task.id); n != 0 {
		t.Fatalf("late result opened %d decisions", n)
	}
	if _, status, _, _ := proposalOf(t, f.db, task.id); status != "queued" {
		t.Fatalf("new proposal status = %s", status)
	}
}

// requireNoProposalCancel checks the heartbeat asks the Runner to stop none
// of its proposals, while it still reports the superseded one as running.
func requireNoProposalCancel(t *testing.T, db *sql.DB, proposalID string) {
	t.Helper()
	reply, err := store.HeartbeatWithProposals(context.Background(), db, "r1", "boot", nil, []string{proposalID})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.CancelRouteProposalIDs) != 0 {
		t.Fatalf("cancelRouteProposalIds = %v; a superseded proposal is not stopped", reply.CancelRouteProposalIDs)
	}
}
