package store

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"

	"modernc.org/sqlite/lib"
)

func TestHarnessAdapterMigrationPreservesExistingDataAndForeignKeys(t *testing.T) {
	previous := fstest.MapFS{}
	for _, name := range []string{"0001_registry.sql", "0002_tasks.sql", "0003_verification_records.sql"} {
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	seedVerificationRecords(t, db)
	mustTaskExec(t, db, `UPDATE executions SET harness_id = 'codex-developer', profile_id = 'profile-1'`)
	var beforePath string
	err = db.QueryRow(`SELECT repo_path FROM harnesses WHERE id='codex-developer'`).Scan(&beforePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = open(context.Background(), path, migrationsBefore(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	var afterPath string
	if err := db.QueryRow(`SELECT repo_path FROM harnesses WHERE id='codex-developer'`).Scan(&afterPath); err != nil || afterPath != beforePath {
		t.Fatalf("path=%s err=%v", afterPath, err)
	}

	var references int
	if err := db.QueryRow(`SELECT count(*) FROM executions e
		JOIN harnesses h ON h.id = e.harness_id
		JOIN harness_profiles hp ON hp.id = e.profile_id AND hp.harness_id = h.id
		JOIN project_harnesses ph ON ph.harness_id = h.id AND ph.profile_id = hp.id`).Scan(&references); err != nil || references != 1 {
		t.Fatalf("preserved references=%d err=%v", references, err)
	}
	for _, table := range []string{"harnesses", "harness_profiles"} {
		_, err := db.Exec("DELETE FROM " + table)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
	if _, err := db.Exec(`UPDATE harnesses SET adapter = 'new_harness'`); err != nil {
		t.Fatal(err)
	}
	var adapter string
	if err := db.QueryRow(`SELECT adapter FROM harnesses WHERE id='codex-developer'`).Scan(&adapter); err != nil || adapter != "new_harness" {
		t.Fatal(adapter, err)
	}
	assertVersions(t, db, []int64{1, 2, 3, 4})
}
