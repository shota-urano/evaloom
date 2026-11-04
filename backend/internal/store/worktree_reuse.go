package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// WorktreeReuseStore belongs to the Control Plane. Runner receives a delivery
// implementation of its history/copy interface, never this DB connection.
type WorktreeReuseStore struct{ DB *sql.DB }

func (s WorktreeReuseStore) WarnMissingCopy(ctx context.Context, executionID, path string) error {
	return (WorktreeCopyStore{DB: s.DB}).WarnMissingCopy(ctx, executionID, path)
}

const reusePredecessor = ` FROM executions e
	JOIN tasks t ON t.id=e.task_id
	JOIN executions p ON p.task_id=e.task_id AND p.seq=e.seq-1
	JOIN worktrees w ON w.id=p.worktree_id AND w.id=?
	WHERE e.id=? AND e.mode='continue' AND e.status='preparing'
	AND w.task_id=e.task_id AND w.project_id=t.project_id AND w.runner_id=e.runner_id
	AND p.base_sha=w.base_sha AND p.branch_name=w.branch_name
	AND NOT EXISTS(SELECT 1 FROM executions newer WHERE newer.task_id=e.task_id AND newer.seq>e.seq)`

func (s WorktreeReuseStore) LoadWorktreeReuse(ctx context.Context, executionID, worktreeID string) (model.WorktreeReuseState, error) {
	var state model.WorktreeReuseState
	err := s.DB.QueryRowContext(ctx, `SELECT w.status,
		coalesce(nullif(p.pushed_sha,''),nullif(p.harness_head_sha,''),p.base_sha)`+reusePredecessor,
		worktreeID, executionID).Scan(&state.Status, &state.ExpectedHeadSHA)
	return state, err
}

// BeginWorktreeRestore checkpoints a directory found missing by Runner. Keep
// it cleaned until copies are saved and the result API activates it. Thus a
// cancellation cannot make an unfinished new directory look ready on retry.
// This uses the same unspecified delivery boundary as RecordRestoredCopies.
func (s WorktreeReuseStore) BeginWorktreeRestore(ctx context.Context, executionID, worktreeID string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE worktrees SET status='cleaned',
		cleaned_at=coalesce(cleaned_at,?),setup_status='pending',copied_files='[]'
		WHERE id=? AND EXISTS(SELECT 1`+reusePredecessor+`)`,
		recordTime(), worktreeID, worktreeID, executionID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("store: restore worktree differs from predecessor")
	}
	return nil
}

// RecordRestoredCopies records the results of the existing copy stage after a
// missing directory was recreated. Its delivery shape is unspecified, like
// RecordCopiedWorktree. Association/activation still use the result API.
func (s WorktreeReuseStore) RecordRestoredCopies(ctx context.Context, executionID, worktreeID string, prepared model.PreparedWorktree) error {
	if prepared.CopiedFiles == nil {
		prepared.CopiedFiles = []model.CopiedFile{}
	}
	seen := make(map[string]bool)
	for _, file := range prepared.CopiedFiles {
		digest, err := hex.DecodeString(file.SHA256)
		if err != nil || len(digest) != 32 || !filepath.IsLocal(file.Path) || file.Path == "." || filepath.Clean(file.Path) != file.Path || seen[file.Path] {
			return fmt.Errorf("store: invalid restored copy manifest")
		}
		seen[file.Path] = true
	}
	manifest, err := json.Marshal(prepared.CopiedFiles)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var path, branch, base string
	err = tx.QueryRowContext(ctx, `SELECT w.path,w.branch_name,w.base_sha`+reusePredecessor,
		worktreeID, executionID).Scan(&path, &branch, &base)
	if err != nil {
		return err
	}
	if path != prepared.Path || branch != prepared.BranchName || base != prepared.BaseSHA {
		return fmt.Errorf("store: restored worktree differs from predecessor")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worktrees SET copied_files=?,setup_status='pending' WHERE id=?`, string(manifest), worktreeID); err != nil {
		return err
	}
	return tx.Commit()
}
