package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"modernc.org/sqlite/lib"
)

func TestTaskMigrationUpgradesRegistryAndPersistsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	registrySQL, err := migrationFiles.ReadFile("migrations/0001_registry.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := open(context.Background(), path, fstest.MapFS{
		"0001_registry.sql": {Data: registrySQL},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openRegistry(t, path)
	seedTaskTables(t, db)
	// Before claim, all execution metadata is absent, rather than placeholder
	// values that could be mistaken for pinned metadata after a restart.
	var optionalCount int
	err = db.QueryRow(`SELECT count(*) FROM executions WHERE id = 'execution-1'
		AND runner_id IS NULL AND role IS NULL AND harness_id IS NULL
		AND profile_id IS NULL AND profile_version IS NULL AND profile_commit_sha IS NULL
		AND worktree_id IS NULL AND branch_name IS NULL AND base_sha IS NULL
		AND gate1_base_sha IS NULL AND resume_point IS NULL AND harness_head_sha IS NULL
		AND verified_sha IS NULL AND pushed_sha IS NULL AND pr_number IS NULL
		AND pr_url IS NULL AND harness_session_id IS NULL AND error_class IS NULL
		AND error_message IS NULL AND cancel_requested_at IS NULL AND claimed_at IS NULL
		AND started_at IS NULL AND finished_at IS NULL AND last_heartbeat_at IS NULL`).Scan(&optionalCount)
	if err != nil || optionalCount != 1 {
		t.Fatalf("unclaimed execution with NULL metadata = %d, err = %v; want 1", optionalCount, err)
	}
	sha := strings.Repeat("a", 40)
	copied := `[{"path":"config.json","sha256":"` + strings.Repeat("b", 64) + `"}]`
	mustTaskExec(t, db, `UPDATE worktrees SET copied_files = ? WHERE id = 'worktree-1'`, copied)
	mustTaskExec(t, db, `UPDATE executions SET runner_id = 'mac-mini', role = 'developer',
		harness_id = 'codex-developer', profile_id = 'profile-1', profile_version = '1.2.0',
		profile_commit_sha = ?, worktree_id = 'worktree-1', branch_name = 'evaloom/task-001-change',
		base_sha = ?, gate1_base_sha = ?, resume_point = 'publish', harness_head_sha = ?,
		verified_sha = ?, pushed_sha = ?, pr_number = 12, pr_url = 'https://example.invalid/pull/12',
		harness_session_id = 'session-1', error_class = 'publish_failed', error_message = 'push failed',
		cancel_requested_at = ?, claimed_at = ?, started_at = ?, finished_at = ?, last_heartbeat_at = ?
		WHERE id = 'execution-1'`, sha, sha, sha, sha, sha, sha,
		registryTime, registryTime, registryTime, registryTime, registryTime)
	mustTaskExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('execution-1', 'context_pinned', ?, ?)`, `{"revisions":[]}`, registryTime)
	for _, layer := range []string{"global", "harness", "project", "task"} {
		mustTaskExec(t, db, `INSERT INTO context_revisions
			(id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at)
			VALUES (?, 'execution-1', ?, 'agents_md', 'sub/AGENTS.md', ?, ?, ?, ?)`,
			"revision-"+layer, layer, sha, strings.Repeat("c", 64), "instructions for "+layer, registryTime)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openRegistry(t, path)
	var applied int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations
		WHERE version = 2 AND name = '0002_tasks.sql'`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("task migration history entries = %d, err = %v; want 1", applied, err)
	}
	var project, criteria, version, commit, pathValue, files, payload, content string
	var pr int
	err = db.QueryRow(`SELECT p.id, t.acceptance_criteria, e.profile_version, e.profile_commit_sha,
		w.path, w.copied_files, e.pr_number, ev.payload, cr.content
		FROM tasks t JOIN projects p ON p.id = t.project_id
		JOIN executions e ON e.task_id = t.id JOIN worktrees w ON w.id = e.worktree_id
		JOIN execution_events ev ON ev.execution_id = e.id
		JOIN context_revisions cr ON cr.execution_id = e.id
		WHERE e.id = 'execution-1' AND cr.layer = 'project'`).Scan(
		&project, &criteria, &version, &commit, &pathValue, &files, &pr, &payload, &content)
	if err != nil {
		t.Fatal(err)
	}
	if project != "project-a" || criteria != "tests pass" || version != "1.2.0" || commit != sha ||
		pathValue != "/worktrees/task-001" || files != copied || pr != 12 ||
		payload != `{"revisions":[]}` || content != "instructions for project" {
		t.Fatalf("unexpected persisted task data: %q %q %q %q %q %q %d %q %q",
			project, criteria, version, commit, pathValue, files, pr, payload, content)
	}
	var revisions int
	if err := db.QueryRow("SELECT count(*) FROM context_revisions WHERE execution_id = 'execution-1'").Scan(&revisions); err != nil || revisions != 4 {
		t.Fatalf("persisted context revisions = %d, err = %v; want 4", revisions, err)
	}
	var cleanedAt sql.NullString
	if err := db.QueryRow("SELECT cleaned_at FROM worktrees WHERE id = 'worktree-1'").Scan(&cleanedAt); err != nil || cleanedAt.Valid {
		t.Fatalf("active worktree cleaned_at = %v, err = %v; want NULL", cleanedAt, err)
	}
}

