package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestNotificationMigrationPreservesPendingRequestsAndReservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evaloom.db")
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, entry := range entries {
		if entry.Name() >= "0007" {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous[entry.Name()] = &fstest.MapFile{Data: data}
	}
	db, err := open(ctx, path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO notification_requests (execution_id, status, created_at)
		VALUES ('execution-1', 'blocked', ?)`, registryTime)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	var attempted sql.NullString
	var created string
	if err := db.QueryRow(`SELECT attempted_at, created_at FROM notification_requests
		WHERE execution_id = 'execution-1' AND status = 'blocked'`).Scan(&attempted, &created); err != nil || attempted.Valid || created != registryTime {
		t.Fatalf("pending request changed by migration: %v %s %v", attempted, created, err)
	}
	mustTaskExec(t, db, `UPDATE notification_requests SET attempted_at = ?`, registryTime)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	if err := db.QueryRow(`SELECT attempted_at FROM notification_requests WHERE execution_id = 'execution-1' AND status = 'blocked'`).Scan(&attempted); err != nil || !attempted.Valid || attempted.String != registryTime {
		t.Fatalf("reservation lost on reopen: %v %v", attempted, err)
	}
}
