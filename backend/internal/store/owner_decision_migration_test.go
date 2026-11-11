package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func insertDecision(db *sql.DB, id, kind, status string, cause any) error {
	var execution, subject any
	if kind != "route" {
		execution = "execution-1"
	}
	if kind == "route" || kind == "approval" {
		subject = "subject-" + id
	}
	_, err := db.Exec(`INSERT INTO owner_decisions
		(id,kind,task_id,execution_id,subject_id,cause,detail,status,created_at)
		VALUES (?,?,'task-001',?,?,?,'{}',?,'t')`, id, kind, execution, subject, cause, status)
	return err
}

func insertGenericApproval(db *sql.DB, id, execution, operation, status string) error {
	_, err := db.Exec(`INSERT INTO approvals
		(id,task_id,execution_id,decision_id,requested_stage,operation,target,content_id,params_hash,status,created_at)
		VALUES (?,'task-001',?,'decision-approval','check',?,'{}','opaque','hash',?,'t')`, id, execution, operation, status)
	return err
}

func TestOwnerDecisionColumnsMatchSpecification(t *testing.T) {
	db := openTaskTables(t)
	for table, want := range map[string]string{
		"owner_decisions": "id,kind,task_id,execution_id,subject_id,cause,basis,detail,status,withdrawn_reason,answer,answered_at,created_at",
		"route_proposals": "id,task_id,trigger,seen_labels,candidates,status,runner_id,proposed_label,reason,error_class,decision_id,created_at,finished_at",
		"approvals":       "id,task_id,execution_id,decision_id,requested_stage,operation,target,summary,content_id,params_hash,status,invalidated_reason,decided_at,expires_at,max_uses,use_count,consumed_at,created_at",
	} {
		var got string
		if err := db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_table_info(?) ORDER BY cid)`, table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s columns=%s want=%s err=%v", table, got, want, err)
		}
	}
}

func TestOwnerDecisionsAllowOneOpenNonApprovalPerTask(t *testing.T) {
	for _, first := range []string{"route", "blocked", "question"} {
		for _, second := range []string{"route", "blocked", "question"} {
			t.Run(first+"/"+second, func(t *testing.T) {
				db := openTaskTables(t)
				causes := map[string]any{"route": nil, "blocked": "check_blocked", "question": "question"}
				if err := insertDecision(db, "first", first, "open", causes[first]); err != nil {
					t.Fatal(err)
				}
				assertRegistryConstraint(t, insertDecision(db, "second", second, "open", causes[second]), sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				if err := insertDecision(db, "second", second, "answered", causes[second]); err != nil {
					t.Fatal(err)
				}
				_, err := db.Exec(`UPDATE owner_decisions SET status='open' WHERE id='second'`)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				mustTaskExec(t, db, `UPDATE owner_decisions SET status='withdrawn',withdrawn_reason='superseded' WHERE id='first'`)
				mustTaskExec(t, db, `UPDATE owner_decisions SET status='open' WHERE id='second'`)
				if _, err := insertTask(db, "task-002", "key-2", "routing"); err != nil {
					t.Fatal(err)
				}
				mustTaskExec(t, db, `INSERT INTO owner_decisions (id,kind,task_id,subject_id,detail,status,created_at)
					VALUES ('other-task','route','task-002','proposal','{}','open','t')`)
				for _, id := range []string{"approval-a", "approval-b"} {
					if err := insertDecision(db, id, "approval", "open", nil); err != nil {
						t.Fatal(err)
					}
				}
				_, err = db.Exec(`UPDATE owner_decisions SET subject_id='subject-approval-a' WHERE id='approval-b'`)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				_, err = db.Exec(`INSERT INTO owner_decisions SELECT 'duplicate',kind,task_id,execution_id,subject_id,cause,basis,detail,status,withdrawn_reason,answer,answered_at,created_at FROM owner_decisions WHERE id='approval-a'`)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
			})
		}
	}
}

func TestOwnerDecisionKindCausePairsFollowSection56(t *testing.T) {
	db := openTaskTables(t)
	for _, kind := range []string{"route", "approval", "blocked", "question", "unknown", ""} {
		for _, cause := range []any{nil, "check_blocked", "publish_unknown", "context_conflict", "workspace_missing", "question", "unknown", ""} {
			valid := (kind == "route" || kind == "approval") && cause == nil ||
				kind == "question" && cause == "question" ||
				kind == "blocked" && (cause == "check_blocked" || cause == "publish_unknown" || cause == "context_conflict" || cause == "workspace_missing")
			err := insertDecision(db, "decision", kind, "answered", cause)
			if valid {
				if err != nil {
					t.Fatalf("kind=%s cause=%v: %v", kind, cause, err)
				}
				mustTaskExec(t, db, `DELETE FROM owner_decisions`)
			} else {
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
			}
		}
	}
	if err := insertDecision(db, "decision", "question", "open", "question"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE owner_decisions SET kind='obsolete'`,
		`UPDATE owner_decisions SET cause=NULL`,
		`UPDATE owner_decisions SET cause='other'`,
		`UPDATE owner_decisions SET status='closed'`,
		`UPDATE owner_decisions SET withdrawn_reason='other'`,
		`UPDATE owner_decisions SET subject_id='not-null'`,
		`UPDATE owner_decisions SET execution_id=NULL`,
	} {
		_, err := db.Exec(query)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
}

