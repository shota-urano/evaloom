package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestOpenEnablesWAL(t *testing.T) {
	// URI metacharacters must remain part of the filename, not connection options.
	path := filepath.Join(t.TempDir(), "evaloom ?#%.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertJournalMode(t, db, "wal")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at the supplied path: %v", err)
	}
	// Force replacement of the underlying connection and check its settings.
	db.SetMaxIdleConns(0)
	assertJournalMode(t, db, "wal")
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, err = %v; want 1", foreignKeys, err)
	}
}

func TestOpenConvertsExistingDatabaseToWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = DELETE; CREATE TABLE existing (value TEXT); INSERT INTO existing VALUES ('preserved')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	assertJournalMode(t, db, "delete")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertJournalMode(t, db, "wal")
	var value string
	if err := db.QueryRow("SELECT value FROM existing").Scan(&value); err != nil || value != "preserved" {
		t.Fatalf("existing data = %q, err = %v", value, err)
	}
}

func TestOpenAppliesOnlyPendingMigrationsInNumericOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source := fstest.MapFS{
		"2_create.sql":  {Data: []byte("CREATE TABLE sample (value TEXT NOT NULL);")},
		"10_insert.sql": {Data: []byte("INSERT INTO sample (value) VALUES ('first');")},
	}
	db, err := open(context.Background(), path, source)
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, db, []string{"first"})
	assertVersions(t, db, []int64{2, 10})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening must skip CREATE TABLE and the non-idempotent INSERT, while
	// applying the newly shipped migration against the persisted schema.
	source["11_extend.sql"] = &fstest.MapFile{Data: []byte(`
		ALTER TABLE sample ADD COLUMN extra TEXT;
		INSERT INTO sample (value, extra) VALUES ('second', 'new column');`)}
	db, err = open(context.Background(), path, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertValues(t, db, []string{"first", "second"})
	assertVersions(t, db, []int64{2, 10, 11})
	assertJournalMode(t, db, "wal")
	var extra string
	if err := db.QueryRow("SELECT extra FROM sample WHERE value = 'second'").Scan(&extra); err != nil || extra != "new column" {
		t.Fatalf("new column = %q, err = %v", extra, err)
	}
	var appliedAt string
	if err := db.QueryRow("SELECT applied_at FROM schema_migrations WHERE version = 11").Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, appliedAt); err != nil || !strings.HasSuffix(appliedAt, "Z") {
		t.Fatalf("migration timestamp %q must be UTC RFC3339: %v", appliedAt, err)
	}
}

func TestOpenRollsBackFailedMigrationAndStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source := fstest.MapFS{
		"1_create.sql": {Data: []byte("CREATE TABLE sample (value TEXT);")},
		"2_fail.sql": {Data: []byte(`
			INSERT INTO sample VALUES ('must roll back');
			CREATE TABLE partial_schema (id INTEGER);
			INSERT INTO missing_table VALUES (1);`)},
		"3_later.sql": {Data: []byte("INSERT INTO sample VALUES ('later');")},
	}
	db, err := open(context.Background(), path, source)
	if err == nil || db != nil || !strings.Contains(err.Error(), "2_fail.sql") {
		if db != nil {
			db.Close()
		}
		t.Fatalf("Open = %v, %v; want nil database and failing migration name", db, err)
	}
	// Inspect on a separate connection, after the failed startup has closed its
	// connection. Earlier migrations persist; failed and later ones do not.
	inspection, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, inspection, nil)
	assertVersions(t, inspection, []int64{1})
	var count int
	if err := inspection.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'partial_schema'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partially created tables = %d, err = %v; want 0", count, err)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
	source["2_fail.sql"] = &fstest.MapFile{Data: []byte("INSERT INTO sample VALUES ('recovered');")}
	db, err = open(context.Background(), path, source)
	if err != nil {
		t.Fatalf("retry after fixing the unapplied migration: %v", err)
	}
	defer db.Close()
	assertValues(t, db, []string{"recovered", "later"})
	assertVersions(t, db, []int64{1, 2, 3})
}

func TestOpenRejectsInvalidMigrations(t *testing.T) {
	for name, source := range map[string]fstest.MapFS{
		"invalid name": {"create.sql": {Data: []byte("SELECT 1;")}},
		"zero version": {"0_create.sql": {Data: []byte("SELECT 1;")}},
		"duplicate version": {
			"1_first.sql":   {Data: []byte("SELECT 1;")},
			"01_second.sql": {Data: []byte("SELECT 2;")},
		},
		"empty SQL": {"1_empty.sql": {Data: []byte(" \n")}},
	} {
		t.Run(name, func(t *testing.T) {
			db, err := open(context.Background(), filepath.Join(t.TempDir(), "evaloom.db"), source)
			if err == nil || db != nil {
				if db != nil {
					db.Close()
				}
				t.Fatalf("Open = %v, %v; want nil database and an error", db, err)
			}
		})
	}
}

func TestOpenReturnsErrors(t *testing.T) {
	for _, path := range []string{"", ":memory:", t.TempDir(), filepath.Join(t.TempDir(), "missing", "evaloom.db")} {
		t.Run(path, func(t *testing.T) {
			db, err := Open(context.Background(), path)
			if err == nil || db != nil {
				if db != nil {
					db.Close()
				}
				t.Fatalf("Open = %v, %v; want nil database and an error", db, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "cancelled.db"))
	if db != nil || !errors.Is(err, context.Canceled) {
		if db != nil {
			db.Close()
		}
		t.Fatalf("cancelled Open = %v, %v; want nil, context.Canceled", db, err)
	}
}

func assertJournalMode(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != want {
		t.Fatalf("journal_mode = %q, err = %v; want %q", mode, err, want)
	}
}

func assertValues(t *testing.T, db *sql.DB, want []string) {
	t.Helper()
	rows, err := db.Query("SELECT value FROM sample ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored values = %v; want %v", got, want)
	}
}

func assertVersions(t *testing.T, db *sql.DB, want []int64) {
	t.Helper()
	rows, err := db.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		got = append(got, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applied migrations = %v; want %v", got, want)
	}
}
