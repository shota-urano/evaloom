package task_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/shota-urano/evaloom/backend/internal/testregistry"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

const testTime = "2026-10-25T00:00:00Z"

type persistedCase struct {
	from, to         model.ExecutionStatus
	fromTask, toTask model.TaskStatus
	stored, resume   model.Stage
}

func TestPersistedExecutionTransitions(t *testing.T) {
	db := lifecycleDB(t)
	// Every row of spec 02 5.2 must persist the task state from the mapping
	// table in the same transaction.
	cases := []persistedCase{
		{"pending", "claimed", "queued", "running", "", ""},
		{"pending", "cancelled", "queued", "cancelled", "", ""},
		{"pending", "failed", "queued", "failed", "", ""},
		{"pending", "blocked", "queued", "blocked", "publish", ""},
		{"pending", "waiting_approval", "queued", "waiting_approval", "publish", ""},
		{"claimed", "preparing", "running", "running", "", ""},
		{"claimed", "working", "running", "running", "", ""},
		{"claimed", "working", "running", "running", "work", ""},
		{"claimed", "publishing", "running", "running", "publish", ""},
		{"preparing", "working", "running", "running", "", ""},
		{"working", "checking", "running", "running", "", ""},
		{"working", "blocked", "running", "blocked", "", ""},
		{"checking", "publishing", "running", "running", "", ""},
		{"checking", "waiting_approval", "running", "waiting_approval", "", ""},
		{"checking", "blocked", "running", "blocked", "", ""},
		{"publishing", "waiting_approval", "running", "waiting_approval", "", ""},
		{"publishing", "pending", "running", "queued", "", "publish"},
		{"publishing", "blocked", "running", "blocked", "", ""},
		{"publishing", "succeeded", "running", "completed", "", ""},
		{"waiting_approval", "pending", "waiting_approval", "queued", "", "publish"},
		{"waiting_approval", "failed", "waiting_approval", "failed", "", ""},
		{"waiting_approval", "cancelled", "waiting_approval", "cancelled", "", ""},
		{"blocked", "waiting_approval", "blocked", "waiting_approval", "", ""},
		{"blocked", "pending", "blocked", "queued", "", "work"},
		{"blocked", "pending", "blocked", "queued", "work", "publish"},
		{"blocked", "cancelled", "blocked", "cancelled", "", ""},
		{"blocked", "failed", "blocked", "failed", "", ""},
	}
	for _, from := range []model.ExecutionStatus{"claimed", "preparing", "working", "checking", "publishing"} {
		for _, to := range []struct {
			execution model.ExecutionStatus
			task      model.TaskStatus
		}{
			{"failed", "failed"}, {"interrupted", "failed"}, {"timed_out", "timed_out"}, {"cancelled", "cancelled"},
		} {
			cases = append(cases, persistedCase{from, to.execution, "running", to.task, "", ""})
		}
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to)+"/"+string(tc.stored)+"/"+string(tc.resume), func(t *testing.T) {
			setStates(t, db, tc.fromTask, tc.from, tc.stored)
			if err := task.TransitionExecution(context.Background(), db, task.Transition{
				TaskID: "task-1", ExecutionID: "execution-1", FromTask: tc.fromTask,
				FromExecution: tc.from, ToExecution: tc.to, ResumeStage: tc.resume,
			}); err != nil {
				t.Fatal(err)
			}
			assertStates(t, db, tc.toTask, tc.to)
			var updated string
			if err := db.QueryRow("SELECT updated_at FROM tasks WHERE id = 'task-1'").Scan(&updated); err != nil || updated == testTime {
				t.Fatalf("task updated_at = %q, err = %v; want refreshed timestamp", updated, err)
			}
			want := tc.stored
			if tc.to == "pending" {
				want = tc.resume
			}
			var resume sql.NullString
			if err := db.QueryRow("SELECT resume_stage FROM executions WHERE id = 'execution-1'").Scan(&resume); err != nil || model.Stage(resume.String) != want {
				t.Fatalf("resume_stage = %v, err = %v; want %q", resume, err, want)
			}
		})
	}
}

