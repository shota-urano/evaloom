package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
	AND (e.worktree_id IS NULL OR e.worktree_id=w.id)
	AND (e.base_sha IS NULL OR e.base_sha=w.base_sha)
	AND (e.branch_name IS NULL OR e.branch_name=w.branch_name)
	AND NOT EXISTS(SELECT 1 FROM executions newer WHERE newer.task_id=e.task_id AND newer.seq>e.seq)`

func (s WorktreeReuseStore) LoadWorktreeReuse(ctx context.Context, executionID, worktreeID string) (model.WorktreeReuseState, error) {
	var state model.WorktreeReuseState
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	var setup, command string
	err = tx.QueryRowContext(ctx, `SELECT w.status,
		coalesce(nullif(p.pushed_sha,''),nullif(p.harness_head_sha,''),p.base_sha),w.setup_status,
		coalesce((SELECT setup_command FROM projects WHERE id=w.project_id),'')`+reusePredecessor,
		worktreeID, executionID).Scan(&state.Status, &state.ExpectedHeadSHA, &setup, &command)
	if err != nil {
		return state, err
	}
	ready, err := WorktreeFilesReady(ctx, tx, worktreeID)
	if err != nil {
		return state, err
	}
	setupReady := setup == "succeeded"
	if setup == "pending" && strings.TrimSpace(command) == "" {
		// Today's empty command cannot prove that setup was unnecessary when
		// this directory was prepared. Require that generation's skip record.
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM executions h
			JOIN execution_events skip ON skip.execution_id=h.id AND skip.type='log'
			WHERE h.worktree_id=? AND json_extract(skip.payload,'$.sourceId')=?
			AND NOT EXISTS(SELECT 1 FROM executions r JOIN execution_events start ON start.execution_id=r.id
				WHERE r.task_id=h.task_id AND start.type='log' AND json_extract(start.payload,'$.sourceId')=?
				AND (r.seq>h.seq OR (r.seq=h.seq AND start.id>skip.id))))`,
			worktreeID, setupSkippedPrefix+worktreeID, restoreSourcePrefix+worktreeID).Scan(&setupReady)
	}
	state.PreparationComplete = ready && setupReady
	return state, err
}

const restoreSourcePrefix = "worktree-restore/"
const setupSkippedPrefix = "worktree-setup-not-required/"

// WorktreeFilesReady uses existing events as a generation-specific preparation
// record. An older readiness event cannot validate a newer unfinished restore,
// even if its buffered event arrives later. Setup completion is checked apart.
func WorktreeFilesReady(ctx context.Context, tx *sql.Tx, worktreeID string) (bool, error) {
	var ready bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worktrees w
		JOIN executions h ON h.worktree_id=w.id AND h.task_id=w.task_id AND h.runner_id=w.runner_id
		JOIN execution_events ready ON ready.execution_id=h.id AND ready.type='worktree_ready'
		WHERE w.id=? AND h.base_sha=w.base_sha AND h.branch_name=w.branch_name
		AND json_extract(ready.payload,'$.path')=w.path
		AND json_extract(ready.payload,'$.branch')=w.branch_name
		AND json_extract(ready.payload,'$.baseSha')=w.base_sha
		AND NOT EXISTS(SELECT 1 FROM execution_events start JOIN executions r ON r.id=start.execution_id
			WHERE r.task_id=w.task_id AND r.runner_id=w.runner_id AND start.type='log'
			AND json_extract(start.payload,'$.sourceId')=? || w.id
			AND (r.seq>h.seq OR (r.seq=h.seq AND start.id>ready.id))))`, worktreeID, restoreSourcePrefix).Scan(&ready)
	return ready, err
}

func recordWorktreeReady(ctx context.Context, tx *sql.Tx, executionID string, prepared model.PreparedWorktree) error {
	payload, err := json.Marshal(map[string]string{"path": prepared.Path, "branch": prepared.BranchName, "baseSha": prepared.BaseSHA})
	if err == nil {
		_, err = AppendExecutionEvent(ctx, tx, executionID, model.EventWorktreeReady, payload)
	}
	if err != nil {
		return err
	}
	var command, worktreeID string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(p.setup_command,''),e.worktree_id
		FROM executions e JOIN tasks t ON t.id=e.task_id JOIN projects p ON p.id=t.project_id
		WHERE e.id=?`, executionID).Scan(&command, &worktreeID); err != nil {
		return err
	}
	if strings.TrimSpace(command) != "" {
		return nil
	}
	payload, _ = json.Marshal(map[string]string{"sourceId": setupSkippedPrefix + worktreeID,
		"stream": "runner", "text": "セットアップコマンド未設定のため実行不要"})
	_, err = AppendExecutionEvent(ctx, tx, executionID, model.EventLog, payload)
	return err
}

