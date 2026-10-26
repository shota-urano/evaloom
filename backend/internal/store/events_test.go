package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestExecutionEventAppendValidationAndTransactions(t *testing.T) {
	ctx := context.Background()
	db := openTaskTables(t)
	for _, tc := range []struct {
		execution string
		kind      model.ExecutionEventType
		payload   string
	}{
		{"execution-1", model.EventLog, "invalid"},
		{"execution-1", model.EventLog, ""},
		{"execution-1", "unknown", `{}`},
		{"missing", model.EventLog, `{}`},
	} {
		if _, err := AppendExecutionEvent(ctx, db, tc.execution, tc.kind, json.RawMessage(tc.payload)); err == nil {
			t.Fatalf("invalid append succeeded: %+v", tc)
		}
	}
	events, err := ListExecutionEvents(ctx, db, "execution-1", 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("failed append left events: %+v err=%v", events, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendExecutionEvent(ctx, tx, "execution-1", model.EventLog, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	events, err = ListExecutionEvents(ctx, db, "execution-1", 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("rolled back event visible: %+v err=%v", events, err)
	}
	// A write failure cannot yield an event for an active SSE reader.
	mustTaskExec(t, db, `CREATE TRIGGER reject_event BEFORE INSERT ON execution_events
		BEGIN SELECT RAISE(FAIL, 'event write unavailable'); END`)
	if _, err := AppendExecutionEvent(ctx, db, "execution-1", model.EventLog, json.RawMessage(`{}`)); err == nil {
		t.Fatal("failed write succeeded")
	}
	events, err = ListExecutionEvents(ctx, db, "execution-1", 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("failed write visible: %+v err=%v", events, err)
	}
}

func TestExecutionEventsPersistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db := openRegistry(t, path)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	id, err := AppendExecutionEvent(ctx, db, "execution-1", model.EventLog, json.RawMessage(`{"text":"saved"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRegistry(t, path)
	if err := CheckExecutionEventCursor(ctx, reopened, "execution-1", &id); err != nil {
		t.Fatal(err)
	}
	events, err := ListExecutionEvents(ctx, reopened, "execution-1", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("reopened events=%+v err=%v", events, err)
	}
	event := events[0]
	if event.ID != id || event.ExecutionID != "execution-1" || event.Type != model.EventLog || string(event.Payload) != `{"text":"saved"}` || event.CreatedAt == "" {
		t.Fatalf("persisted event=%+v", event)
	}
	if err := CheckExecutionEventCursor(ctx, reopened, "missing", nil); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("missing execution: %v", err)
	}
}
