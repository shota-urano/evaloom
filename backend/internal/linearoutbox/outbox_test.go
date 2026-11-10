package linearoutbox_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/shota-urano/evaloom/backend/internal/testregistry"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const testTime = "2026-11-08T00:00:00Z"

// outboxDB has a Linear task "linear-1" linked to Issue "issue-1" and a
// manual task "manual-1", each with Execution "<task>-e1" in the given states.
func outboxDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := testregistry.Open(context.Background(), filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `INSERT INTO runners (id, name, token_hash, status, last_heartbeat_at, created_at) VALUES ('runner', 'Runner', 'hash', 'online', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO projects (id, name, runner_id, created_at, updated_at, harness_id, profile_id) VALUES ('project', 'Project', 'runner', ?, ?, 'fixture-harness', 'fixture-profile')`, testTime, testTime)
	for _, source := range []string{"linear", "manual"} {
		id := source + "-1"
		mustExec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at, source)
			VALUES (?, 'project', 'Title', 'Body', 'tests pass', 'solo', 'queued', ?, ?, ?, ?)`,
			id, "key-"+id, testTime, testTime, source)
		mustExec(t, db, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
			VALUES (?, ?, 1, 'continue', 'pending', ?)`, id+"-e1", id, testTime)
	}
	mustExec(t, db, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id,
		task_id, attach_seq, created_at) VALUES ('link-1', 'issue-1', 'ENG-1', 'https://linear.app/x/ENG-1',
		'team-a', 'linear-1', 1, ?)`, testTime)
	return db
}

func setStates(t *testing.T, db *sql.DB, taskID string, taskStatus model.TaskStatus, execution model.ExecutionStatus) {
	t.Helper()
	mustExec(t, db, `UPDATE tasks SET status = ? WHERE id = ?`, taskStatus, taskID)
	mustExec(t, db, `UPDATE executions SET status = ? WHERE id = ?`, execution, taskID+"-e1")
}

func transition(t *testing.T, db *sql.DB, taskID string, fromTask model.TaskStatus, from, to model.ExecutionStatus) {
	t.Helper()
	if err := task.TransitionExecution(context.Background(), db, task.Transition{TaskID: taskID,
		ExecutionID: taskID + "-e1", FromTask: fromTask, FromExecution: from, ToExecution: to}); err != nil {
		t.Fatal(err)
	}
}

type outboxRow struct {
	taskID, issueID, kind, dedupeKey, body, status string
	attempts                                       int
	nextAttemptAt, commentID, sentAt               sql.NullString
	createdAt                                      string
}

func outboxRows(t *testing.T, db *sql.DB) []outboxRow {
	t.Helper()
	// These existing assertions cover comment payloads. State changes have
	// NULL bodies and are checked independently in state_test.go.
	rows, err := db.Query(`SELECT task_id, linear_issue_id, kind, dedupe_key, body, status, attempts,
		next_attempt_at, linear_comment_id, sent_at, created_at FROM linear_outbox WHERE action = 'comment' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.taskID, &r.issueID, &r.kind, &r.dedupeKey, &r.body, &r.status, &r.attempts,
			&r.nextAttemptAt, &r.commentID, &r.sentAt, &r.createdAt); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// 受け入れ基準: source=linear のタスクが succeeded になると outbox に1行できる。
func TestLinearTaskSucceededWritesOneOutboxRow(t *testing.T) {
	db := outboxDB(t)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing, model.ExecutionSucceeded)

	rows := outboxRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %+v; want one", rows)
	}
	r := rows[0]
	if r.taskID != "linear-1" || r.issueID != "issue-1" || r.kind != "succeeded" ||
		r.dedupeKey != "linear-1:succeeded:linear-1-e1" || r.status != "pending" || r.attempts != 0 ||
		r.nextAttemptAt.Valid || r.commentID.Valid || r.sentAt.Valid || r.createdAt == "" {
		t.Fatalf("outbox row = %+v", r)
	}
	// The body ends with the dedupe_key so an unknown send can be matched
	// (spec 16 3.4) and names the task by ID only (3.3).
	if !strings.HasSuffix(r.body, r.dedupeKey) || !strings.Contains(r.body, "linear-1") {
		t.Fatalf("body = %q", r.body)
	}
}