func TestTransitionRejectsInvalidOrStaleRequestsWithoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name            string
		storedTask      model.TaskStatus
		storedExecution model.ExecutionStatus
		storedResume    model.Stage
		change          task.Transition
	}{
		{"completed cancel", "completed", "succeeded", "", task.Transition{FromTask: "completed", FromExecution: "succeeded", ToExecution: "cancelled"}},
		{"terminal reopen", "failed", "failed", "", task.Transition{FromTask: "failed", FromExecution: "failed", ToExecution: "pending", ResumeStage: "work"}},
		{"skip stages", "running", "working", "", task.Transition{FromTask: "running", FromExecution: "working", ToExecution: "succeeded"}},
		{"skip claim", "queued", "pending", "", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "working"}},
		{"skip work", "running", "claimed", "", task.Transition{FromTask: "running", FromExecution: "claimed", ToExecution: "checking"}},
		{"duplicate report", "running", "working", "", task.Transition{FromTask: "running", FromExecution: "working", ToExecution: "working"}},
		{"stale execution", "running", "claimed", "", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "cancelled"}},
		{"stale task", "blocked", "pending", "", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "claimed"}},
		{"inconsistent expectations", "queued", "pending", "", task.Transition{FromTask: "running", FromExecution: "pending", ToExecution: "claimed"}},
		{"unknown target", "queued", "pending", "", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "unknown"}},
		{"unknown source", "queued", "pending", "", task.Transition{FromTask: "queued", FromExecution: "unknown", ToExecution: "claimed"}},
		{"obsolete task", "routing", "pending", "", task.Transition{FromTask: "planning", FromExecution: "pending", ToExecution: "claimed"}},
		{"publish without resume", "running", "claimed", "", task.Transition{FromTask: "running", FromExecution: "claimed", ToExecution: "publishing"}},
		{"prepare on work resume", "running", "claimed", "work", task.Transition{FromTask: "running", FromExecution: "claimed", ToExecution: "preparing"}},
		{"work on publish resume", "running", "claimed", "publish", task.Transition{FromTask: "running", FromExecution: "claimed", ToExecution: "working"}},
		{"resume without stage", "blocked", "blocked", "", task.Transition{FromTask: "blocked", FromExecution: "blocked", ToExecution: "pending"}},
		{"approval resumes work", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "pending", ResumeStage: "work"}},
		{"waived resumes work", "running", "publishing", "", task.Transition{FromTask: "running", FromExecution: "publishing", ToExecution: "pending", ResumeStage: "work"}},
		{"resume on non-pending", "running", "working", "", task.Transition{FromTask: "running", FromExecution: "working", ToExecution: "checking", ResumeStage: "work"}},
		{"claimed blocked", "running", "claimed", "", task.Transition{FromTask: "running", FromExecution: "claimed", ToExecution: "blocked"}},
		{"preparing blocked", "running", "preparing", "", task.Transition{FromTask: "running", FromExecution: "preparing", ToExecution: "blocked"}},
		{"approval succeeds", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "succeeded"}},
		{"merge without approval", "running", "publishing", "", task.Transition{FromTask: "running", FromExecution: "publishing", ToExecution: "merging"}},
		{"waiting reclaimed", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "claimed"}},
		{"waiting blocked", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "blocked"}},
		{"waiting interrupted", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "interrupted"}},
		{"blocked interrupted", "blocked", "blocked", "", task.Transition{FromTask: "blocked", FromExecution: "blocked", ToExecution: "interrupted"}},
		{"waiting claims without resume", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "working"}},
		{"waiting duplicate", "waiting_approval", "waiting_approval", "", task.Transition{FromTask: "waiting_approval", FromExecution: "waiting_approval", ToExecution: "waiting_approval"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := lifecycleDB(t)
			setStates(t, db, tc.storedTask, tc.storedExecution, tc.storedResume)
			tc.change.TaskID, tc.change.ExecutionID = "task-1", "execution-1"
			assertConflict(t, task.TransitionExecution(context.Background(), db, tc.change))
			assertStates(t, db, tc.storedTask, tc.storedExecution)
			var resume sql.NullString
			if err := db.QueryRow("SELECT resume_stage FROM executions WHERE id = 'execution-1'").Scan(&resume); err != nil || model.Stage(resume.String) != tc.storedResume {
				t.Fatalf("resume_stage = %v, err = %v; want unchanged %q", resume, err, tc.storedResume)
			}
		})
	}
}

