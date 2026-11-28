package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// Migration 0034 (spec 20 7章) keeps every existing task and audit row, adds
// program_clients, and ties source=program to program_client_id.
func TestProgramClientsMigrationPreservesRowsAndConstrainsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "program.db")
	db, err := open(context.Background(), path, migrationsBefore(t, 34))
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO audit_logs (actor, action, target_type, target_id, detail, created_at)
		VALUES ('runner:mac-mini', 'x', 'request', '/', '{}', 't')`)
	beforeTasks := migrationRows(t, db, `SELECT id, source, project_id, harness_id, title, body, acceptance_criteria,
		collaboration_mode, status, intake_block_cause, idempotency_key, created_at, updated_at FROM tasks ORDER BY id`)
	beforeAudit := migrationRows(t, db, `SELECT * FROM audit_logs ORDER BY id`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := migrationRows(t, db, `SELECT id, source, project_id, harness_id, title, body, acceptance_criteria,
		collaboration_mode, status, intake_block_cause, idempotency_key, created_at, updated_at FROM tasks ORDER BY id`); !reflect.DeepEqual(got, beforeTasks) {
		t.Fatalf("tasks changed: %v -> %v", beforeTasks, got)
	}
	if got := migrationRows(t, db, `SELECT * FROM audit_logs ORDER BY id`); !reflect.DeepEqual(got, beforeAudit) {
		t.Fatalf("audit_logs changed: %v -> %v", beforeAudit, got)
	}
	var executions int
	if err := db.QueryRow(`SELECT count(*) FROM executions WHERE task_id = 'task-001'`).Scan(&executions); err != nil || executions != 1 {
		t.Fatalf("executions=%d err=%v", executions, err)
	}

	mustTaskExec(t, db, `INSERT INTO program_clients (id, name, token_hash, harness_id, created_at)
		VALUES ('c1', 'c1', 'h1', 'codex-developer', 't')`)
	_, err = db.Exec(`INSERT INTO program_clients (id, name, token_hash, created_at) VALUES ('c2', 'c2', 'h2', 't')`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = db.Exec(`INSERT INTO program_clients (id, name, token_hash, project_id, harness_id, created_at)
		VALUES ('c3', 'c3', 'h3', 'project-a', 'codex-developer', 't')`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)

	insert := `INSERT INTO tasks (id, source, harness_id, title, body, acceptance_criteria, collaboration_mode,
		status, idempotency_key, created_at, updated_at, program_client_id)
		VALUES (?, ?, 'codex-developer', 't', 'b', '', 'solo', 'queued', ?, 't', 't', ?)`
	mustTaskExec(t, db, insert, "task-101", "program", "k101", "c1")
	_, err = db.Exec(insert, "task-102", "program", "k102", nil)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = db.Exec(insert, "task-103", "manual", "k103", "c1")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = db.Exec(insert, "task-104", "program", "k104", "unknown")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)

	audit := `INSERT INTO audit_logs (actor, action, target_type, target_id, detail, created_at) VALUES (?, 'x', 't', 'i', '{}', 't')`
	mustTaskExec(t, db, audit, "program:c1")
	for _, actor := range []string{"program:", "programs:c1", "someone"} {
		_, err = db.Exec(audit, actor)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
}
