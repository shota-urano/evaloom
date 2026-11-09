package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"modernc.org/sqlite/lib"
)

func insertApprovalRow(db *sql.DB, id, execution, operation, status string) (sql.Result, error) {
	return db.Exec(`INSERT INTO approvals (id, task_id, execution_id, operation, target,
		content_version, params_hash, status, created_at)
		VALUES (?, 'task-001', ?, ?, '{"repository":"owner/repo","prNumber":12}', ?, ?, ?, ?)`,
		id, execution, operation, strings.Repeat("c", 40), strings.Repeat("d", 64), status, registryTime)
}

// Spec 17 3.9: one Execution and operation can hold at most one pending /
// approved approval. Decided rows do not count, and other operations or
// Executions are independent.
func TestApprovalsAllowOneUnusedRowPerExecutionAndOperation(t *testing.T) {
	for _, first := range []string{"pending", "approved"} {
		for _, second := range []string{"pending", "approved"} {
			t.Run(first+"+"+second, func(t *testing.T) {
				db := openTaskTables(t)
				if _, err := insertApprovalRow(db, "approval-1", "execution-1", "merge_pull_request", first); err != nil {
					t.Fatal(err)
				}
				_, err := insertApprovalRow(db, "approval-2", "execution-1", "merge_pull_request", second)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				// Turning a decided row back into an unused one is refused too.
				if _, err := insertApprovalRow(db, "approval-3", "execution-1", "merge_pull_request", "rejected"); err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`UPDATE approvals SET status = ? WHERE id = 'approval-3'`, second)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				if got := migrationRows(t, db, `SELECT id, status FROM approvals ORDER BY id`); !reflect.DeepEqual(got,
					[][]any{{"approval-1", first}, {"approval-3", "rejected"}}) {
					t.Fatalf("approvals = %#v", got)
				}
			})
		}
	}
	t.Run("decided rows and other keys", func(t *testing.T) {
		db := openTaskTables(t)
		mustTaskExec(t, db, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
			VALUES ('execution-2', 'task-001', 2, 'continue', 'pending', ?)`, registryTime)
		for i, status := range []string{"rejected", "consumed", "invalidated", "expired"} {
			if _, err := insertApprovalRow(db, "decided-"+string(rune('a'+i)), "execution-1", "merge_pull_request", status); err != nil {
				t.Fatalf("decided %s: %v", status, err)
			}
		}
		for _, row := range []struct{ id, execution, operation string }{
			{"open-merge", "execution-1", "merge_pull_request"},
			{"open-deploy", "execution-1", "deploy_production"},
			{"open-other-execution", "execution-2", "merge_pull_request"},
		} {
			if _, err := insertApprovalRow(db, row.id, row.execution, row.operation, "pending"); err != nil {
				t.Fatalf("%s: %v", row.id, err)
			}
		}
		// Superseding (3.3): once the open row is invalidated, a new pending row fits.
		mustTaskExec(t, db, `UPDATE approvals SET status = 'invalidated', invalidated_reason = 'superseded' WHERE id = 'open-merge'`)
		if _, err := insertApprovalRow(db, "open-merge-2", "execution-1", "merge_pull_request", "pending"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestApprovalsConstrainEnumeratedColumns(t *testing.T) {
	db := openTaskTables(t)
	for _, operation := range []string{"merge_pull_request", "deploy_production", "send_external_email",
		"delete_important_file", "change_auth", "change_contract", "change_evaluation_criteria", "change_security_policy"} {
		if _, err := insertApprovalRow(db, "op-"+operation, "execution-1", operation, "rejected"); err != nil {
			t.Fatalf("operation %s: %v", operation, err)
		}
	}
	_, err := insertApprovalRow(db, "bad-op", "execution-1", "merge", "pending")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = insertApprovalRow(db, "bad-status", "execution-1", "merge_pull_request", "done")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = insertApprovalRow(db, "bad-execution", "missing", "merge_pull_request", "pending")
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	for _, statement := range []string{
		`UPDATE approvals SET invalidated_reason = 'other' WHERE id = 'op-merge_pull_request'`,
		`UPDATE approvals SET decided_by = 'runner:mac-mini' WHERE id = 'op-merge_pull_request'`,
		`UPDATE approvals SET target = 'not json' WHERE id = 'op-merge_pull_request'`,
		`UPDATE approvals SET result = 'not json' WHERE id = 'op-merge_pull_request'`,
		`UPDATE approvals SET use_count = 2 WHERE id = 'op-merge_pull_request'`,
		`UPDATE approvals SET max_uses = 0, use_count = 0 WHERE id = 'op-merge_pull_request'`,
	} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	if got := migrationRows(t, db, `SELECT max_uses, use_count FROM approvals WHERE id = 'op-merge_pull_request'`); !reflect.DeepEqual(got, [][]any{{int64(1), int64(0)}}) {
		t.Fatalf("defaults = %#v; want max_uses 1, use_count 0", got)
	}
	mustTaskExec(t, db, `UPDATE approvals SET status = 'consumed', use_count = 1, consumed_at = ?,
		decided_by = 'owner', result = '{"mergeCommitSha":"abc"}' WHERE id = 'op-merge_pull_request'`, registryTime)
}

// 0019 rebuilds executions to widen its status CHECK. Existing rows and the
// references from other tables survive unchanged.
func TestApprovalsMigrationPreservesExecutions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 19 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `UPDATE executions SET status = 'publishing', runner_id = 'mac-mini', worktree_id = 'worktree-1',
		branch_name = 'evaloom/task-001-change', pushed_sha = ?, pr_number = 12, pr_url = 'https://example.test/pr/12',
		resume_point = 'publish', cancel_requested_at = ? WHERE id = 'execution-1'`, strings.Repeat("e", 40), registryTime)
	mustTaskExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('execution-1', 'pr_created', '{}', ?)`, registryTime)
	_, err = db.Exec(`UPDATE executions SET status = 'waiting_approval' WHERE id = 'execution-1'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	before := migrationRows(t, db, `SELECT * FROM executions`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24})
	if after := migrationRows(t, db, `SELECT * FROM executions`); !reflect.DeepEqual(after, before) {
		t.Fatalf("executions changed:\nbefore=%#v\nafter=%#v", before, after)
	}
	for _, status := range []string{"waiting_approval", "merging"} {
		mustTaskExec(t, db, `UPDATE executions SET status = ? WHERE id = 'execution-1'`, status)
	}
	if got := migrationRows(t, db, `SELECT count(*) FROM execution_events WHERE execution_id = 'execution-1'`); !reflect.DeepEqual(got, [][]any{{int64(1)}}) {
		t.Fatalf("events = %#v", got)
	}
	// References to executions(id) are still enforced after the rebuild.
	_, err = db.Exec(`INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('missing', 'log', '{}', ?)`, registryTime)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	if _, err := insertApprovalRow(db, "approval-1", "execution-1", "merge_pull_request", "pending"); err != nil {
		t.Fatal(err)
	}
}
