package task_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

const testTime = "2026-10-25T00:00:00Z"

func TestPersistedExecutionTransitions(t *testing.T) {
	db := lifecycleDB(t)
	// All normal substeps, all failure paths from active states, and both
	// blocked exits must persist the task state from the mapping table.
	cases := []struct {
		from, to         model.ExecutionStatus
		fromTask, toTask model.TaskStatus
		resume           string
	}{
		{"pending", "claimed", "queued", "running", ""},
		{"pending", "cancelled", "queued", "cancelled", ""},
		{"claimed", "preparing", "running", "running", ""},
		{"preparing", "running", "running", "running", ""},
		{"running", "verifying", "running", "verifying", ""},
		{"verifying", "publishing", "verifying", "verifying", ""},
		{"publishing", "succeeded", "verifying", "completed", ""},
		{"blocked", "pending", "blocked", "queued", "gate2"},
		{"blocked", "cancelled", "blocked", "cancelled", ""},
		{"claimed", "verifying", "running", "verifying", "gate2"},
		{"claimed", "verifying", "running", "verifying", "gate3"},
		{"claimed", "publishing", "running", "verifying", "publish"},
		{"preparing", "verifying", "running", "verifying", "gate2"},
		{"preparing", "publishing", "running", "verifying", "publish"},
	}
	for _, from := range []struct {
		execution model.ExecutionStatus
		task      model.TaskStatus
	}{
		{"claimed", "running"}, {"preparing", "running"}, {"running", "running"},
		{"verifying", "verifying"}, {"publishing", "verifying"},
	} {
		for _, to := range []struct {
			execution model.ExecutionStatus
			task      model.TaskStatus
		}{
			{"failed", "failed"}, {"interrupted", "failed"}, {"timed_out", "timed_out"},
			{"cancelled", "cancelled"}, {"blocked", "blocked"},
		} {
			cases = append(cases, struct {
				from, to         model.ExecutionStatus
				fromTask, toTask model.TaskStatus
				resume           string
			}{from.execution, to.execution, from.task, to.task, ""})
		}
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to)+"/"+tc.resume, func(t *testing.T) {
			setStates(t, db, tc.fromTask, tc.from, tc.resume)
			if err := task.TransitionExecution(context.Background(), db, task.Transition{
				TaskID: "task-1", ExecutionID: "execution-1", FromTask: tc.fromTask,
				FromExecution: tc.from, ToExecution: tc.to,
			}); err != nil {
				t.Fatal(err)
			}
			assertStates(t, db, tc.toTask, tc.to)
			var updated string
			if err := db.QueryRow("SELECT updated_at FROM tasks WHERE id = 'task-1'").Scan(&updated); err != nil || updated == testTime {
				t.Fatalf("task updated_at = %q, err = %v; want refreshed timestamp", updated, err)
			}
		})
	}
}

func TestTransitionRejectsInvalidOrStaleRequestsWithoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name            string
		storedTask      model.TaskStatus
		storedExecution model.ExecutionStatus
		change          task.Transition
	}{
		{"completed cancel", "completed", "succeeded", task.Transition{FromTask: "completed", FromExecution: "succeeded", ToExecution: "cancelled"}},
		{"terminal reopen", "failed", "failed", task.Transition{FromTask: "failed", FromExecution: "failed", ToExecution: "pending"}},
		{"skip gates", "running", "running", task.Transition{FromTask: "running", FromExecution: "running", ToExecution: "succeeded"}},
		{"skip claim", "queued", "pending", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "running"}},
		{"unapproved resume", "running", "claimed", task.Transition{FromTask: "running", FromExecution: "claimed", ToExecution: "verifying"}},
		{"duplicate report", "running", "running", task.Transition{FromTask: "running", FromExecution: "running", ToExecution: "running"}},
		{"stale execution", "running", "claimed", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "cancelled"}},
		{"stale task", "blocked", "pending", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "claimed"}},
		{"inconsistent expectations", "queued", "pending", task.Transition{FromTask: "running", FromExecution: "pending", ToExecution: "claimed"}},
		{"unknown target", "queued", "pending", task.Transition{FromTask: "queued", FromExecution: "pending", ToExecution: "unknown"}},
		{"unknown source", "queued", "pending", task.Transition{FromTask: "queued", FromExecution: "unknown", ToExecution: "claimed"}},
		{"reserved task", "planning", "pending", task.Transition{FromTask: "planning", FromExecution: "pending", ToExecution: "claimed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := lifecycleDB(t)
			setStates(t, db, tc.storedTask, tc.storedExecution, "")
			tc.change.TaskID, tc.change.ExecutionID = "task-1", "execution-1"
			assertConflict(t, task.TransitionExecution(context.Background(), db, tc.change))
			assertStates(t, db, tc.storedTask, tc.storedExecution)
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
				mustExec(t, db, `INSERT INTO tasks SELECT 'task-2', project_id, title, body, acceptance_criteria,
					short_name, task_type, collaboration_mode, status, 'key-2', created_at, updated_at FROM tasks`)
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
	observer, err := store.Open(context.Background(), path)
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
		{"received", ""}, {"blocked", "blocked"}, {"failed", "failed"}, {"failed", "interrupted"},
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
		{"queued retry", "queued", "", "execution-1"},
		{"reserved phase", "planning", "", "execution-1"},
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
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('runner', 'Runner', 'hash', '/worktrees', 'online', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO projects (id, name, local_repo_path, git_remote_url, runner_id, base_branch,
		default_role, context_location, copy_allowlist, allowed_operations, approval_policy, timeouts, created_at, updated_at)
		VALUES ('project', 'Project', '/repos/project', 'https://example.test/repo.git', 'runner', 'main',
		'developer', '.', '[]', '[]', '{}', '{}', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name,
		task_type, collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES ('task-1', 'project', 'Title', 'Body', 'tests pass', 'test', 'development', 'solo', 'queued', 'key-1', ?, ?)`, testTime, testTime)
	mustExec(t, db, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('execution-1', 'task-1', 1, 'continue', 'pending', ?)`, testTime)
	return db
}

func setStates(t *testing.T, db *sql.DB, taskStatus model.TaskStatus, execution model.ExecutionStatus, resume string) {
	t.Helper()
	mustExec(t, db, "UPDATE tasks SET status = ?, updated_at = ? WHERE id = 'task-1'", taskStatus, testTime)
	mustExec(t, db, "UPDATE executions SET status = ?, resume_point = nullif(?, '') WHERE id = 'execution-1'", execution, resume)
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
