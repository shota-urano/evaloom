package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// WorktreeCleanupStore is Control Plane persistence. Runner must use a delivery
// implementation, never open this DB. No cleanup trigger/wire shape is defined.
type WorktreeCleanupStore struct{ DB *sql.DB }

// WithWorktreeCleanup holds BEGIN IMMEDIATE until removal and recording finish,
// excluding retry/claim/resume and other lifecycle writes. The callback must
// check the PR and remove on Runner, without re-entering the Control Plane DB.
// Delivery must preserve this exclusion through callback completion, including
// cancellation. Filesystem deletion cannot be rolled back: a failed commit can
// leave an absent directory with the old status; a retry must tolerate that.
func (s WorktreeCleanupStore) WithWorktreeCleanup(ctx context.Context, runnerID, worktreeID string,
	remove func(context.Context, model.WorktreeCleanupTarget) error) error {
	if runnerID == "" || worktreeID == "" || remove == nil {
		return fmt.Errorf("store: cleanup identity and removal callback are required")
	}
	// database/sql rolls back a BeginTx(ctx) as soon as ctx is cancelled,
	// potentially while Runner is still stopping Git. Keep the reservation
	// until the callback has returned; acquisition and every operation still
	// observe the request context.
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var target model.WorktreeCleanupTarget
	var status string
	err = tx.QueryRowContext(ctx, `SELECT w.project_id,p.git_remote_url,w.path,w.branch_name,w.status,
		(SELECT e.id FROM executions e WHERE e.worktree_id=w.id AND e.task_id=w.task_id
			AND e.runner_id=w.runner_id ORDER BY e.seq DESC LIMIT 1),
		coalesce((SELECT e.pr_number FROM executions e WHERE e.worktree_id=w.id AND e.task_id=w.task_id
			AND e.runner_id=w.runner_id AND e.branch_name=w.branch_name AND e.base_sha=w.base_sha
			AND e.pr_number>0 ORDER BY e.seq DESC LIMIT 1),0)
		FROM worktrees w JOIN tasks t ON t.id=w.task_id AND t.project_id=w.project_id
		JOIN projects p ON p.id=w.project_id
		WHERE w.id=? AND w.runner_id=?
		AND t.status IN ('completed','failed','cancelled','timed_out')
		AND (SELECT e.status FROM executions e WHERE e.task_id=t.id ORDER BY e.seq DESC LIMIT 1)
			IN ('succeeded','failed','interrupted','cancelled','timed_out')
		AND NOT EXISTS(SELECT 1 FROM executions e JOIN tasks busy ON busy.id=e.task_id
			WHERE busy.project_id=w.project_id AND e.status IN `+activeExecutionStatuses+`)`,
		worktreeID, runnerID).Scan(&target.ProjectID, &target.RepositoryURL, &target.Path, &target.BranchName,
		&status, &target.ExecutionID, &target.PRNumber)
	if err != nil {
		return err
	}
	if status == "cleaned" {
		return tx.Commit()
	}
	if target.PRNumber <= 0 {
		return fmt.Errorf("store: cleanup requires a recorded pull request")
	}
	if err := remove(ctx, target); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worktrees SET status='cleaned',cleaned_at=? WHERE id=?`, recordTime(), worktreeID); err != nil {
		return err
	}
	return tx.Commit()
}
