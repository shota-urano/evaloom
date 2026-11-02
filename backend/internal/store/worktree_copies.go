package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// WorktreeCopyStore is Control Plane persistence, never a Runner DB connection.
// Delivery from Runner remains an interface until the wire contract is defined.
type WorktreeCopyStore struct{ DB *sql.DB }

// RecordCopiedWorktree atomically saves the copy manifest, creates the worktree
// row, and associates it with the preparing Execution. An identical retry is
// idempotent; a saved manifest cannot be replaced through this entry point.
func (s WorktreeCopyStore) RecordCopiedWorktree(ctx context.Context, executionID string, prepared model.PreparedWorktree) (string, error) {
	if !filepath.IsAbs(prepared.Path) || prepared.BranchName == "" || prepared.BaseSHA == "" {
		return "", fmt.Errorf("store: invalid prepared worktree")
	}
	if prepared.CopiedFiles == nil {
		prepared.CopiedFiles = []model.CopiedFile{}
	}
	seen := make(map[string]bool)
	for _, file := range prepared.CopiedFiles {
		digest, err := hex.DecodeString(file.SHA256)
		if err != nil || len(digest) != 32 || !filepath.IsLocal(file.Path) || file.Path == "." || seen[file.Path] {
			return "", fmt.Errorf("store: invalid copied file manifest")
		}
		seen[file.Path] = true
	}
	copied, err := json.Marshal(prepared.CopiedFiles)
	if err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var projectID, taskID, runnerID string
	var worktreeID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT t.project_id,e.task_id,e.runner_id,e.worktree_id
		FROM executions e JOIN tasks t ON t.id=e.task_id
		WHERE e.id=? AND e.status='preparing' AND e.base_sha=?`, executionID, prepared.BaseSHA).
		Scan(&projectID, &taskID, &runnerID, &worktreeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: preparing execution with recorded start SHA is required")
	}
	if err != nil {
		return "", err
	}
	if worktreeID.Valid {
		var previous model.PreparedWorktree
		var manifest string
		err := tx.QueryRowContext(ctx, `SELECT path,branch_name,base_sha,copied_files FROM worktrees
			WHERE id=? AND project_id=? AND task_id=? AND runner_id=? AND status='active'`,
			worktreeID.String, projectID, taskID, runnerID).
			Scan(&previous.Path, &previous.BranchName, &previous.BaseSHA, &manifest)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal([]byte(manifest), &previous.CopiedFiles); err != nil {
			return "", err
		}
		if !reflect.DeepEqual(previous, prepared) {
			return "", fmt.Errorf("store: recorded copied worktree is immutable")
		}
		return worktreeID.String, tx.Commit()
	}
	id := ulid.Make().String()
	_, err = tx.ExecContext(ctx, `INSERT INTO worktrees
		(id,project_id,task_id,runner_id,path,branch_name,base_sha,status,setup_status,copied_files,created_at)
		VALUES (?,?,?,?,?,?,?,'active','pending',?,?)`,
		id, projectID, taskID, runnerID, prepared.Path, prepared.BranchName, prepared.BaseSHA, string(copied), recordTime())
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE executions SET worktree_id=?,branch_name=? WHERE id=?`, id, prepared.BranchName, executionID); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// WarnMissingCopy records the required log warning without file contents or an
// absolute owner path. Quote the relative path so control characters are inert.
func (s WorktreeCopyStore) WarnMissingCopy(ctx context.Context, executionID, path string) error {
	payload, err := json.Marshal(struct {
		Stream string `json:"stream"`
		Text   string `json:"text"`
	}{"stderr", "警告: コピー元が無いためスキップ: " + strconv.Quote(path)})
	if err != nil {
		return err
	}
	_, err = AppendExecutionEvent(ctx, s.DB, executionID, model.EventLog, payload)
	return err
}