func TestTransitionRejectsMissingWrongTaskAndHistoricalExecution(t *testing.T) {
	for _, name := range []string{"missing execution", "missing task", "wrong task", "historical"} {
		t.Run(name, func(t *testing.T) {
			db := lifecycleDB(t)
			change := task.Transition{TaskID: "task-1", ExecutionID: "execution-1", FromTask: "queued", FromExecution: "pending", ToExecution: "claimed"}
			switch name {
			case "missing execution":
				change.ExecutionID = "missing"
			case "missing task":
				change.TaskID = "missing"
			case "wrong task":
				mustExec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria,
					collaboration_mode, status, idempotency_key, created_at, updated_at)
					SELECT 'task-2', project_id, title, body, acceptance_criteria,
					collaboration_mode, status, 'key-2', created_at, updated_at FROM tasks`)
				change.TaskID = "task-2"
			case "historical":
				mustExec(t, db, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
					VALUES ('execution-2', 'task-1', 2, 'continue', 'pending', ?)`, testTime)
			}
			assertConflict(t, task.TransitionExecution(context.Background(), db, change))
			assertStates(t, db, "queued", "pending")
		})
	}
}

func TestTransitionRollsBackWhenTaskWriteFails(t *testing.T) {
	db := lifecycleDB(t)
	mustExec(t, db, `CREATE TRIGGER fail_task_update BEFORE UPDATE OF status ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected task write failure'); END`)
	err := task.TransitionExecution(context.Background(), db, task.Transition{
		TaskID: "task-1", ExecutionID: "execution-1", FromTask: "queued", FromExecution: "pending", ToExecution: "claimed",
	})
	if err == nil || !strings.Contains(err.Error(), "injected task write failure") {
		t.Fatalf("error = %v; want injected database failure", err)
	}
	assertStates(t, db, "queued", "pending")
}

func TestTransitionTxCommitAndRollbackAreAtomicToObservers(t *testing.T) {
	db := lifecycleDB(t)
	var path string
	var seq int
	var name string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	observer, err := testregistry.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close() })
	for _, commit := range []bool{false, true} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := task.TransitionExecutionTx(context.Background(), tx, task.Transition{
			TaskID: "task-1", ExecutionID: "execution-1", FromTask: "queued", FromExecution: "pending", ToExecution: "claimed",
		}); err != nil {
			t.Fatal(err)
		}
		assertStates(t, tx, "running", "claimed")
		assertStates(t, observer, "queued", "pending")
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			assertStates(t, observer, "running", "claimed")
		} else {
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			assertStates(t, observer, "queued", "pending")
		}
	}
}

