package store

import (
	"context"
	"database/sql"
)

// RunnerSummary deliberately excludes token_hash and local filesystem paths.
type RunnerSummary struct {
	ID, Name, Status, LastHeartbeatAt, CreatedAt string
}

// ListRunners reads IDs in SQLite's binary ascending order, strictly after the
// supplied boundary. The boundary need not still exist. Callers may request an
// extra row to detect continuation without a count query or offset scan.
func ListRunners(ctx context.Context, db *sql.DB, after string, limit int) ([]RunnerSummary, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, status, last_heartbeat_at, created_at
		FROM runners WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RunnerSummary{}
	for rows.Next() {
		var runner RunnerSummary
		if err := rows.Scan(&runner.ID, &runner.Name, &runner.Status, &runner.LastHeartbeatAt, &runner.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, runner)
	}
	return result, rows.Err()
}
