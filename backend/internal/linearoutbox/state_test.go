package linearoutbox_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

type stateRow struct {
	taskID, issueID, kind, name, key, status string
	executionID                              sql.NullString
}

func stateRows(t *testing.T, db *sql.DB) []stateRow {
	t.Helper()
	rows, err := db.Query(`SELECT task_id, linear_issue_id, kind, state_name, dedupe_key, status, execution_id,
		body, linear_comment_id, outcome_unknown, attempts FROM linear_outbox
		WHERE action = 'set_state' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []stateRow
	for rows.Next() {
		var r stateRow
		var body, comment sql.NullString
		var unknown, attempts int
		if err := rows.Scan(&r.taskID, &r.issueID, &r.kind, &r.name, &r.key, &r.status, &r.executionID,
			&body, &comment, &unknown, &attempts); err != nil {
			t.Fatal(err)
		}
		if body.Valid || comment.Valid || unknown != 0 || attempts != 0 {
			t.Fatalf("invalid state row: %+v body=%v comment=%v unknown=%d attempts=%d", r, body, comment, unknown, attempts)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func wantState(t *testing.T, db *sql.DB, kind, name, executionID string) {
	t.Helper()
	target := executionID
	if target == "" {
		target = "linear-1"
	}
	want := []stateRow{{"linear-1", "issue-1", kind, name, "linear-1:state_" + kind + ":" + target,
		"pending", sql.NullString{String: executionID, Valid: executionID != ""}}}
	if got := stateRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("state rows = %+v; want %+v", got, want)
	}
}

func TestStateStartedOncePerExecution(t *testing.T) {
	db := outboxDB(t)
	transition(t, db, "linear-1", model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed)
	wantState(t, db, "started", "In Progress", "linear-1-e1")
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionClaimed, model.ExecutionPreparing)
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionPreparing, model.ExecutionRunning)
	wantState(t, db, "started", "In Progress", "linear-1-e1")
	// Resume reuses the same Execution. A retried Execution has a new key.
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionRunning, model.ExecutionBlocked)
	transition(t, db, "linear-1", model.TaskBlocked, model.ExecutionBlocked, model.ExecutionPending)
	transition(t, db, "linear-1", model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed)
	wantState(t, db, "started", "In Progress", "linear-1-e1")
	transition(t, db, "linear-1", model.TaskRunning, model.ExecutionClaimed, model.ExecutionFailed)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mustExec(t, tx, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('linear-1-e2', 'linear-1', 2, 'continue', 'pending', ?)`, testTime)
	if err := task.QueueExecutionTx(context.Background(), tx, "linear-1", "linear-1-e2", model.TaskFailed); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := task.TransitionExecution(context.Background(), db, task.Transition{TaskID: "linear-1", ExecutionID: "linear-1-e2",
		FromTask: model.TaskQueued, FromExecution: model.ExecutionPending, ToExecution: model.ExecutionClaimed}); err != nil {
		t.Fatal(err)
	}
	got := stateRows(t, db)
	if len(got) != 2 || got[0].status != "skipped" || got[1].key != "linear-1:state_started:linear-1-e2" || got[1].status != "pending" {
		t.Fatalf("retry state rows = %+v", got)
	}
}

func TestStateTransitionMapping(t *testing.T) {
	for _, tc := range []struct {
		fromTask   model.TaskStatus
		from, to   model.ExecutionStatus
		kind, name string
	}{
		{model.TaskRunning, model.ExecutionPreparing, model.ExecutionRunning, "started", "In Progress"},
		{model.TaskVerifying, model.ExecutionPublishing, model.ExecutionWaitingApproval, "waiting_approval", "PR Review"},
		{model.TaskVerifying, model.ExecutionPublishing, model.ExecutionSucceeded, "completed", "Done"},
		{model.TaskWaitingApproval, model.ExecutionMerging, model.ExecutionSucceeded, "completed", "Done"},
		{model.TaskWaitingApproval, model.ExecutionWaitingApproval, model.ExecutionSucceeded, "completed", "Done"},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionFailed, "", ""},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionInterrupted, "", ""},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionTimedOut, "", ""},
		{model.TaskRunning, model.ExecutionRunning, model.ExecutionBlocked, "", ""},
	} {
		for _, source := range []string{"linear", "manual"} {
			t.Run(source+"/"+string(tc.from)+"/"+string(tc.to), func(t *testing.T) {
				db := outboxDB(t)
				setStates(t, db, source+"-1", tc.fromTask, tc.from)
				transition(t, db, source+"-1", tc.fromTask, tc.from, tc.to)
				if source == "manual" || tc.kind == "" {
					if got := stateRows(t, db); len(got) != 0 {
						t.Fatalf("unexpected state rows: %+v", got)
					}
				} else {
					wantState(t, db, tc.kind, tc.name, "linear-1-e1")
				}
			})
		}
	}
}

