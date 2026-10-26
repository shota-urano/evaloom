package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

var (
	ErrExecutionNotFound      = errors.New("execution not found")
	ErrEventCursorUnavailable = errors.New("event cursor is not available for this execution")
)

type ExecutionEvent struct {
	ID          int64
	ExecutionID string
	Type        model.ExecutionEventType
	Payload     json.RawMessage
	CreatedAt   string
}

// AppendExecutionEvent is the Control Plane's persistence entry point, not a
// Runner HTTP endpoint. The caller must remove secrets from payload. Passing a
// transaction allows future producers to commit state and event together;
// readers only observe committed rows, without an in-memory notification step.
func AppendExecutionEvent(ctx context.Context, writer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, executionID string, kind model.ExecutionEventType, payload json.RawMessage) (int64, error) {
	if err := kind.Validate(); err != nil {
		return 0, err
	}
	if !json.Valid(payload) {
		return 0, fmt.Errorf("store: invalid execution event JSON")
	}
	result, err := writer.ExecContext(ctx, `INSERT INTO execution_events
		(execution_id, type, payload, created_at) VALUES (?, ?, ?, ?)`,
		executionID, kind, string(payload), recordTime())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// CheckExecutionEventCursor validates the stream before HTTP headers are sent.
// A supplied cursor must still identify a row belonging to this Execution.
// Nil starts at the beginning; no retention or pruning policy is imposed here.
func CheckExecutionEventCursor(ctx context.Context, db *sql.DB, executionID string, after *int64) error {
	var exists, cursorExists bool
	err := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM executions WHERE id = ?),
		EXISTS(SELECT 1 FROM execution_events WHERE execution_id = ? AND id = ?)`,
		executionID, executionID, after).Scan(&exists, &cursorExists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrExecutionNotFound
	}
	if after != nil && !cursorExists {
		return ErrEventCursorUnavailable
	}
	return nil
}

// ListExecutionEvents reads a bounded batch strictly after the last delivered
// global AUTOINCREMENT ID. Close the rows before returning so a slow stream
// never holds the database's sole connection or a long-lived read transaction.
func ListExecutionEvents(ctx context.Context, db *sql.DB, executionID string, after int64, limit int) ([]ExecutionEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, execution_id, type, payload, created_at
		FROM execution_events WHERE execution_id = ? AND id > ? ORDER BY id LIMIT ?`,
		executionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []ExecutionEvent{}
	for rows.Next() {
		var event ExecutionEvent
		var payload string
		if err := rows.Scan(&event.ID, &event.ExecutionID, &event.Type, &payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := event.Type.Validate(); err != nil {
			return nil, fmt.Errorf("store: invalid stored event type: %w", err)
		}
		if !json.Valid([]byte(payload)) {
			return nil, fmt.Errorf("store: invalid stored event JSON (id %d)", event.ID)
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	return events, rows.Err()
}
