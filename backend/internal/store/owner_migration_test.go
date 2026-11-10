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

func TestOwnerMigrationPreservesExistingDatabaseAndCredentialsOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.db")
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
		if m.version < 24 {
			previous[m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	before := migrationRows(t, db, `SELECT * FROM runners`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = open(context.Background(), path, migrationsBefore(t, 25))
	if err != nil {
		t.Fatal(err)
	}
	if got := migrationRows(t, db, `SELECT * FROM runners`); !reflect.DeepEqual(got, before) {
		t.Fatal("migration modified existing registry data")
	}
	for _, table := range []string{"owner_credentials", "owner_sessions"} {
		if got := migrationRows(t, db, `SELECT * FROM `+table); len(got) != 0 {
			t.Fatalf("migration populated %s", table)
		}
	}
	if err := SetOwnerCredentials(context.Background(), db, "owner", "hash"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `INSERT INTO owner_sessions (id, token_hash, csrf_token_hash, created_at, expires_at, via)
		VALUES ('session', 'token-hash', 'csrf-hash', ?, ?, 'tailnet')`, registryTime, registryTime)
	credentials := migrationRows(t, db, `SELECT * FROM owner_credentials`)
	sessions := migrationRows(t, db, `SELECT * FROM owner_sessions`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = open(context.Background(), path, migrationsBefore(t, 25))
	if err != nil {
		t.Fatal(err)
	}
	if got := migrationRows(t, db, `SELECT * FROM owner_credentials`); !reflect.DeepEqual(got, credentials) {
		t.Fatal("restart changed owner credentials")
	}
	if got := migrationRows(t, db, `SELECT * FROM owner_sessions`); !reflect.DeepEqual(got, sessions) {
		t.Fatal("restart changed owner sessions")
	}
	for _, statement := range []string{
		`INSERT INTO owner_credentials VALUES ('second-owner', 'hash', 'now', 'now')`,
		`INSERT INTO owner_sessions VALUES ('duplicate-token', 'token-hash', 'csrf', 'now', 'later', 'loopback')`,
	} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	}
	_, err = db.Exec(`UPDATE owner_sessions SET via = 'internet'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	// The migration must use exactly the specification's columns.
	for table, want := range map[string][]string{
		"owner_credentials": {"id", "password_hash", "created_at", "updated_at"},
		"owner_sessions":    {"id", "token_hash", "csrf_token_hash", "created_at", "expires_at", "via"},
	} {
		rows := migrationRows(t, db, `SELECT name FROM pragma_table_info('`+table+`') ORDER BY cid`)
		var got []string
		for _, row := range rows {
			got = append(got, row[0].(string))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s columns = %v, want %v", table, got, want)
		}
	}
}
