package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type AuditLog struct {
	Actor, Action, TargetType, TargetID string
	Detail                              map[string]any
}

// RecordAuditLog returns the database-assigned audit ID. Callers must remove
// secrets before supplying Detail; this storage layer does not redact content.
func RecordAuditLog(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record AuditLog) (int64, error) {
	detail, err := recordDetail(record.Detail)
	if err != nil {
		return 0, err
	}
	result, err := db.ExecContext(ctx, `INSERT INTO audit_logs
		(actor, action, target_type, target_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		record.Actor, record.Action, record.TargetType, record.TargetID, detail, recordTime())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func recordDetail(detail map[string]any) (string, error) {
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return "", fmt.Errorf("store: encode detail: %w", err)
	}
	return string(encoded), nil
}

func recordTime() string { return time.Now().UTC().Format(time.RFC3339) }
