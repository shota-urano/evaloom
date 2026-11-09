package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestLinearOutboxStateMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, m := range migrations {
		if m.version < 22 {
			previous[m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	for _, status := range []string{"pending", "sent", "failed"} {
		mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key,
			body, status, attempts, next_attempt_at, linear_comment_id, created_at, sent_at,
			execution_id, outcome_unknown, failure_reason, failed_at)
			VALUES (?, 'task-001', 'issue-a', 'succeeded', ?, 'comment body', ?, 2, ?, 'comment-id', ?, ?,
			'execution-1', 1, 'other', ?)`, status, status, status, registryTime, registryTime, registryTime, registryTime)
	}
	query := `SELECT id, task_id, linear_issue_id, kind, dedupe_key, body, status, attempts, next_attempt_at,
		linear_comment_id, created_at, sent_at, execution_id, outcome_unknown, failure_reason, failed_at
		FROM linear_outbox ORDER BY id`
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen twice to exercise migration and restart without reapplying it.
	for range 2 {
		db = openRegistry(t, path)
		if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
			t.Fatalf("migration changed existing rows: got %#v, want %#v", got, before)
		}
		want := [][]any{{"failed", "comment", nil}, {"pending", "comment", nil}, {"sent", "comment", nil}}
		if got := migrationRows(t, db, `SELECT id, action, state_name FROM linear_outbox ORDER BY id`); !reflect.DeepEqual(got, want) {
			t.Fatalf("migrated actions = %#v, want %#v", got, want)
		}
		if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
			t.Fatalf("foreign key violations: %#v", got)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	db = openRegistry(t, path)
	// The old insert shape still defaults to a comment.
	mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body, status, created_at)
		VALUES ('default', 'task-001', 'issue-a', 'blocked', 'default', 'body', 'pending', ?)`, registryTime)
	if got := migrationRows(t, db, `SELECT action, state_name FROM linear_outbox WHERE id = 'default'`); !reflect.DeepEqual(got, [][]any{{"comment", nil}}) {
		t.Fatalf("default action = %#v", got)
	}
	for _, kind := range []string{"started", "waiting_approval", "completed", "cancelled"} {
		mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, action, state_name,
			dedupe_key, status, created_at) VALUES (?, 'task-001', 'issue-a', ?, 'set_state', 'Done', ?, 'skipped', ?)`,
			kind, kind, kind, registryTime)
	}
	for _, statement := range []string{
		`UPDATE linear_outbox SET action = 'invalid' WHERE id = 'default'`,
		`UPDATE linear_outbox SET status = 'invalid' WHERE id = 'default'`,
		`UPDATE linear_outbox SET body = NULL WHERE id = 'default'`,
		`UPDATE linear_outbox SET state_name = 'Done' WHERE id = 'default'`,
		`UPDATE linear_outbox SET outcome_unknown = 1 WHERE id = 'started'`,
		`UPDATE linear_outbox SET body = 'body' WHERE id = 'started'`,
		`UPDATE linear_outbox SET state_name = NULL WHERE id = 'started'`,
		`UPDATE linear_outbox SET linear_comment_id = 'comment' WHERE id = 'started'`,
	} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	_, err = db.Exec(`UPDATE linear_outbox SET execution_id = 'missing' WHERE id = 'started'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`UPDATE linear_outbox SET task_id = 'missing' WHERE id = 'started'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`UPDATE linear_outbox SET dedupe_key = 'pending' WHERE id = 'started'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}

func TestLinearStateNotFoundMigrationPreservesAllColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state-reason.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, m := range migrations {
		if m.version < 23 {
			previous[m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	for _, status := range []string{"pending", "sent", "failed", "skipped"} {
		mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key,
			body, status, attempts, next_attempt_at, linear_comment_id, created_at, sent_at,
			execution_id, outcome_unknown, failure_reason, failed_at)
			VALUES (?, 'task-001', 'issue-a', 'succeeded', ?, 'body', ?, 2, ?, 'comment-id', ?, ?,
			'execution-1', 1, 'other', ?)`, "comment-"+status, "comment-"+status, status,
			registryTime, registryTime, registryTime, registryTime)
		mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, action, state_name,
			dedupe_key, status, attempts, next_attempt_at, created_at, sent_at, execution_id, failure_reason, failed_at)
			VALUES (?, 'task-001', 'issue-a', 'waiting_approval', 'set_state', 'PR Review', ?, ?,
			3, ?, ?, ?, 'execution-1', 'auth', ?)`, "state-"+status, "state-"+status, status,
			registryTime, registryTime, registryTime, registryTime)
	}
	const query = `SELECT * FROM linear_outbox ORDER BY id`
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db = openRegistry(t, path)
		if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
			t.Fatalf("rows changed: got %#v, want %#v", got, before)
		}
		if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
			t.Fatalf("foreign keys=%v", got)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	db = openRegistry(t, path)
	defer db.Close()
	mustTaskExec(t, db, `UPDATE linear_outbox SET failure_reason = 'state_not_found' WHERE id = 'state-failed'`)
	_, err = db.Exec(`UPDATE linear_outbox SET failure_reason = 'invalid' WHERE id = 'state-failed'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
}
