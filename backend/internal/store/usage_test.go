package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestRecordUsageParticipatesInResultTransaction(t *testing.T) {
	db := openTaskTables(t)
	ctx := context.Background()
	for _, commit := range []bool{false, true} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := RecordUsage(ctx, tx, UsageRecord{
			ID: "usage-1", ExecutionID: "execution-1", WallTimeMS: 1234,
			InputTokens: new(int64(0)), OutputTokens: new(int64(9007199254740993)),
		}); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM usage_records`).Scan(&count); err != nil || count != map[bool]int{false: 0, true: 1}[commit] {
			t.Fatalf("commit=%v usage rows=%d err=%v", commit, count, err)
		}
	}
	var input, output, wall int64
	var cost sql.NullFloat64
	if err := db.QueryRow(`SELECT input_tokens, output_tokens, wall_time_ms, cost_amount FROM usage_records`).
		Scan(&input, &output, &wall, &cost); err != nil || input != 0 || output != 9007199254740993 || wall != 1234 || cost.Valid {
		t.Fatalf("saved usage input=%d output=%d wall=%d cost=%+v err=%v", input, output, wall, cost, err)
	}
}