// 受け入れ基準: 同じ契機を2回発生させても dedupe_key の一意制約で行が1つ。
func TestSameTriggerTwiceKeepsOneRow(t *testing.T) {
	t.Run("enqueue twice", func(t *testing.T) {
		db := outboxDB(t)
		trigger := linearoutbox.Trigger{TaskID: "linear-1", Kind: linearoutbox.KindSucceeded, TargetID: "linear-1-e1"}
		for range 2 {
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := linearoutbox.EnqueueTx(context.Background(), tx, trigger); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if rows := outboxRows(t, db); len(rows) != 1 {
			t.Fatalf("outbox rows = %+v; want one", rows)
		}
	})
	t.Run("blocked twice on the same execution", func(t *testing.T) {
		// The production path: a blocked Execution is continued by the owner
		// and blocked again. Its blocked comment is written once.
		db := outboxDB(t)
		setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning)
		transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionBlocked)
		transition(t, db, "linear-1", model.TaskBlocked, model.ExecutionBlocked, model.ExecutionPending)
		transition(t, db, "linear-1", model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed)
		transition(t, db, "linear-1", model.TaskRunning, model.ExecutionClaimed, model.ExecutionBlocked)
		rows := outboxRows(t, db)
		if len(rows) != 1 || rows[0].dedupeKey != "linear-1:blocked:linear-1-e1" {
			t.Fatalf("outbox rows = %+v; want one blocked row", rows)
		}
	})
	t.Run("schema rejects a duplicate key", func(t *testing.T) {
		db := outboxDB(t)
		insert := `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body, status,
			attempts, created_at) VALUES (?, 'linear-1', 'issue-1', 'succeeded', 'linear-1:succeeded:x', 'b',
			'pending', 0, ?)`
		mustExec(t, db, insert, "row-1", testTime)
		_, err := db.Exec(insert, "row-2", testTime)
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			t.Fatalf("duplicate dedupe_key err = %v; want UNIQUE violation", err)
		}
		if rows := outboxRows(t, db); len(rows) != 1 {
			t.Fatalf("outbox rows = %+v; want one", rows)
		}
	})
}

// 受け入れ基準: source=manual のタスクでは行ができない。
func TestManualTaskWritesNoOutboxRow(t *testing.T) {
	cases := []struct {
		fromTask model.TaskStatus
		from, to model.ExecutionStatus
	}{
		{model.TaskRunning, model.ExecutionPublishing, model.ExecutionSucceeded},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionInterrupted},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionTimedOut},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionBlocked},
		{model.TaskQueued, model.ExecutionPending, model.ExecutionCancelled},
	}
	for _, tc := range cases {
		t.Run(string(tc.to), func(t *testing.T) {
			db := outboxDB(t)
			setStates(t, db, "manual-1", tc.fromTask, tc.from)
			transition(t, db, "manual-1", tc.fromTask, tc.from, tc.to)
			if rows := outboxRows(t, db); len(rows) != 0 {
				t.Fatalf("manual task wrote outbox rows: %+v", rows)
			}
		})
	}
	// Enqueueing a manual task directly writes nothing either.
	db := outboxDB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := linearoutbox.EnqueueTx(context.Background(), tx, linearoutbox.Trigger{
		TaskID: "manual-1", Kind: linearoutbox.KindSucceeded, TargetID: "manual-1-e1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, db); len(rows) != 0 {
		t.Fatalf("manual task wrote outbox rows: %+v", rows)
	}
}

// Every Execution trigger of spec 16 3.2 maps to its kind; intermediate
// steps write nothing.
func TestExecutionTriggerKinds(t *testing.T) {
	cases := []struct {
		fromTask model.TaskStatus
		from, to model.ExecutionStatus
		kind     string // empty: no row
	}{
		{model.TaskRunning, model.ExecutionPublishing, model.ExecutionSucceeded, "succeeded"},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed, "failed"},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionInterrupted, "failed"},
		{model.TaskRunning, model.ExecutionVerifying, model.ExecutionTimedOut, "failed"},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionBlocked, "blocked"},
		{model.TaskQueued, model.ExecutionPending, model.ExecutionCancelled, "cancelled"},
		{model.TaskBlocked, model.ExecutionBlocked, model.ExecutionCancelled, "cancelled"},
		{model.TaskRunning, model.ExecutionPublishing, model.ExecutionWaitingApproval, "waiting_approval"},
		{model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed, ""},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionVerifying, ""},
		{model.TaskRunning, model.ExecutionVerifying, model.ExecutionPublishing, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			db := outboxDB(t)
			setStates(t, db, "linear-1", tc.fromTask, tc.from)
			transition(t, db, "linear-1", tc.fromTask, tc.from, tc.to)
			rows := outboxRows(t, db)
			if tc.kind == "" {
				if len(rows) != 0 {
					t.Fatalf("outbox rows = %+v; want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].kind != tc.kind || rows[0].dedupeKey != "linear-1:"+tc.kind+":linear-1-e1" {
				t.Fatalf("outbox rows = %+v; want one %s", rows, tc.kind)
			}
		})
	}
}

