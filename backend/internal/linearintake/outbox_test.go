package linearintake

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// outboxKeys returns the task's linear_outbox rows as kind -> dedupe_key,
// and fails on a row that is not a pending comment for Issue "1".
func outboxKeys(t *testing.T, db *sql.DB, taskID string) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT kind, dedupe_key, linear_issue_id, status FROM linear_outbox
		WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][]string{}
	for rows.Next() {
		var kind, key, issue, status string
		if err := rows.Scan(&kind, &key, &issue, &status); err != nil {
			t.Fatal(err)
		}
		if issue != "1" || status != "pending" {
			t.Fatalf("outbox row %s: issue %q status %q", key, issue, status)
		}
		got[kind] = append(got[kind], key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// spec 16 3.2: a routing hold and a missing-criteria hold each write their
// comment in the hold's transaction; a routed Issue writes none.
func TestHoldWritesOutboxRow(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel)
	got := outboxKeys(t, f.db, held.id)
	if len(got) != 1 || len(got["intake_blocked"]) != 1 ||
		got["intake_blocked"][0] != held.id+":intake_blocked:routing_unknown_label" {
		t.Fatalf("outbox = %v", got)
	}

	f2 := newRecheckFixture(t)
	issue := routedIssue("1", "repo:one")
	issue.Description = "no criteria"
	held2 := f2.hold(t, issue, CauseMissingAcceptanceCriteria)
	got = outboxKeys(t, f2.db, held2.id)
	if len(got) != 1 || len(got["missing_acceptance_criteria"]) != 1 ||
		got["missing_acceptance_criteria"][0] != held2.id+":missing_acceptance_criteria:missing_acceptance_criteria" {
		t.Fatalf("outbox = %v", got)
	}

	f3 := newRecheckFixture(t)
	f3.mustRound(t, routedIssue("1", "repo:one"))
	if n := countRows(t, f3.db, `SELECT count(*) FROM linear_outbox`); n != 0 {
		t.Fatalf("started task wrote %d outbox rows", n)
	}
}

// spec 16 3.5 rule 3: a recheck with the same cause writes no second
// comment; a different cause writes one for the new cause; a release
// writes none.
func TestRecheckOutboxRows(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel)

	same := routedIssue("1", "repo:other")
	same.UpdatedAt = base.Add(time.Minute)
	f.mustRound(t, same)
	if got := outboxKeys(t, f.db, held.id); len(got["intake_blocked"]) != 1 {
		t.Fatalf("same cause outbox = %v", got)
	}

	changed := routedIssue("1", "repo:one", "repo:three")
	changed.UpdatedAt = base.Add(2 * time.Minute)
	f.mustRound(t, changed)
	got := outboxKeys(t, f.db, held.id)
	if len(got["intake_blocked"]) != 2 || got["intake_blocked"][1] != held.id+":intake_blocked:routing_multiple_labels" {
		t.Fatalf("changed cause outbox = %v", got)
	}

	fixed := routedIssue("1", "repo:one")
	fixed.UpdatedAt = base.Add(3 * time.Minute)
	f.mustRound(t, fixed)
	if got := outboxKeys(t, f.db, held.id); len(got) != 1 || len(got["intake_blocked"]) != 2 {
		t.Fatalf("released task outbox = %v", got)
	}
}

// spec 16 3.2: a Linear-caused cancellation comments, both for a held task
// (no Execution: keyed on the task) and for a queued task (keyed on its
// Execution). The owner's cancellation takes the same CancelTaskTx path.
func TestCancellationWritesOutboxRow(t *testing.T) {
	for _, state := range []string{"held", "queued", "blocked"} {
		t.Run(state, func(t *testing.T) {
			f := newCancelFixture(t)
			issue, taskID := f.attach(t, state)
			before := outboxKeys(t, f.db, taskID)
			detachTriggers[0].run(t, f, issue)
			got := outboxKeys(t, f.db, taskID)
			want := taskID
			if state != "held" {
				if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, taskID).Scan(&want); err != nil {
					t.Fatal(err)
				}
			}
			if len(got["cancelled"]) != 1 || got["cancelled"][0] != taskID+":cancelled:"+want ||
				len(got)-len(before) != 1 {
				t.Fatalf("outbox = %v (before %v)", got, before)
			}
		})
	}
}

func outboxBody(t *testing.T, db *sql.DB, taskID string) string {
	t.Helper()
	var body string
	if err := db.QueryRow(`SELECT body FROM linear_outbox WHERE task_id = ?`, taskID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func bodyHas(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q:\n%s", s, body)
		}
	}
}

// 受け入れ基準: ルーティングの保留では理由と直し方が載る。Each routing
// hold (and the assignment hold) of spec 15 3.6 comments its reason and its
// fix (spec 16 3.2), naming the routing label prefix the decision used.
func TestRoutingHoldCommentHasReasonAndFix(t *testing.T) {
	for _, test := range []struct {
		name        string
		issue       linear.Issue
		cause       BlockCause
		reason, fix string
	}{
		{"two labels", routedIssue("1", "repo:one", "repo:three"), CauseRoutingMultipleLabels,
			"実行先ラベル（repo: で始まるラベル）が2つ以上付いている", "直し方: 実行先ラベルを1つにする"},
		{"unknown label", routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel,
			"実行先ラベル（repo: で始まるラベル）が Evaloom に登録されていない", "直し方: Evaloom に登録済みの実行先ラベルを付ける"},
		{"team mismatch", routedIssue("1", "repo:two"), CauseRoutingTeamMismatch,
			"Linear Team がこの Issue の Team と違う", "直し方: この Issue の Team で登録済みの実行先ラベルを付ける"},
		{"no label", routedIssue("1", "bug"), CauseRoutingNoLabel,
			"実行先ラベル（repo: で始まるラベル）が付いていない", "直し方: Evaloom に登録済みの実行先ラベルを1つ付ける"},
		{"no assignment", routedIssue("1", "repo:three"), CauseConfigMissingAssignment,
			"default role にハーネスが割り当てられていない", "直し方: 管理画面で Project の default role にハーネスを割り当てる"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			held := f.hold(t, test.issue, test.cause)
			body := outboxBody(t, f.db, held.id)
			bodyHas(t, body, "- 実行ステータス: Blocked", "管理画面のタスク ID: "+held.id,
				"- 保留の理由: ", test.reason, test.fix)
			if strings.Contains(body, "受け入れ条件の記入") {
				t.Errorf("routing hold asks for acceptance criteria:\n%s", body)
			}
		})
	}
}