func TestConcurrentClaimAndCancelHaveOneWinner(t *testing.T) {
	db := lifecycleDB(t)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, target := range []model.ExecutionStatus{"claimed", "cancelled"} {
		wg.Go(func() {
			<-start
			errs <- task.TransitionExecution(context.Background(), db, task.Transition{
				TaskID: "task-1", ExecutionID: "execution-1", FromTask: "queued", FromExecution: "pending", ToExecution: target,
			})
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	var success, conflicts int
	for err := range errs {
		if err == nil {
			success++
		} else {
			assertConflict(t, err)
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d; want one each", success, conflicts)
	}
	var execution model.ExecutionStatus
	if err := db.QueryRow("SELECT status FROM executions WHERE id = 'execution-1'").Scan(&execution); err != nil {
		t.Fatal(err)
	}
	if execution == "claimed" {
		assertStates(t, db, "running", "claimed")
	} else {
		assertStates(t, db, "cancelled", "cancelled")
	}
}

func TestQueueNewExecutionForCreationAndRetry(t *testing.T) {
	for _, tc := range []struct {
		from     model.TaskStatus
		previous model.ExecutionStatus
	}{
		{"received", ""}, {"failed", "failed"}, {"failed", "interrupted"},
		{"timed_out", "timed_out"}, {"cancelled", "cancelled"}, {"completed", "succeeded"},
	} {
		t.Run(string(tc.from)+"/"+string(tc.previous), func(t *testing.T) {
			db := lifecycleDB(t)
			initial := tc.previous == ""
			if initial {
				setStates(t, db, tc.from, "pending", "")
			} else {
				setStates(t, db, tc.from, tc.previous, "")
			}
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			id := "execution-1"
			if !initial {
				id = "execution-2"
				mustExec(t, tx, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
					VALUES (?, 'task-1', 2, 'fresh', 'pending', ?)`, id, testTime)
			}
			if err := task.QueueExecutionTx(context.Background(), tx, "task-1", id, tc.from); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var status string
			var count int
			if err := db.QueryRow(`SELECT t.status, count(e.id) FROM tasks t JOIN executions e ON e.task_id = t.id WHERE t.id = 'task-1' GROUP BY t.id`).Scan(&status, &count); err != nil {
				t.Fatal(err)
			}
			wantCount := 2
			if initial {
				wantCount = 1
			}
			if status != "queued" || count != wantCount {
				t.Fatalf("task=%s executions=%d; want queued/%d", status, count, wantCount)
			}
			if initial {
				assertStates(t, db, "queued", "pending")
			} else {
				assertStates(t, db, "queued", tc.previous)
			}
			var pending string
			if err := db.QueryRow("SELECT status FROM executions WHERE id = ?", id).Scan(&pending); err != nil || pending != "pending" {
				t.Fatalf("new execution=%s err=%v", pending, err)
			}
		})
	}
}

func TestQueueRejectsInvalidStatesAndExecutions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  model.TaskStatus
		setup string
		id    string
	}{
		{"running retry", "running", "", "execution-1"},
		// Q131: blocked and waiting_approval resume the same Execution.
		{"blocked retry", "blocked", `INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('execution-2', 'task-1', 2, 'continue', 'pending', 'now')`, "execution-2"},
		{"waiting retry", "waiting_approval", `INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('execution-2', 'task-1', 2, 'continue', 'pending', 'now')`, "execution-2"},
		{"queued retry", "queued", "", "execution-1"},
		{"reserved phase", "routing", "", "execution-1"},
		{"missing execution", "received", "", "missing"},
		{"not pending", "received", "UPDATE executions SET status = 'claimed'", "execution-1"},
		{"no preceding execution", "failed", "", "execution-1"},
		{"stale task expectation", "received", "", "execution-1"},
		{"noninitial sequence", "received", "UPDATE executions SET seq = 2", "execution-1"},
		{"previous mismatch", "failed", `INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('execution-2', 'task-1', 2, 'continue', 'pending', 'now')`, "execution-2"},
		{"historical", "received", `INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('execution-2', 'task-1', 2, 'continue', 'pending', 'now')`, "execution-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := lifecycleDB(t)
			storedTask := tc.from
			if tc.name == "stale task expectation" {
				storedTask = "queued"
			}
			setStates(t, db, storedTask, "pending", "")
			if tc.setup != "" {
				mustExec(t, db, tc.setup)
			}
			var storedExecution model.ExecutionStatus
			if err := db.QueryRow("SELECT status FROM executions WHERE id = 'execution-1'").Scan(&storedExecution); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			assertConflict(t, task.QueueExecutionTx(context.Background(), tx, "task-1", tc.id, tc.from))
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			assertStates(t, db, storedTask, storedExecution)
		})
	}
}

func TestQueueInsertAndTaskUpdateRollBackTogether(t *testing.T) {
	db := lifecycleDB(t)
	setStates(t, db, "failed", "failed", "")
	mustExec(t, db, `CREATE TRIGGER fail_queue BEFORE UPDATE OF status ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mustExec(t, tx, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('execution-2', 'task-1', 2, 'continue', 'pending', ?)`, testTime)
	err = task.QueueExecutionTx(context.Background(), tx, "task-1", "execution-2", "failed")
	if err == nil || !strings.Contains(err.Error(), "injected queue failure") {
		t.Fatalf("error = %v; want database failure", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertStates(t, db, "failed", "failed")
	var count int
	if err := db.QueryRow("SELECT count(*) FROM executions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("executions = %d, err = %v; want 1", count, err)
	}
}

func TestCancelledContextIsNotAConflict(t *testing.T) {
	db := lifecycleDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := task.TransitionExecution(ctx, db, task.Transition{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	assertStates(t, db, "queued", "pending")
}

func lifecycleDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := testregistry.Open(context.Background(), filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `INSERT INTO runners (id, name, token_hash, status, last_heartbeat_at, created_at) VALUES ('runner', 'Runner', 'hash', 'online', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO projects (id, name, runner_id, created_at, updated_at, harness_id, profile_id) VALUES ('project', 'Project', 'runner', ?, ?, 'fixture-harness', 'fixture-profile')`, testTime, testTime)
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES ('task-1', 'project', 'Title', 'Body', 'tests pass', 'solo', 'queued', 'key-1', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('execution-1', 'task-1', 1, 'continue', 'pending', ?)`, testTime)
	return db
}

func setStates(t *testing.T, db *sql.DB, taskStatus model.TaskStatus, execution model.ExecutionStatus, resume model.Stage) {
	t.Helper()
	mustExec(t, db, "UPDATE tasks SET status = ?, updated_at = ? WHERE id = 'task-1'", taskStatus, testTime)
	mustExec(t, db, "UPDATE executions SET status = ?, resume_stage = NULLIF(?, '') WHERE id = 'execution-1'", execution, resume)
}

type rowQuerier interface{ QueryRow(string, ...any) *sql.Row }

func assertStates(t *testing.T, db rowQuerier, wantTask model.TaskStatus, wantExecution model.ExecutionStatus) {
	t.Helper()
	var taskStatus model.TaskStatus
	var execution model.ExecutionStatus
	err := db.QueryRow(`SELECT t.status, e.status FROM tasks t JOIN executions e ON e.task_id = t.id
		WHERE t.id = 'task-1' AND e.id = 'execution-1'`).Scan(&taskStatus, &execution)
	if err != nil || taskStatus != wantTask || execution != wantExecution {
		t.Fatalf("states = %q/%q, err = %v; want %q/%q", taskStatus, execution, err, wantTask, wantExecution)
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

func TestTaskOnlyTransitionsMatchSpec04Section32(t *testing.T) {
	// Rows of 3.2 without an Execution: intake routing/hold, hold release to
	// routing, and cancellation before any Execution exists.
	allowed := map[model.TaskStatus][]model.TaskStatus{
		"received": {"routing", "blocked", "cancelled"},
		"routing":  {"cancelled"},
		"blocked":  {"routing", "cancelled"},
	}
	for _, from := range taskStates {
		for _, to := range taskStates {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				db := taskOnlyDB(t, from, "missing_acceptance_criteria")
				var cause model.IntakeBlockCause
				if to == "blocked" {
					cause = "config_missing_profile"
				}
				tx, err := db.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				err = task.TransitionTaskTx(context.Background(), tx, task.TaskTransition{TaskID: "task-1", From: from, To: to, IntakeBlockCause: cause})
				if !contains(allowed[from], to) {
					assertConflict(t, err)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				var status model.TaskStatus
				var stored sql.NullString
				if err := db.QueryRow("SELECT status, intake_block_cause FROM tasks WHERE id = 'task-1'").Scan(&status, &stored); err != nil {
					t.Fatal(err)
				}
				if status != to || model.IntakeBlockCause(stored.String) != cause {
					t.Fatalf("task = %q/%v; want %q/%q", status, stored, to, cause)
				}
			})
		}
	}
}

func TestTaskOnlyTransitionRejectsWithoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored model.TaskStatus
		cause  model.IntakeBlockCause
		setup  string
		change task.TaskTransition
	}{
		{"hold without cause", "received", "", "", task.TaskTransition{From: "received", To: "blocked"}},
		{"hold with unknown cause", "received", "", "", task.TaskTransition{From: "received", To: "blocked", IntakeBlockCause: "routing_no_label"}},
		{"cause on routing", "received", "", "", task.TaskTransition{From: "received", To: "routing", IntakeBlockCause: "config_missing_profile"}},
		{"stale task", "routing", "", "", task.TaskTransition{From: "received", To: "cancelled"}},
		{"decision blocked", "blocked", "", `INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('execution-1', 'task-1', 1, 'continue', 'blocked', 'now')`, task.TaskTransition{From: "blocked", To: "cancelled"}},
		{"blocked without cause", "blocked", "", "", task.TaskTransition{From: "blocked", To: "routing"}},
		{"received with execution", "received", "", `INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('execution-1', 'task-1', 1, 'continue', 'pending', 'now')`, task.TaskTransition{From: "received", To: "cancelled"}},
		{"queue without execution", "received", "", "", task.TaskTransition{From: "received", To: "queued"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := taskOnlyDB(t, tc.stored, tc.cause)
			if tc.setup != "" {
				mustExec(t, db, tc.setup)
			}
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			tc.change.TaskID = "task-1"
			assertConflict(t, task.TransitionTaskTx(context.Background(), tx, tc.change))
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			var status model.TaskStatus
			var updated string
			if err := db.QueryRow("SELECT status, updated_at FROM tasks WHERE id = 'task-1'").Scan(&status, &updated); err != nil || status != tc.stored || updated != testTime {
				t.Fatalf("task = %q at %q, err = %v; want unchanged %q", status, updated, err, tc.stored)
			}
		})
	}
}

// taskOnlyDB holds task-1 without Executions. A blocked task gets cause.
func taskOnlyDB(t *testing.T, status model.TaskStatus, cause model.IntakeBlockCause) *sql.DB {
	t.Helper()
	db := lifecycleDB(t)
	mustExec(t, db, "DELETE FROM executions")
	if status != "blocked" {
		cause = ""
	}
	if status.Validate() != nil {
		status = "received" // obsolete values cannot be stored; the expectation is stale
	}
	mustExec(t, db, "UPDATE tasks SET status = ?, intake_block_cause = NULLIF(?, ''), updated_at = ? WHERE id = 'task-1'", status, cause, testTime)
	return db
}
