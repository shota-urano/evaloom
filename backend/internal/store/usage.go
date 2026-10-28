package store

import (
	"context"
	"database/sql"
)

// UsageRecord mirrors the existing usage_records columns (spec 02, section 3.2).
// Unknown tokens and cost are nil; zero is an available measurement.
type UsageRecord struct {
	ID, ExecutionID           string
	InputTokens, OutputTokens *int64
	WallTimeMS                int64
	CostAmount                *float64
}

// RecordUsage is a Control Plane storage primitive used in the same
// transaction as result persistence and the succeeded transition. It does not
// infer token counts, prices, or a second duration's meaning from row order.
func RecordUsage(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record UsageRecord) error {
	_, err := db.ExecContext(ctx, `INSERT INTO usage_records
		(id, execution_id, input_tokens, output_tokens, wall_time_ms, cost_amount, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, record.ID, record.ExecutionID,
		record.InputTokens, record.OutputTokens, record.WallTimeMS, record.CostAmount, recordTime())
	return err
}
