package store

import (
	"context"
	"database/sql"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ContextExecution reads only saved Execution pins and its Task input, never the
// current Project pin or a moving Profile tag.
type ContextExecution struct {
	ID, TaskID, RunnerID, HarnessID, ProfileID, ProfileVersion, ProfileCommitSHA string
	Status                                                                       model.ExecutionStatus
	TaskStatus                                                                   model.TaskStatus
	Title, Body, AcceptanceCriteria                                              string
	Latest                                                                       bool
}

// StopContextTx stops before invoking the work adapter. The caller rolls back
// unfinished context pins and commits this transition with its events/decision.
func StopContextTx(ctx context.Context, tx *sql.Tx, id, message string, conflict bool) error {
	e, err := readReportExecution(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := e.checkTask(); err != nil {
		return err
	}
	status := model.StageFailed
	var class any = model.ErrorConfig
	if conflict {
		status, class = model.StageBlocked, nil
		if err := e.move(ctx, tx, model.ExecutionBlocked, "", string(model.CauseContextConflict)); err != nil {
			return err
		}
		if _, err := openDecision(ctx, tx, e, model.DecisionBlocked, model.CauseContextConflict, nil, nil, map[string]string{"message": message}); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class=?,error_message=? WHERE id=?`, model.ErrorConfig, message, id); err != nil {
			return err
		}
		if err := e.move(ctx, tx, model.ExecutionFailed, "", string(model.ErrorConfig)); err != nil {
			return err
		}
	}
	// A Runner may already have reported stage_started while preparing its
	// work environment. No micro-harness is allowed to run after this stop.
	_, err = tx.ExecContext(ctx, `UPDATE stage_runs SET status=?,error_class=?,finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE execution_id=? AND stage='work' AND status='running'`, status, class, id)
	return err
}

func GetContextExecution(ctx context.Context, reader projectReader, id string) (ContextExecution, error) {
	var execution ContextExecution
	err := reader.QueryRowContext(ctx, `SELECT e.id, e.task_id, COALESCE(e.runner_id, ''),
		COALESCE(e.harness_id, ''), COALESCE(e.profile_id, ''),
		COALESCE(e.profile_version, ''), COALESCE(e.profile_commit_sha, ''), e.status, t.status,
		t.title, t.body, t.acceptance_criteria,
		e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id)
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(
		&execution.ID, &execution.TaskID, &execution.RunnerID,
		&execution.HarnessID, &execution.ProfileID, &execution.ProfileVersion, &execution.ProfileCommitSHA,
		&execution.Status, &execution.TaskStatus, &execution.Title, &execution.Body, &execution.AcceptanceCriteria, &execution.Latest)
	return execution, err
}
