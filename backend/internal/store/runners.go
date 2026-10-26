package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrRunnerExists = errors.New("runner is already registered")

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
