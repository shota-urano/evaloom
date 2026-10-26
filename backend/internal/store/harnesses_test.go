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
	before, err := GetHarness(context.Background(), db, "codex-developer")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	after, err := GetHarness(context.Background(), db, before.ID)
	if err != nil || before.Adapter != after.Adapter || before.RepoPath != after.RepoPath || before.CreatedAt != after.CreatedAt {
		t.Fatalf("migrated harness=%+v before=%+v err=%v", after, before, err)
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
	if got, err := GetHarness(context.Background(), db, before.ID); err != nil || got.Adapter != "new_harness" {
		t.Fatalf("extension name=%+v err=%v", got, err)
	}
	assertVersions(t, db, []int64{1, 2, 3, 4})
}
