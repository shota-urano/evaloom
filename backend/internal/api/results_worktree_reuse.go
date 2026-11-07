package api

import (
	"database/sql"
	"net/http"
	"reflect"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Stage 3 records reuse before Context is pinned. The existing result fields
// identify an existing row; this does not register a new worktree or manifest.
// Identical reports are idempotent and never emit a new state transition.
func saveWorktreeReuseResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if input.FromStatus != "preparing" || input.Status != "preparing" {
		return &Error{Code: Conflict, Message: "Worktree reuse must be reported during preparation"}
	}
	expected := resultInput{FromStatus: input.FromStatus, Status: input.Status,
		WorktreeID: input.WorktreeID, BaseSHA: input.BaseSHA, BranchName: input.BranchName}
	if !reflect.DeepEqual(input, expected) || input.BranchName == nil {
		return invalid("worktreeId", "reuse_measurement_only")
	}
	if err := ValidateID("worktreeId", *input.WorktreeID, ULID); err != nil {
		return err
	}
	// All other validation still applies, including immutable base_sha. The
	// query below checks ownership and permits the cleaned predecessor only
	// here; ordinary progress/terminal reports cannot resurrect arbitrary rows.
	metadata := input
	metadata.WorktreeID = nil
	if err := validateResult(r, tx, execution, metadata); err != nil {
		return err
	}
	own, err := ownsPredecessorWorktree(r, tx, execution.ID, input)
	if err != nil {
		return err
	}
	if !own {
		return &Error{Code: PreconditionFailed, Message: "Reused worktree must match the predecessor's Task, Runner, branch and start SHA"}
	}
	ready, err := store.WorktreeFilesReady(r.Context(), tx, *input.WorktreeID)
	if err != nil {
		return err
	}
	if !ready {
		return &Error{Code: PreconditionFailed, Message: "Worktree preparation has no completed copy record"}
	}
	if err := saveResult(r, tx, execution, input); err != nil {
		return err
	}
	_, err = tx.ExecContext(r.Context(), `UPDATE worktrees SET status='active',cleaned_at=NULL,
		setup_status=CASE WHEN status='cleaned' THEN 'pending' ELSE setup_status END WHERE id=?`, *input.WorktreeID)
	return err
}

func ownsPredecessorWorktree(r *http.Request, tx *sql.Tx, executionID string, input resultInput) (bool, error) {
	if input.WorktreeID == nil || input.BaseSHA == nil || input.BranchName == nil {
		return false, nil
	}
	var own bool
	err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM executions e
		JOIN tasks t ON t.id=e.task_id
		JOIN executions p ON p.task_id=e.task_id AND p.seq=e.seq-1
		JOIN worktrees w ON w.id=p.worktree_id
		WHERE e.id=? AND e.mode='continue' AND w.id=? AND w.task_id=e.task_id
		AND w.project_id=t.project_id AND w.runner_id=e.runner_id
		AND w.base_sha=? AND w.branch_name=? AND p.base_sha=w.base_sha AND p.branch_name=w.branch_name
		AND (e.base_sha IS NULL OR e.base_sha=w.base_sha)
		AND (e.branch_name IS NULL OR e.branch_name=w.branch_name)
		AND (e.worktree_id IS NULL OR e.worktree_id=w.id))`,
		executionID, *input.WorktreeID, *input.BaseSHA, *input.BranchName).Scan(&own)
	return own, err
}
