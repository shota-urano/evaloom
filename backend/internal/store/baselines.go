package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// BaselineStore is Control Plane persistence. Runner wiring must use a remote
// implementation of its repository interface, never this database handle.
type BaselineStore struct{ DB *sql.DB }

var (
	ErrBaselineScope  = errors.New("store: baseline worktree does not belong to the execution or start SHA differs")
	ErrBaselineExists = errors.New("store: worktree baseline is immutable")
)

// FindBaseline follows executions.worktree_id, not the latest Execution ID or
// task alone: continue reuses a worktree, fresh creates a different one. The
// original test_runs row and its artifact ownership remain unchanged.
func (s BaselineStore) FindBaseline(ctx context.Context, executionID, worktreeID string) (*model.TestRun, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	base, err := baselineScope(ctx, tx, executionID, worktreeID)
	if err != nil {
		return nil, err
	}
	run, err := findBaseline(ctx, tx, worktreeID)
	if err == nil && run != nil && run.TargetSHA != base {
		err = ErrBaselineScope
	}
	return run, err
}

// SaveBaseline serializes the existence check and insert without new columns
// or indexes. Retrying the identical record succeeds; replacing a baseline
// cannot succeed, even under another Execution of the same worktree.
func (s BaselineStore) SaveBaseline(ctx context.Context, worktreeID string, run model.TestRun) error {
	if run.ID == "" || run.Kind != "baseline" || run.Command == "" || run.StartedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) {
		return fmt.Errorf("store: invalid baseline record")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	base, err := baselineScope(ctx, tx, run.ExecutionID, worktreeID)
	if err != nil {
		return err
	}
	if run.TargetSHA != base {
		return ErrBaselineScope
	}
	previous, err := findBaseline(ctx, tx, worktreeID)
	if err != nil {
		return err
	}
	// Normalize times to the same UTC representation as the persisted record.
	run.StartedAt, run.FinishedAt = run.StartedAt.UTC(), run.FinishedAt.UTC()
	if previous != nil {
		if reflect.DeepEqual(*previous, run) {
			return nil
		}
		return ErrBaselineExists
	}
	if run.TestIDsArtifactID != nil {
		var own bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts
			WHERE id = ? AND execution_id = ? AND kind = 'test_ids')`, *run.TestIDsArtifactID, run.ExecutionID).Scan(&own); err != nil {
			return err
		}
		if !own {
			return fmt.Errorf("store: baseline IDs must reference this execution's test_ids artifact")
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO test_runs
		(id, execution_id, kind, target_sha, command, exit_code, total, passed, failed, skipped,
		test_ids_artifact_id, timed_out, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.ExecutionID, run.Kind, run.TargetSHA, run.Command, run.ExitCode, run.Total, run.Passed, run.Failed, run.Skipped,
		run.TestIDsArtifactID, run.TimedOut, run.StartedAt.Format(time.RFC3339Nano), run.FinishedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func baselineScope(ctx context.Context, tx *sql.Tx, executionID, worktreeID string) (string, error) {
	var base string
	err := tx.QueryRowContext(ctx, `SELECT w.base_sha FROM executions e
		JOIN tasks t ON t.id = e.task_id JOIN worktrees w ON w.id = e.worktree_id
		WHERE e.id = ? AND w.id = ? AND w.task_id = e.task_id AND w.project_id = t.project_id
		AND w.runner_id = e.runner_id AND w.base_sha = e.base_sha AND w.status != 'cleaned'`, executionID, worktreeID).Scan(&base)
	if errors.Is(err, sql.ErrNoRows) || err == nil && base == "" {
		return "", ErrBaselineScope
	}
	return base, err
}

func findBaseline(ctx context.Context, tx *sql.Tx, worktreeID string) (*model.TestRun, error) {
	var run model.TestRun
	var started, finished string
	err := tx.QueryRowContext(ctx, `SELECT r.id, r.execution_id, r.kind, r.target_sha, r.command,
		r.exit_code, r.total, r.passed, r.failed, r.skipped, r.test_ids_artifact_id, r.timed_out, r.started_at, r.finished_at
		FROM test_runs r JOIN executions e ON e.id = r.execution_id
		JOIN worktrees w ON w.id = e.worktree_id
		WHERE w.id = ? AND e.task_id = w.task_id AND e.runner_id = w.runner_id AND r.kind = 'baseline'
		ORDER BY e.seq, r.started_at, r.id LIMIT 1`, worktreeID).Scan(
		&run.ID, &run.ExecutionID, &run.Kind, &run.TargetSHA, &run.Command,
		&run.ExitCode, &run.Total, &run.Passed, &run.Failed, &run.Skipped, &run.TestIDsArtifactID, &run.TimedOut, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if run.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return nil, err
	}
	if run.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
		return nil, err
	}
	return &run, nil
}