// The prefix in the reason is the round's setting, not a fixed "repo:".
func TestHoldCommentUsesRoundPrefix(t *testing.T) {
	f := newRecheckFixture(t)
	f.intake.Starter.(*TaskStarter).RoutingLabelPrefix = "run:"
	held := f.hold(t, routedIssue("1", "bug"), CauseRoutingNoLabel)
	bodyHas(t, outboxBody(t, f.db, held.id), "実行先ラベル（run: で始まるラベル）が付いていない")
}

// 受け入れ基準: missing_acceptance_criteria で保留したタスクの Issue に記入
// 依頼のコメントが1回付く。The hold writes one row asking for the criteria
// with the heading to write (spec 15 3.6.1); later updates of the Issue that
// still lack the criteria write no second row (spec 16 3.5 rule 3, 3.7).
func TestMissingCriteriaHoldAsksOnce(t *testing.T) {
	f := newRecheckFixture(t)
	issue := routedIssue("1", "repo:one")
	issue.Description = "no criteria"
	held := f.hold(t, issue, CauseMissingAcceptanceCriteria)
	bodyHas(t, outboxBody(t, f.db, held.id), "受け入れ条件の記入をお願いします",
		"保留の理由: Issue の本文に受け入れ条件が書かれていない",
		"「受け入れ条件」または「Acceptance Criteria」という見出し", "## 受け入れ条件")
	for i := 1; i <= 3; i++ {
		issue.Description = "still no criteria " + strings.Repeat("x", i)
		issue.UpdatedAt = base.Add(time.Duration(i) * time.Minute)
		f.mustRound(t, issue)
	}
	if got := outboxKeys(t, f.db, held.id); len(got) != 1 || len(got["missing_acceptance_criteria"]) != 1 {
		t.Fatalf("outbox = %v", got)
	}
}

// outboxExecutionIDs returns the task's linear_outbox rows as kind ->
// execution_id ("" for NULL).
func outboxExecutionIDs(t *testing.T, db *sql.DB, taskID string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT kind, execution_id FROM linear_outbox WHERE task_id = ?`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var kind string
		var executionID sql.NullString
		if err := rows.Scan(&kind, &executionID); err != nil {
			t.Fatal(err)
		}
		got[kind] = executionID.String
		if executionID.Valid && executionID.String == "" {
			t.Fatalf("%s: execution_id is an empty string, not NULL", kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// 受け入れ基準 (spec 16 3.4.2, 3.7, Q82): Execution の契機の行を書くと
// execution_id が入り、intake_blocked・missing_acceptance_criteria・保留した
// タスクの中止の行では NULL になる。
func TestOutboxExecutionIDByTrigger(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel)
	if got := outboxExecutionIDs(t, f.db, held.id); len(got) != 1 || got["intake_blocked"] != "" {
		t.Fatalf("intake_blocked execution_id = %v", got)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM linear_outbox WHERE kind = 'intake_blocked' AND execution_id IS NULL`); n != 1 {
		t.Fatalf("intake_blocked rows with NULL execution_id = %d", n)
	}

	f2 := newRecheckFixture(t)
	issue := routedIssue("1", "repo:one")
	issue.Description = "no criteria"
	held2 := f2.hold(t, issue, CauseMissingAcceptanceCriteria)
	if n := countRows(t, f2.db, `SELECT count(*) FROM linear_outbox WHERE task_id = '`+held2.id+`'
		AND kind = 'missing_acceptance_criteria' AND execution_id IS NULL`); n != 1 {
		t.Fatalf("missing_acceptance_criteria rows with NULL execution_id = %d (%v)", n, outboxExecutionIDs(t, f2.db, held2.id))
	}

	for _, state := range []string{"held", "queued", "blocked"} {
		t.Run(state, func(t *testing.T) {
			f := newCancelFixture(t)
			issue, taskID := f.attach(t, state)
			detachTriggers[0].run(t, f, issue)
			want := ""
			if state != "held" {
				if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, taskID).Scan(&want); err != nil {
					t.Fatal(err)
				}
			}
			got := outboxExecutionIDs(t, f.db, taskID)
			if got["cancelled"] != want {
				t.Fatalf("cancelled execution_id = %q; want %q (%v)", got["cancelled"], want, got)
			}
			if state == "held" {
				if n := countRows(t, f.db, `SELECT count(*) FROM linear_outbox WHERE task_id = '`+taskID+`'
					AND kind = 'cancelled' AND execution_id IS NULL`); n != 1 {
					t.Fatalf("held cancellation rows with NULL execution_id = %d", n)
				}
			}
		})
	}
}
