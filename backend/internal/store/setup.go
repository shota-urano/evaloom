package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// SetupStore belongs to the Control Plane. Runner delivery is an interface
// until the setup measurement/status wire contract is defined by the specs.
type SetupStore struct{ DB *sql.DB }

func (s SetupStore) SetupSucceeded(ctx context.Context, executionID, worktreeID string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := baselineScope(ctx, tx, executionID, worktreeID); err != nil {
		return false, err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT setup_status FROM worktrees WHERE id=?`, worktreeID).Scan(&status); err != nil {
		return false, err
	}
	return status == "succeeded", nil
}

// SaveSetup records one attempt per Execution. A failed continue attempt may
// retry under a new Execution; an ACK loss only resends this immutable record.
// succeeded is separate because a runtime failure can have an exit code of 0
// (e.g. isolation/cleanup failure), which must not mark setup successful.
func (s SetupStore) SaveSetup(ctx context.Context, worktreeID string, run model.TestRun, succeeded bool) error {
	if id, err := ulid.ParseStrict(run.ID); err != nil || id.String() != run.ID {
		return fmt.Errorf("store: invalid setup ID")
	}
	if run.Kind != "setup" || run.Command == "" || run.StartedAt.IsZero() || run.FinishedAt.IsZero() ||
		run.FinishedAt.Before(run.StartedAt) || run.Total != nil || run.Passed != nil || run.Failed != nil || run.Skipped != nil ||
		run.TestIDsArtifactID != nil || succeeded && (run.TimedOut || run.ExitCode == nil || *run.ExitCode != 0) {
		return fmt.Errorf("store: invalid setup record")
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
		return fmt.Errorf("store: setup target differs from start SHA")
	}
	status := "failed"
	if succeeded {
		status = "succeeded"
	}
	run.StartedAt, run.FinishedAt = run.StartedAt.UTC(), run.FinishedAt.UTC()
	var previous model.TestRun
	var started, finished, savedStatus string
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.execution_id,r.kind,r.target_sha,r.command,r.exit_code,r.timed_out,
		r.started_at,r.finished_at,w.setup_status FROM test_runs r JOIN executions e ON e.id=r.execution_id
		JOIN worktrees w ON w.id=e.worktree_id WHERE r.execution_id=? AND r.kind='setup'`, run.ExecutionID).
		Scan(&previous.ID, &previous.ExecutionID, &previous.Kind, &previous.TargetSHA, &previous.Command, &previous.ExitCode,
			&previous.TimedOut, &started, &finished, &savedStatus)
	if err == nil {
		if previous.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return err
		}
		if previous.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
			return err
		}
		if !reflect.DeepEqual(run, previous) || savedStatus != status {
			return fmt.Errorf("store: setup measurement is immutable")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var command sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT p.setup_command FROM projects p JOIN tasks t ON t.project_id=p.id
		JOIN executions e ON e.task_id=t.id WHERE e.id=? AND e.status='preparing'`, run.ExecutionID).Scan(&command); err != nil {
		return err
	}
	if !command.Valid || command.String != run.Command {
		return fmt.Errorf("store: setup command differs from Registry")
	}
	if err := RecordTestRun(ctx, tx, run); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worktrees SET setup_status=? WHERE id=?`, status, worktreeID); err != nil {
		return err
	}
	return tx.Commit()
}
