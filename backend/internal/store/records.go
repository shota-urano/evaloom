package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Finding is the persisted shape from data-model section 5.5. Gate-specific
// interpretation of Rule belongs to the gate implementation.
type Finding struct {
	Rule    string `json:"rule"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type GateResult struct {
	ID, ExecutionID    string
	Gate               int
	Result             string
	BaseSHA, TargetSHA string
	Findings           []Finding
}

// RecordGateResult serializes typed findings before writing, so findings is
// always a JSON array, including when there are no findings.
func RecordGateResult(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record GateResult) error {
	if record.Findings == nil {
		record.Findings = []Finding{}
	}
	findings, err := json.Marshal(record.Findings)
	if err != nil {
		return fmt.Errorf("store: encode findings: %w", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO gate_results
		(id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, record.ID, record.ExecutionID, record.Gate,
		record.Result, record.BaseSHA, record.TargetSHA, string(findings), recordTime())
	return err
}

type ExecutionBlock struct {
	ID, ExecutionID, Cause string
	GateResultID           *string
	HeadSHA                string
	Detail                 map[string]any
}

// SaveExecutionBlockTx is shared by Context Builder and Runner reports. Both
// the undecided block and its event belong to the state change's transaction.
func SaveExecutionBlockTx(ctx context.Context, tx *sql.Tx, record ExecutionBlock) error {
	record.ID = ulid.Make().String()
	if err := RecordExecutionBlock(ctx, tx, record); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"cause": record.Cause, "blockId": record.ID})
	_, err := AppendExecutionEvent(ctx, tx, record.ExecutionID, model.EventBlocked, payload)
	return err
}

// RecordExecutionBlock creates an undecided block. The lifecycle service owns
// subsequent owner decisions and execution state transitions.
func RecordExecutionBlock(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record ExecutionBlock) error {
	detail, err := recordDetail(record.Detail)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO execution_blocks
		(id, execution_id, cause, gate_result_id, head_sha, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, record.ID, record.ExecutionID, record.Cause,
		record.GateResultID, record.HeadSHA, detail, recordTime())
	return err
}

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
