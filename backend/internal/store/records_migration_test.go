package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestRecordTableColumnsMatchSpecification(t *testing.T) {
	db := openTaskTables(t)
	for table, want := range map[string]string{
		"artifacts":       "id,execution_id,route_proposal_id,stage,kind,name,storage_path,sha256,size_bytes,redacted,created_at",
		"harness_records": "id,harness_id,artifact_id,execution_id,task_id,source_identifier,file_name,created_at,deleted_at",
		"audit_logs":      "id,actor,action,target_type,target_id,detail,created_at",
		"usage_records":   "id,execution_id,stage,input_tokens,output_tokens,wall_time_ms,cost_amount,created_at",
	} {
		var got string
		if err := db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_table_info(?) ORDER BY cid)`, table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s columns=%s want=%s err=%v", table, got, want, err)
		}
	}
}

func TestRecordsForeignKeysOwnersAndStages(t *testing.T) {
	db := openTaskTables(t)
	seedVerificationRecords(t, db)
	mustTaskExec(t, db, `INSERT INTO route_proposals (id,task_id,trigger,seen_labels,candidates,status,created_at)
		VALUES ('proposal','task-001','no_label','[]','[]','queued','t')`)
	mustTaskExec(t, db, `INSERT INTO harness_records VALUES
		('record','codex-developer','artifact-1','execution-1','task-001',NULL,'record.md','t',NULL)`)
	for _, query := range []string{
		`UPDATE artifacts SET execution_id=NULL`,
		`UPDATE artifacts SET route_proposal_id='proposal'`,
	} {
		_, err := db.Exec(query)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	mustTaskExec(t, db, `UPDATE artifacts SET execution_id=NULL,route_proposal_id='proposal',stage='proposal',kind='proposal_result'`)
	for table, columns := range map[string][]string{
		"artifacts":       {"route_proposal_id"},
		"harness_records": {"harness_id", "artifact_id", "execution_id", "task_id"},
	} {
		for _, column := range columns {
			_, err := db.Exec("UPDATE " + table + " SET " + column + "='missing'")
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
		}
	}
	for table, stages := range map[string][]string{
		"artifacts":     {"prepare", "work", "check", "publish", "runner", "proposal"},
		"usage_records": {"prepare", "work", "check", "publish"},
	} {
		for _, stage := range stages {
			mustTaskExec(t, db, "UPDATE "+table+" SET stage=?", stage)
		}
		_, err := db.Exec("UPDATE " + table + " SET stage='unknown'")
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
		_, err = db.Exec("UPDATE " + table + " SET stage=NULL")
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_NOTNULL)
	}
	for _, stage := range []string{"runner", "proposal"} {
		_, err := db.Exec(`UPDATE usage_records SET stage=?`, stage)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
}

func TestRecordsMigrationRejectsUnlabelledLegacyDataAtomically(t *testing.T) {
	for _, table := range []string{"artifacts", "usage_records"} {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db := openBefore(t, path, 29)
			seedRegistry(t, db)
			seedTaskTables(t, db)
			if table == "artifacts" {
				mustTaskExec(t, db, `INSERT INTO artifacts VALUES ('old','execution-1','harness_log','artifacts/original','hash',1,1,'t')`)
			} else {
				mustTaskExec(t, db, `INSERT INTO usage_records VALUES ('old','execution-1',NULL,NULL,10,NULL,'t')`)
			}
			before := migrationRows(t, db, "SELECT * FROM "+table)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			failed, err := Open(context.Background(), path)
			if failed != nil {
				failed.Close()
				t.Fatal("failed migration returned a DB")
			}
			if err == nil || !strings.Contains(err.Error(), "0029_records.sql") {
				t.Fatal(err)
			}
			db = openBefore(t, path, 29)
			if got := migrationRows(t, db, "SELECT * FROM "+table); !reflect.DeepEqual(got, before) {
				t.Fatal("legacy rows changed", got, before)
			}
			for _, name := range []string{"artifacts_new", "usage_records_new", "harness_records"} {
				if tableExists(t, db, name) {
					t.Fatal("partial migration survived", name)
				}
			}
			if got := migrationRows(t, db, `SELECT count(*) FROM schema_migrations WHERE version=29`); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
				t.Fatal(got)
			}
		})
	}
}

func TestRecordsMigrationPreservesAuditSequenceAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	db := openBefore(t, path, 29)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO audit_logs VALUES (40,'owner','project.update','project','project-a','{}','t')`)
	mustTaskExec(t, db, `INSERT INTO audit_logs VALUES (90,'system','task.cancel','task','task-001','{}','t')`)
	mustTaskExec(t, db, `DELETE FROM audit_logs WHERE id=90`)
	before := migrationRows(t, db, `SELECT * FROM audit_logs`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	if got := migrationRows(t, db, `SELECT * FROM audit_logs`); !reflect.DeepEqual(got, before) {
		t.Fatal("audit history changed", got, before)
	}
	if id, err := RecordAuditLog(context.Background(), db, AuditLog{Actor: "system", Action: "test", TargetType: "test", TargetID: "id"}); err != nil || id <= 90 {
		t.Fatal("audit sequence regressed", id, err)
	}
	mustTaskExec(t, db, `INSERT INTO artifacts VALUES ('file','execution-1',NULL,'check','out_file','record.md','artifacts/file','hash',1,1,'t')`)
	mustTaskExec(t, db, `INSERT INTO harness_records VALUES ('record','codex-developer','file','execution-1','task-001','ENG-123','record.md','t',NULL)`)
	mustTaskExec(t, db, `INSERT INTO stage_runs (id,execution_id,stage,attempt,status,request_artifact_id,started_at)
		VALUES ('stage','execution-1','check',1,'running','file','t')`)
	rows := migrationRows(t, db, `SELECT * FROM harness_records`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	if got := migrationRows(t, db, `SELECT * FROM harness_records`); !reflect.DeepEqual(got, rows) {
		t.Fatal(got, rows)
	}
	_, err := db.Exec(`UPDATE stage_runs SET request_artifact_id='missing'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatal(got)
	}
}
