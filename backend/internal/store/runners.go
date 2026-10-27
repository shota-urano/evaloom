package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrRunnerExists = errors.New("runner is already registered")

var ErrHeartbeatExecutionOwner = errors.New("heartbeat execution does not belong to runner")

// Heartbeat validates ownership before recording liveness and reading pending
// cancellation instructions in one transaction. Unknown IDs are also denied;
// terminal reports can race a heartbeat and are harmless, but are not renewed.
// Boot-ID recovery belongs to the separate restart-recovery lifecycle path.
func Heartbeat(ctx context.Context, db *sql.DB, runnerID string, runningIDs []string) ([]string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, id := range runningIDs {
		var own bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM executions WHERE id = ? AND runner_id = ?)`, id, runnerID).Scan(&own); err != nil {
			return nil, err
		}
		if !own {
			return nil, ErrHeartbeatExecutionOwner
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE runners SET status = 'online', last_heartbeat_at = ? WHERE id = ?`, now, runnerID); err != nil {
		return nil, err
	}
	for _, id := range runningIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET last_heartbeat_at = ?
			WHERE id = ? AND runner_id = ? AND status IN `+activeExecutionStatuses, now, id, runnerID); err != nil {
			return nil, err
		}
	}
	// Read all active owned Executions, including a just-claimed one that the
	// Runner has not yet added to its reported list. Never consume the request:
	// repeat delivery until result transitions it out of the active set.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM executions WHERE runner_id = ?
		AND status IN `+activeExecutionStatuses+` AND cancel_requested_at IS NOT NULL ORDER BY id`, runnerID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// RegisterRunner inserts a new offline Runner. Never replace an existing token.
func RegisterRunner(ctx context.Context, db *sql.DB, id, name, tokenHash, root, createdAt string) error {
	result, err := db.ExecContext(ctx, `INSERT INTO runners
		(id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES (?, ?, ?, ?, 'offline', '', ?) ON CONFLICT(id) DO NOTHING`, id, name, tokenHash, root, createdAt)
	if err != nil {
		return fmt.Errorf("store: register runner: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRunnerExists
	}
	return nil
}

// RunnerIDByTokenHash resolves the credential's identity, not a caller's ID.
// Ambiguous credentials fail closed even if rows were inserted outside the CLI.
func RunnerIDByTokenHash(ctx context.Context, db *sql.DB, hash string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM runners WHERE token_hash = ? LIMIT 2`, hash)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var id string
	count := 0
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "", sql.ErrNoRows
	}
	if count != 1 || id == "" {
		return "", fmt.Errorf("store: ambiguous or empty runner identity")
	}
	return id, nil
}

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
