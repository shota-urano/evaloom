package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

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