func cancel(t *testing.T, db *sql.DB, c store.TaskCancellation) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := store.CancelTaskTx(context.Background(), tx, c); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStateCancellationRequester(t *testing.T) {
	for _, phase := range []string{"queued", "blocked", "waiting_approval", "running", "verifying", "merging", "held"} {
		for _, reason := range []string{"", "linear_label_removed", "linear_issue_cancelled", "linear_issue_removed", "linear_issue_moved"} {
			for _, source := range []string{"linear", "manual"} {
				if phase == "held" && source == "manual" {
					continue
				}
				t.Run(source+"/"+phase+"/"+reason, func(t *testing.T) {
					db := outboxDB(t)
					id, execStatus := source+"-1", model.ExecutionStatus(phase)
					if phase == "held" {
						mustExec(t, db, `DELETE FROM executions WHERE task_id = ?`, id)
						mustExec(t, db, `UPDATE tasks SET status = 'blocked', intake_block_cause = 'routing_no_label' WHERE id = ?`, id)
					} else {
						if phase == "queued" {
							execStatus = model.ExecutionPending
						}
						ts, err := task.TaskStatusForExecution(execStatus)
						if err != nil {
							t.Fatal(err)
						}
						setStates(t, db, id, ts, execStatus)
					}
					actor := "owner"
					if reason != "" {
						actor = "system"
					}
					cancel(t, db, store.TaskCancellation{TaskID: id, Actor: actor, Reason: reason})
					if phase == "running" || phase == "verifying" || phase == "merging" {
						if got := stateRows(t, db); len(got) != 0 {
							t.Fatalf("cancel request changed Issue before transition: %+v", got)
						}
						// Duplicate requests and a recreated caller keep the requester
						// in the durable audit; the Runner eventually reports the stop.
						cancel(t, db, store.TaskCancellation{TaskID: id, Actor: actor, Reason: reason})
						ts, _ := task.TaskStatusForExecution(execStatus)
						transition(t, db, id, ts, execStatus, model.ExecutionCancelled)
					}
					if source == "manual" || reason != "" {
						if got := stateRows(t, db); len(got) != 0 {
							t.Fatalf("unexpected state rows: %+v", got)
						}
					} else {
						execID := id + "-e1"
						if phase == "held" {
							execID = ""
						}
						wantState(t, db, "cancelled", "Canceled", execID)
					}
					// Cancellation still writes its comment, including Linear causes.
					comments := outboxRows(t, db)
					if source == "linear" && (len(comments) != 1 || comments[0].kind != "cancelled") {
						t.Fatalf("cancellation comments = %+v", comments)
					}
				})
			}
		}
	}
}

func TestStateSupersedesOnlyPendingStatesOnNewInsert(t *testing.T) {
	db := outboxDB(t)
	// A different Linear task on the same Issue keeps its pending state.
	mustExec(t, db, `UPDATE tasks SET source = 'linear' WHERE id = 'manual-1'`)
	mustExec(t, db, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id,
		task_id, attach_seq, created_at) VALUES ('link-2', 'issue-1', 'ENG-1', 'https://linear.app/x/ENG-1',
		'team-a', 'manual-1', 2, ?)`, testTime)
	transition(t, db, "manual-1", model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed)
	transition(t, db, "linear-1", model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed)
	// A retry schedule does not protect an obsolete state from supersession.
	mustExec(t, db, `UPDATE linear_outbox SET next_attempt_at = '2099-01-01T00:00:00Z'`)
	setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing)
	transition(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing, model.ExecutionWaitingApproval)
	for _, status := range []string{"sent", "failed", "skipped"} {
		mustExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, action, kind, state_name,
			dedupe_key, status, created_at) VALUES (?, 'linear-1', 'issue-1', 'set_state', 'started', 'In Progress', ?, ?, ?)`,
			status, status, status, testTime)
	}
	// Replaying an older start must leave PR Review pending.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := linearoutbox.EnqueueStateTx(context.Background(), tx, linearoutbox.Trigger{TaskID: "linear-1",
		Kind: linearoutbox.KindStarted, TargetID: "linear-1-e1", ExecutionID: "linear-1-e1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM linear_outbox WHERE kind = 'waiting_approval' AND action = 'set_state'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("PR Review = %q, err=%v", status, err)
	}
	transition(t, db, "linear-1", model.TaskWaitingApproval, model.ExecutionWaitingApproval, model.ExecutionSucceeded)
	for _, r := range stateRows(t, db) {
		want := "skipped"
		if r.kind == "completed" || r.taskID == "manual-1" {
			want = "pending"
		} else if r.key == "sent" || r.key == "failed" {
			want = r.key
		}
		if r.status != want {
			t.Errorf("row %+v; want status %s", r, want)
		}
	}
	for _, r := range outboxRows(t, db) {
		if r.status != "pending" {
			t.Errorf("comment changed: %+v", r)
		}
	}
}

func TestStateEnqueueRollsBackWithTransition(t *testing.T) {
	db := outboxDB(t)
	transition(t, db, "linear-1", model.TaskQueued, model.ExecutionPending, model.ExecutionClaimed)
	setStates(t, db, "linear-1", model.TaskVerifying, model.ExecutionPublishing)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := task.TransitionExecutionTx(context.Background(), tx, task.Transition{TaskID: "linear-1",
		ExecutionID: "linear-1-e1", FromTask: model.TaskVerifying, FromExecution: model.ExecutionPublishing,
		ToExecution: model.ExecutionWaitingApproval}); err != nil {
		t.Fatal(err)
	}
	var pending, skipped int
	if err := tx.QueryRow(`SELECT count(*) FROM linear_outbox WHERE action = 'set_state' AND status = 'pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(`SELECT count(*) FROM linear_outbox WHERE action = 'set_state' AND status = 'skipped'`).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || skipped != 1 {
		t.Fatalf("inside transaction: pending=%d skipped=%d", pending, skipped)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	wantState(t, db, "started", "In Progress", "linear-1-e1")
	var status string
	if err := db.QueryRow(`SELECT status FROM tasks WHERE id = 'linear-1'`).Scan(&status); err != nil || status != "verifying" {
		t.Fatalf("task = %q, err=%v", status, err)
	}
	if comments := outboxRows(t, db); len(comments) != 0 {
		t.Fatalf("rolled back comments = %+v", comments)
	}
}