func TestGenericApprovalsAllowOneUnusedRowPerExecutionOperation(t *testing.T) {
	for _, first := range []string{"pending", "approved"} {
		for _, second := range []string{"pending", "approved"} {
			t.Run(first+"/"+second, func(t *testing.T) {
				db := openTaskTables(t)
				if err := insertDecision(db, "decision-approval", "approval", "open", nil); err != nil {
					t.Fatal(err)
				}
				if err := insertGenericApproval(db, "first", "execution-1", "custom-operation", first); err != nil {
					t.Fatal(err)
				}
				assertRegistryConstraint(t, insertGenericApproval(db, "second", "execution-1", "custom-operation", second), sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				for _, status := range []string{"rejected", "consumed", "invalidated", "expired"} {
					if err := insertGenericApproval(db, status, "execution-1", "custom-operation", status); err != nil {
						t.Fatal(err)
					}
					_, err := db.Exec(`UPDATE approvals SET status=? WHERE id=?`, second, status)
					assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
				}
				mustTaskExec(t, db, `INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES ('execution-2','task-001',2,'continue','pending','t')`)
				for _, key := range []struct{ id, execution, operation string }{
					{"other-operation", "execution-1", "another-operation"},
					{"other-execution", "execution-2", "custom-operation"},
				} {
					if err := insertGenericApproval(db, key.id, key.execution, key.operation, "pending"); err != nil {
						t.Fatal(err)
					}
				}
				mustTaskExec(t, db, `UPDATE approvals SET status='invalidated',invalidated_reason='superseded' WHERE id='first'`)
				if err := insertGenericApproval(db, "replacement", "execution-1", "custom-operation", "pending"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestOwnerDecisionTablesEnumsAndForeignKeys(t *testing.T) {
	db := openTaskTables(t)
	if err := insertDecision(db, "decision-approval", "approval", "open", nil); err != nil {
		t.Fatal(err)
	}
	if err := insertGenericApproval(db, "approval", "execution-1", "custom-operation", "pending"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `INSERT INTO route_proposals (id,task_id,trigger,seen_labels,candidates,status,created_at)
		VALUES ('proposal','task-001','no_label','[]','[]','queued','t')`)
	for _, tc := range []struct {
		table, column string
		values        []string
	}{
		{"owner_decisions", "status", []string{"open", "answered", "withdrawn"}},
		{"owner_decisions", "withdrawn_reason", []string{"cancelled", "superseded", "expired"}},
		{"route_proposals", "trigger", []string{"no_label", "unknown_label", "multiple_labels"}},
		{"route_proposals", "status", []string{"queued", "running", "proposed", "failed", "decided", "superseded", "cancelled"}},
		{"route_proposals", "error_class", []string{"sandbox_error", "timeout", "contract_error", "interrupted"}},
		{"approvals", "requested_stage", []string{"check", "publish"}},
		{"approvals", "status", []string{"pending", "approved", "rejected", "consumed", "invalidated", "expired"}},
		{"approvals", "invalidated_reason", []string{"content_changed", "superseded", "cancelled"}},
	} {
		for _, value := range tc.values {
			mustTaskExec(t, db, "UPDATE "+tc.table+" SET "+tc.column+"=?", value)
		}
		_, err := db.Exec("UPDATE " + tc.table + " SET " + tc.column + "='obsolete'")
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	for table, columns := range map[string][]string{
		"owner_decisions": {"task_id", "execution_id"},
		"route_proposals": {"task_id", "runner_id", "decision_id"},
		"approvals":       {"task_id", "execution_id", "decision_id"},
	} {
		for _, column := range columns {
			_, err := db.Exec("UPDATE " + table + " SET " + column + "='missing'")
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
		}
	}
	for _, query := range []string{
		`UPDATE approvals SET max_uses=2`, `UPDATE approvals SET max_uses=0`,
		`UPDATE approvals SET use_count=-1`, `UPDATE approvals SET use_count=2`,
	} {
		_, err := db.Exec(query)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	mustTaskExec(t, db, `UPDATE route_proposals SET reason=?`, strings.Repeat("あ", 2000))
	_, err := db.Exec(`UPDATE route_proposals SET reason=?`, strings.Repeat("あ", 2001))
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	if got := migrationRows(t, db, `SELECT max_uses,use_count FROM approvals`); !reflect.DeepEqual(got, [][]any{{int64(1), int64(0)}}) {
		t.Fatal(got)
	}
	if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestOwnerDecisionMigrationRejectsLegacyApprovalsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openBefore(t, path, 28)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	if _, err := insertApprovalRow(db, "legacy", "execution-1", "merge_pull_request", "approved"); err != nil {
		t.Fatal(err)
	}
	before := migrationRows(t, db, `SELECT * FROM approvals`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	failed, err := Open(context.Background(), path)
	if failed != nil {
		failed.Close()
		t.Fatal("failed migration returned a DB")
	}
	if err == nil || !strings.Contains(err.Error(), "0028_owner_decisions.sql") {
		t.Fatal(err)
	}
	db = openBefore(t, path, 28)
	if got := migrationRows(t, db, `SELECT * FROM approvals`); !reflect.DeepEqual(before, got) {
		t.Fatal("legacy approval changed", got)
	}
	for _, table := range []string{"owner_decisions", "route_proposals", "approvals_new"} {
		if tableExists(t, db, table) {
			t.Fatal("partial schema survived", table)
		}
	}
	if got := migrationRows(t, db, `SELECT count(*) FROM schema_migrations WHERE version=28`); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
		t.Fatal(got)
	}
}

func TestOwnerDecisionMigrationReopensWithoutChangingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.db")
	db := openBefore(t, path, 28)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	if err := insertDecision(db, "decision-approval", "approval", "open", nil); err != nil {
		t.Fatal(err)
	}
	if err := insertGenericApproval(db, "approval", "execution-1", "custom-operation", "pending"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `INSERT INTO route_proposals (id,task_id,trigger,seen_labels,candidates,status,decision_id,created_at)
		VALUES ('proposal','task-001','no_label','[]','[]','queued','decision-approval','t')`)
	before := map[string][][]any{}
	for _, table := range []string{"tasks", "executions", "owner_decisions", "route_proposals", "approvals"} {
		before[table] = migrationRows(t, db, "SELECT * FROM "+table)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	for table, want := range before {
		if got := migrationRows(t, db, "SELECT * FROM "+table); !reflect.DeepEqual(got, want) {
			t.Fatal(table, got, want)
		}
	}
	if got := migrationRows(t, db, `SELECT count(*) FROM schema_migrations WHERE version=28`); !reflect.DeepEqual(got, [][]any{{int64(1)}}) {
		t.Fatal(got)
	}
}