// BeginWorktreeRestore invalidates the previous generation BEFORE Git changes
// anything. The log uses spec 02's existing sourceId/stream/text fields, not a
// new event type or a sentinel in copied_files/setup_status. Delivery retries
// must never erase a completed manifest or create a second restore generation.
func (s WorktreeReuseStore) BeginWorktreeRestore(ctx context.Context, executionID, worktreeID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var branch, base, head string
	err = tx.QueryRowContext(ctx, `SELECT w.branch_name,w.base_sha,
		coalesce(nullif(p.pushed_sha,''),nullif(p.harness_head_sha,''),p.base_sha)`+reusePredecessor,
		worktreeID, executionID).Scan(&branch, &base, &head)
	if err != nil {
		return err
	}
	var started bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM execution_events
		WHERE execution_id=? AND type='log' AND json_extract(payload,'$.sourceId')=?)`,
		executionID, restoreSourcePrefix+worktreeID).Scan(&started); err != nil {
		return err
	}
	if started {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE executions SET worktree_id=?,base_sha=?,branch_name=?,harness_head_sha=? WHERE id=?`,
		worktreeID, base, branch, head, executionID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE worktrees SET status='cleaned',
		cleaned_at=coalesce(cleaned_at,?),setup_status='pending',copied_files='[]'
		WHERE id=?`, recordTime(), worktreeID)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"sourceId": restoreSourcePrefix + worktreeID,
		"stream": "runner", "text": "worktreeの復元を開始。コピーとセットアップの完了は未確認"})
	if _, err := AppendExecutionEvent(ctx, tx, executionID, model.EventLog, payload); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordRestoredCopies records the results of the existing copy stage after a
// missing directory was recreated. Its delivery shape is unspecified, like
// RecordCopiedWorktree. Activation still uses the result API.
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
	var path, branch, base, savedManifest, status, setup string
	err = tx.QueryRowContext(ctx, `SELECT w.path,w.branch_name,w.base_sha,w.copied_files,w.status,w.setup_status`+reusePredecessor+`
		AND EXISTS(SELECT 1 FROM execution_events start WHERE start.execution_id=e.id AND start.type='log'
			AND json_extract(start.payload,'$.sourceId')=?)`,
		worktreeID, executionID, restoreSourcePrefix+worktreeID).Scan(&path, &branch, &base, &savedManifest, &status, &setup)
	if err != nil {
		return err
	}
	if path != prepared.Path || branch != prepared.BranchName || base != prepared.BaseSHA {
		return fmt.Errorf("store: restored worktree differs from predecessor")
	}
	ready, err := WorktreeFilesReady(ctx, tx, worktreeID)
	if err != nil {
		return err
	}
	if ready {
		if savedManifest != string(manifest) {
			return fmt.Errorf("store: restored copy manifest is immutable")
		}
		return tx.Commit()
	}
	if status != "cleaned" || setup != "pending" {
		return fmt.Errorf("store: restore preparation is not in progress")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worktrees SET copied_files=?,setup_status='pending' WHERE id=?`, string(manifest), worktreeID); err != nil {
		return err
	}
	if err := recordWorktreeReady(ctx, tx, executionID, prepared); err != nil {
		return err
	}
	return tx.Commit()
}
