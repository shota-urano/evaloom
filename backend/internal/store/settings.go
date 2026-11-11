package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ReadSettings returns the raw JSON value of every settings row by key (spec
// 02 3.8). Callers choose the keys they know and validate the values.
func ReadSettings(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("store: read settings: %w", err)
	}
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: read settings: %w", err)
		}
		values[key] = value
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read settings: %w", err)
	}
	return values, nil
}

// PutSetting writes one settings row (spec 02 3.8). The caller writes the
// audit_logs row in the same transaction.
func PutSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	if !json.Valid([]byte(value)) {
		return fmt.Errorf("store: write setting %s: invalid JSON", key)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, recordTime()); err != nil {
		return fmt.Errorf("store: write setting %s: %w", key, err)
	}
	return nil
}