func TestTaskIdempotencyKeyIsGloballyUnique(t *testing.T) {
	db := openTaskTables(t)
	_, err := insertTask(db, "task-002", "key-1", "received")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	insertRegistryProject(t, db, "project-b", "/repos/project-b")
	// The same key must also be refused for a different project.
	_, err = db.Exec(`INSERT INTO tasks
		(id, project_id, title, body, acceptance_criteria, short_name, task_type,
		collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES ('task-003', 'project-b', 'Title', 'Body', 'tests pass', 'change',
		'development', 'solo', 'received', 'key-1', ?, ?)`, registryTime, registryTime)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	if _, err := insertTask(db, "task-002", "key-2", "received"); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE tasks SET idempotency_key = 'key-1' WHERE id = 'task-002'")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	_, err = insertTask(db, "task-003", nil, "received")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_NOTNULL)
	var count int
	if err := db.QueryRow("SELECT count(*) FROM tasks").Scan(&count); err != nil || count != 2 {
		t.Fatalf("tasks = %d, err = %v; want 2", count, err)
	}
}

func TestExecutionSequenceIsUniqueWithinTask(t *testing.T) {
	db := openTaskTables(t)
	_, err := insertExecution(db, "duplicate", "task-001", 1, "pending")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	if _, err := insertExecution(db, "execution-2", "task-001", 2, "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTask(db, "task-002", "key-2", "received"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertExecution(db, "other-task", "task-002", 1, "pending"); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE executions SET seq = 1 WHERE id = 'execution-2'")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	_, err = db.Exec("UPDATE executions SET task_id = 'task-001' WHERE id = 'other-task'")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	for _, seq := range []int{0, -1} {
		_, err = insertExecution(db, "invalid", "task-001", seq, "pending")
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM executions").Scan(&count); err != nil || count != 3 {
		t.Fatalf("executions = %d, err = %v; want 3", count, err)
	}
}

func TestTaskAndExecutionStatusConstraints(t *testing.T) {
	for _, test := range []struct {
		table, id string
		allowed   []string
		invalid   []string
		insert    func(*sql.DB, string, any) (sql.Result, error)
	}{
		{
			table: "tasks", id: "task-001",
			allowed: []string{"received", "planning", "queued", "running", "verifying",
				"waiting_approval", "completed", "failed", "cancelled", "timed_out", "blocked"},
			invalid: []string{"unknown", "", "Running", "succeeded", "pending", "completed "},
			insert: func(db *sql.DB, id string, status any) (sql.Result, error) {
				return insertTask(db, id, id, status)
			},
		},
		{
			table: "executions", id: "execution-1",
			allowed: []string{"pending", "claimed", "preparing", "running", "verifying", "publishing",
				"succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out"},
			invalid: []string{"unknown", "", "Running", "completed", "queued", "succeeded "},
			insert: func(db *sql.DB, id string, status any) (sql.Result, error) {
				// Each accepted row gets a separate task so sequence uniqueness
				// cannot hide a missing status constraint.
				if _, err := insertTask(db, id, id, "queued"); err != nil {
					return nil, err
				}
				return insertExecution(db, id, id, 1, status)
			},
		},
	} {
		t.Run(test.table, func(t *testing.T) {
			db := openTaskTables(t)
			update := "UPDATE " + test.table + " SET status = ? WHERE id = ?"
			for i, status := range test.allowed {
				if _, err := test.insert(db, fmt.Sprintf("valid-%d", i), status); err != nil {
					t.Fatalf("insert status %q: %v", status, err)
				}
				mustTaskExec(t, db, update, status, test.id)
			}
			for i, status := range test.invalid {
				_, err := test.insert(db, fmt.Sprintf("invalid-%d", i), status)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
				_, err = db.Exec(update, status, test.id)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
			}
			_, err := test.insert(db, "null-status", nil)
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_NOTNULL)
			_, err = db.Exec(update, nil, test.id)
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_NOTNULL)
			var status string
			if err := db.QueryRow("SELECT status FROM "+test.table+" WHERE id = ?", test.id).Scan(&status); err != nil || status != test.allowed[len(test.allowed)-1] {
				t.Fatalf("status after rejected updates = %q, err = %v", status, err)
			}
		})
	}
}

