package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestNotificationFailureMigrationPreservesEventsAndCursor(t *testing.T) {
	for _, retain := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained events", false: "all events deleted"}[retain], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "evaloom.db")
			entries, err := migrationFiles.ReadDir("migrations")
			if err != nil {
				t.Fatal(err)
			}
			previous := fstest.MapFS{}
			for _, entry := range entries {
				if entry.Name() >= "0008" {
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
			t.Cleanup(func() { db.Close() })
			seedRegistry(t, db)
			seedTaskTables(t, db)
			mustTaskExec(t, db, `INSERT INTO execution_events (id, execution_id, type, payload, created_at)
				VALUES (41, 'execution-1', 'notification_sent', '{"kind":"blocked","status":"blocked","channel":"macos"}', ?)`, registryTime)
			mustTaskExec(t, db, `INSERT INTO execution_events (id, execution_id, type, payload, created_at)
				VALUES (99, 'execution-1', 'log', '{}', ?)`, registryTime)
			mustTaskExec(t, db, `DELETE FROM execution_events WHERE id = 99`)
			if !retain {
				mustTaskExec(t, db, `DELETE FROM execution_events`)
			}
			before, err := ListExecutionEvents(ctx, db, "execution-1", 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = openRegistry(t, path)
			after, err := ListExecutionEvents(ctx, db, "execution-1", 0, 100)
			if err != nil || len(after) != len(before) {
				t.Fatalf("migration changed events: before=%+v after=%+v err=%v", before, after, err)
			}
			for i, event := range after {
				want := before[i]
				if event.ID != want.ID || event.ExecutionID != want.ExecutionID || event.Type != want.Type ||
					string(event.Payload) != string(want.Payload) || event.CreatedAt != want.CreatedAt {
					t.Fatalf("migration changed event: %+v want %+v", event, want)
				}
			}
			id, err := AppendExecutionEvent(ctx, db, "execution-1", model.EventNotificationFailed,
				json.RawMessage(`{"kind":"failed","channel":"macos","reason":"other"}`))
			if err != nil || id <= 99 {
				t.Fatalf("failure event append: id=%d err=%v; want id > deleted cursor 99", id, err)
			}
		})
	}
}

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
