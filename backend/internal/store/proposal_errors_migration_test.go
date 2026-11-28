package store

import (
	"path/filepath"
	"reflect"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestProposalErrorMigrationPreservesRowsAndReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proposals.db")
	db := openBefore(t, path, 31)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO route_proposals
		(id,task_id,trigger,seen_labels,candidates,status,runner_id,proposed_label,reason,error_class,created_at,finished_at)
		VALUES ('proposal','task-001','unknown_label','["route:old"]','[{"label":"route:old"}]',
		'failed','mac-mini','route:old','reason','contract_error','created','finished')`)
	mustTaskExec(t, db, `INSERT INTO owner_decisions
		(id,kind,task_id,subject_id,basis,detail,status,created_at)
		VALUES ('decision','route','task-001','proposal','proposal','{}','open','created')`)
	mustTaskExec(t, db, `UPDATE route_proposals SET decision_id='decision'`)
	mustTaskExec(t, db, `INSERT INTO artifacts
		(id,route_proposal_id,stage,kind,name,storage_path,sha256,size_bytes,redacted,created_at)
		VALUES ('artifact','proposal','proposal','proposal_result','proposal.json','stored','hash',2,0,'created')`)
	queries := []string{
		`SELECT * FROM route_proposals`, `SELECT * FROM owner_decisions`, `SELECT * FROM artifacts`,
		`SELECT * FROM pragma_table_info('route_proposals')`,
		`SELECT name FROM sqlite_master WHERE type='table' AND name != 'program_clients' ORDER BY name`, // 0034 adds program_clients
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = migrationRows(t, db, query)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db = openRegistry(t, path)
		for i, query := range queries {
			if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before[i]) {
				t.Fatalf("migration changed %s: got=%v want=%v", query, got, before[i])
			}
		}
		for _, class := range []string{"config_error", "work_failed"} {
			mustTaskExec(t, db, `UPDATE route_proposals SET error_class=? WHERE id='proposal'`, class)
		}
		mustTaskExec(t, db, `UPDATE route_proposals SET error_class='contract_error' WHERE id='proposal'`)
		_, err := db.Exec(`UPDATE route_proposals SET error_class='cancelled' WHERE id='proposal'`)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
		_, err = db.Exec(`UPDATE artifacts SET route_proposal_id='missing'`)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
		if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
			t.Fatalf("foreign key violations: %v", got)
		}
		if got := migrationRows(t, db, `SELECT count(*) FROM schema_migrations WHERE version=31`); !reflect.DeepEqual(got, [][]any{{int64(1)}}) {
			t.Fatalf("migration applied incorrectly: %v", got)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