func TestTaskTablesRejectInvalidReferences(t *testing.T) {
	db := openTaskTables(t)
	mustTaskExec(t, db, `UPDATE executions SET runner_id = 'mac-mini',
		harness_id = 'codex-developer', profile_id = 'profile-1', worktree_id = 'worktree-1'`)
	mustTaskExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('execution-1', 'log', '{}', ?)`, registryTime)
	mustTaskExec(t, db, `INSERT INTO context_revisions
		(id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at)
		VALUES ('revision-1', 'execution-1', 'project', 'agents_md', 'AGENTS.md', ?, ?, 'instructions', ?)`,
		strings.Repeat("a", 40), strings.Repeat("b", 64), registryTime)
	for _, test := range []struct{ table, column string }{
		{"tasks", "project_id"},
		{"executions", "task_id"}, {"executions", "runner_id"},
		{"executions", "harness_id"}, {"executions", "profile_id"}, {"executions", "worktree_id"},
		{"worktrees", "project_id"}, {"worktrees", "task_id"}, {"worktrees", "runner_id"},
		{"execution_events", "execution_id"}, {"context_revisions", "execution_id"},
	} {
		t.Run(test.table+"/"+test.column, func(t *testing.T) {
			_, err := db.Exec("UPDATE " + test.table + " SET " + test.column + " = 'missing'")
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
		})
	}
	for _, table := range []string{"tasks", "executions", "worktrees"} {
		_, err := db.Exec("DELETE FROM " + table)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
}

func TestTaskTablesConstrainOtherEnumeratedColumns(t *testing.T) {
	db := openTaskTables(t)
	mustTaskExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('execution-1', 'log', '{}', ?)`, registryTime)
	mustTaskExec(t, db, `INSERT INTO context_revisions
		(id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at)
		VALUES ('revision-1', 'execution-1', 'task', 'task_input', '', '', ?, 'task input', ?)`,
		strings.Repeat("b", 64), registryTime)
	for _, test := range []struct {
		table, column string
		allowed       []string
	}{
		{"tasks", "task_type", []string{"development"}},
		{"tasks", "collaboration_mode", []string{"solo"}},
		{"executions", "mode", []string{"continue", "fresh"}},
		{"executions", "resume_point", []string{"gate2", "gate3", "publish"}},
		{"executions", "error_class", []string{"config_error", "execution_failed", "test_failed", "timeout",
			"cancelled", "interrupted", "publish_failed", "sandbox_error"}},
		{"worktrees", "status", []string{"active", "retained", "cleaned"}},
		{"worktrees", "setup_status", []string{"pending", "succeeded", "failed"}},
		{"execution_events", "type", []string{"status_changed", "log", "worktree_ready", "context_pinned",
			"harness_started", "harness_finished", "gate_result", "blocked", "pushed", "pr_created", "notification_sent", "notification_failed"}},
		{"context_revisions", "layer", []string{"global", "harness", "project", "task"}},
	} {
		t.Run(test.table+"/"+test.column, func(t *testing.T) {
			statement := "UPDATE " + test.table + " SET " + test.column + " = ?"
			for _, value := range test.allowed {
				mustTaskExec(t, db, statement, value)
			}
			_, err := db.Exec(statement, "unknown")
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
		})
	}
	_, err := db.Exec("UPDATE tasks SET acceptance_criteria = ''")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	// kind is an open set in the specification, unlike layer.
	mustTaskExec(t, db, "UPDATE context_revisions SET kind = 'custom_context'")
}

func TestExecutionEventIDsIncreaseAcrossExecutionsAndDeletion(t *testing.T) {
	db := openTaskTables(t)
	if _, err := insertExecution(db, "execution-2", "task-001", 2, "pending"); err != nil {
		t.Fatal(err)
	}
	var previous int64
	for _, execution := range []string{"execution-1", "execution-2", "execution-1"} {
		result := mustTaskExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
			VALUES (?, 'log', '{"text":"hello"}', ?)`, execution, registryTime)
		id, err := result.LastInsertId()
		if err != nil || id <= previous {
			t.Fatalf("event id = %d, err = %v; want greater than %d", id, err, previous)
		}
		previous = id
		mustTaskExec(t, db, "DELETE FROM execution_events WHERE id = ?", id)
	}
}

func openTaskTables(t *testing.T) *sql.DB {
	t.Helper()
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	seedTaskTables(t, db)
	return db
}

func seedTaskTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := insertTask(db, "task-001", "key-1", "queued"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertExecution(db, "execution-1", "task-001", 1, "pending"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `INSERT INTO worktrees
		(id, project_id, task_id, runner_id, path, branch_name, base_sha, status,
		setup_status, copied_files, created_at)
		VALUES ('worktree-1', 'project-a', 'task-001', 'mac-mini', '/worktrees/task-001',
		'evaloom/task-001-change', ?, 'active', 'pending', '[]', ?)`, strings.Repeat("a", 40), registryTime)
}

func insertTask(db *sql.DB, id string, key, status any) (sql.Result, error) {
	return db.Exec(`INSERT INTO tasks
		(id, project_id, title, body, acceptance_criteria, short_name, task_type,
		collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES (?, 'project-a', 'Title', 'Body', 'tests pass', 'change',
		'development', 'solo', ?, ?, ?, ?)`, id, status, key, registryTime, registryTime)
}

func insertExecution(db *sql.DB, id, task string, seq int, status any) (sql.Result, error) {
	return db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES (?, ?, ?, 'continue', ?, ?)`, id, task, seq, status, registryTime)
}

func mustTaskExec(t *testing.T, db *sql.DB, statement string, args ...any) sql.Result {
	t.Helper()
	result, err := db.Exec(statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
