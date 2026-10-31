package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Resuming Gate 3 uses the original measurement, never a new test run or a
// predecessor Execution's verification. Baselines have separate reuse rules.
func findVerifyRun(ctx context.Context, tx *sql.Tx, executionID string) (*model.TestRun, error) {
	var run model.TestRun
	var started, finished string
	err := tx.QueryRowContext(ctx, `SELECT id, execution_id, kind, target_sha, command,
		exit_code, total, passed, failed, skipped, test_ids_artifact_id, timed_out, started_at, finished_at
		FROM test_runs WHERE execution_id=? AND kind='verify' ORDER BY started_at DESC, id DESC LIMIT 1`, executionID).Scan(
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

// RecordTestRun joins the result transition's transaction. The caller validates
// Registry command, artifact ownership, and measured commit before inserting.
func RecordTestRun(ctx context.Context, tx *sql.Tx, run model.TestRun) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO test_runs
		(id, execution_id, kind, target_sha, command, exit_code, total, passed, failed, skipped,
		test_ids_artifact_id, timed_out, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.ExecutionID, run.Kind, run.TargetSHA, run.Command, run.ExitCode, run.Total, run.Passed, run.Failed, run.Skipped,
		run.TestIDsArtifactID, run.TimedOut, run.StartedAt.UTC().Format(time.RFC3339Nano), run.FinishedAt.UTC().Format(time.RFC3339Nano))
	return err
}