// The row belongs to the transition's transaction (spec 16 3.4 step 1).
func TestOutboxRowRollsBackWithTransition(t *testing.T) {
	db := outboxDB(t)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := task.TransitionExecutionTx(context.Background(), tx, task.Transition{TaskID: "linear-1",
		ExecutionID: "linear-1-e1", FromTask: model.TaskRunning, FromExecution: model.ExecutionPublishing,
		ToExecution: model.ExecutionSucceeded}); err != nil {
		t.Fatal(err)
	}
	var inside int
	if err := tx.QueryRow(`SELECT count(*) FROM linear_outbox WHERE action = 'comment'`).Scan(&inside); err != nil || inside != 1 {
		t.Fatalf("rows inside the transaction = %d, err = %v", inside, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, db); len(rows) != 0 {
		t.Fatalf("rolled back transition left outbox rows: %+v", rows)
	}
}

// A Linear task without its Issue link is inconsistent data: the transition
// fails and nothing changes, instead of silently skipping the comment.
func TestLinearTaskWithoutLinkFailsTransition(t *testing.T) {
	db := outboxDB(t)
	mustExec(t, db, `DELETE FROM linear_issue_links`)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing)
	err := task.TransitionExecution(context.Background(), db, task.Transition{TaskID: "linear-1",
		ExecutionID: "linear-1-e1", FromTask: model.TaskRunning, FromExecution: model.ExecutionPublishing,
		ToExecution: model.ExecutionSucceeded})
	if err == nil || !strings.Contains(err.Error(), "no linked Issue") {
		t.Fatalf("err = %v; want missing link", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM executions WHERE id = 'linear-1-e1'`).Scan(&status); err != nil || status != "publishing" {
		t.Fatalf("execution = %q, err = %v; want unchanged", status, err)
	}
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func mustExec(t *testing.T, db execer, statement string, args ...any) {
	t.Helper()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

// spec 17 3.3 step 4, spec 16 3.2/3.3: a Linear task waiting for approval
// gets one comment naming the required approval, and a refused consume that
// returns the same Execution to the wait does not comment again.
func TestWaitingApprovalCommentNamesRequiredApprovalOnce(t *testing.T) {
	db := outboxDB(t)
	setStates(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing)
	mustExec(t, db, `INSERT INTO approvals (id, task_id, execution_id, operation, target, content_version, params_hash, status, created_at)
		VALUES ('approval-1', 'linear-1', 'linear-1-e1', 'merge_pull_request',
		'{"repository":"owner/repo","prNumber":12,"baseBranch":"main","headBranch":"evaloom/linear-1-test"}', ?, ?, 'pending', ?)`,
		strings.Repeat("a", 40), strings.Repeat("d", 64), testTime)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionPublishing, model.ExecutionWaitingApproval)
	transition(t, db, "linear-1", model.TaskWaitingApproval, model.ExecutionWaitingApproval, model.ExecutionPending)
	transition(t, db, "linear-1", model.TaskQueued, model.ExecutionPending, model.ExecutionWaitingApproval)
	rows := outboxRows(t, db)
	if len(rows) != 1 || rows[0].kind != "waiting_approval" || rows[0].dedupeKey != "linear-1:waiting_approval:linear-1-e1" {
		t.Fatalf("outbox rows = %+v; want one waiting_approval", rows)
	}
	for _, want := range []string{
		"- 実行ステータス: Waiting Approval\n",
		"- 必要な承認事項: merge_pull_request\n",
		"- 承認・却下は管理画面で本人が行う\n",
	} {
		if !strings.Contains(rows[0].body, want) {
			t.Errorf("body lacks %q:\n%s", want, rows[0].body)
		}
	}
}
