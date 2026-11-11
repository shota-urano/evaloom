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
// cancellation instructions in one transaction. A changed, known boot identity
// interrupts all active owned Executions in that same transaction.
// Unknown IDs are also denied;
// terminal reports can race a heartbeat and are harmless, but are not renewed.
func Heartbeat(ctx context.Context, db *sql.DB, runnerID, bootID string, runningIDs []string) ([]string, error) {
	reply, err := HeartbeatWithProposals(ctx, db, runnerID, bootID, runningIDs, nil)
	return reply.CancelExecutionIDs, err
}

// HeartbeatReply is the heartbeat response of spec 05 5 (cleanWorkspaceIds,
// Q137, is not part of it yet).
type HeartbeatReply struct {
	CancelExecutionIDs     []string
	CancelRouteProposalIDs []string
}

// HeartbeatWithProposals is Heartbeat with the route proposals the Runner
// reports as running (spec 05 5). Each must belong to the Runner. The reply
// also lists the Runner's running proposals whose task was cancelled (spec 04
// 3.8, Q133), repeated until the Runner reports their result.
func HeartbeatWithProposals(ctx context.Context, db *sql.DB, runnerID, bootID string, runningIDs, runningProposalIDs []string) (HeartbeatReply, error) {
	var reply HeartbeatReply
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return reply, err
	}
	defer tx.Rollback()
	for _, id := range runningProposalIDs {
		var own bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM route_proposals WHERE id = ? AND runner_id = ?)`, id, runnerID).Scan(&own); err != nil {
			return reply, err
		}
		if !own {
			return reply, ErrHeartbeatExecutionOwner
		}
	}
	for _, id := range runningIDs {
		var own bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM executions WHERE id = ? AND runner_id = ?)`, id, runnerID).Scan(&own); err != nil {
			return reply, err
		}
		if !own {
			return reply, ErrHeartbeatExecutionOwner
		}
	}
	var previous sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT boot_id FROM runners WHERE id = ?`, runnerID).Scan(&previous); err != nil {
		return reply, err
	}
	if previous.Valid && previous.String != bootID {
		if err := interruptRunnerTx(ctx, tx, runnerID, "runner_boot_changed"); err != nil {
			return reply, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE runners SET status = 'online', last_heartbeat_at = ?, boot_id = ? WHERE id = ?`, now, bootID, runnerID); err != nil {
		return reply, err
	}
	for _, id := range runningIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET last_heartbeat_at = ?
			WHERE id = ? AND runner_id = ? AND status IN `+activeExecutionStatuses, now, id, runnerID); err != nil {
			return reply, err
		}
	}
	// Read all active owned Executions, including a just-claimed one that the
	// Runner has not yet added to its reported list. Never consume the request:
	// repeat delivery until result transitions it out of the active set.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM executions WHERE runner_id = ?
		AND status IN `+activeExecutionStatuses+` AND cancel_requested_at IS NOT NULL ORDER BY id`, runnerID)
	if err != nil {
		return reply, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return reply, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return reply, err
	}
	proposals, err := tx.QueryContext(ctx, `SELECT rp.id FROM route_proposals rp JOIN tasks t ON t.id = rp.task_id
		WHERE rp.runner_id = ? AND rp.status = 'running' AND t.status = 'cancelled' ORDER BY rp.id`, runnerID)
	if err != nil {
		return reply, err
	}
	proposalIDs := []string{}
	for proposals.Next() {
		var id string
		if err := proposals.Scan(&id); err != nil {
			proposals.Close()
			return reply, err
		}
		proposalIDs = append(proposalIDs, id)
	}
	err = proposals.Err()
	proposals.Close()
	if err != nil {
		return reply, err
	}
	if err := tx.Commit(); err != nil {
		return reply, err
	}
	return HeartbeatReply{CancelExecutionIDs: ids, CancelRouteProposalIDs: proposalIDs}, nil
}

// RegisterRunner inserts a new offline Runner. Never replace an existing token.
func RegisterRunner(ctx context.Context, db *sql.DB, id, name, tokenHash, createdAt string) error {
	result, err := db.ExecContext(ctx, `INSERT INTO runners
		(id, name, token_hash, status, created_at)
		VALUES (?, ?, ?, 'offline', ?) ON CONFLICT(id) DO NOTHING`, id, name, tokenHash, createdAt)
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
	ID, Name, Status, CreatedAt string
	LastHeartbeatAt             *string
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
