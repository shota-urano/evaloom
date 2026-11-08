package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

// spec 03 3.3.1 (Q73): 0015 gives Projects registered before the column an
// empty setup_allowed_hosts, so their setup stays fully offline.
func TestSetupAllowedHostsMigrationDefaultsExistingProjectsToEmpty(t *testing.T) {
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
		if migration.version < 15 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	if got := migrationRows(t, db, `SELECT setup_allowed_hosts FROM projects`); !reflect.DeepEqual(got, [][]any{{"[]"}}) {
		t.Fatalf("setup_allowed_hosts of existing rows = %#v, want []", got)
	}
	p, err := GetProject(context.Background(), db, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if p.SetupAllowedHosts == nil || len(p.SetupAllowedHosts) != 0 {
		t.Fatalf("SetupAllowedHosts = %#v, want empty slice", p.SetupAllowedHosts)
	}
}
