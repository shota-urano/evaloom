package store

import (
	"context"
	"database/sql"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ContextRevision is one immutable file (or the task input) pinned to an Execution.
// Non-Git sources use an empty SourceCommitSHA; ContentHash always covers Content.
type ContextRevision struct {
	ID, ExecutionID, Layer, Kind, SourcePath, SourceCommitSHA, ContentHash, Content, CreatedAt string
}

// InsertContextRevision participates in the caller's pinning transaction.
func InsertContextRevision(ctx context.Context, tx *sql.Tx, revision ContextRevision) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO context_revisions
		(id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, revision.ID, revision.ExecutionID, revision.Layer,
		revision.Kind, revision.SourcePath, revision.SourceCommitSHA, revision.ContentHash, revision.Content, revision.CreatedAt)
	return err
}

func ListContextRevisions(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, executionID string) ([]ContextRevision, error) {
	rows, err := reader.QueryContext(ctx, `SELECT id, execution_id, layer, kind, source_path,
		source_commit_sha, content_hash, content, created_at FROM context_revisions
		WHERE execution_id = ? ORDER BY CASE layer WHEN 'global' THEN 0 WHEN 'harness' THEN 1
		WHEN 'project' THEN 2 ELSE 3 END, source_path, id`, executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []ContextRevision{}
	for rows.Next() {
		var revision ContextRevision
		if err := rows.Scan(&revision.ID, &revision.ExecutionID, &revision.Layer, &revision.Kind,
			&revision.SourcePath, &revision.SourceCommitSHA, &revision.ContentHash, &revision.Content, &revision.CreatedAt); err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// ContextExecution reads only saved Execution pins and its Task input, never the
// current project_harnesses assignment or a moving Profile tag.
type ContextExecution struct {
	ID, TaskID, RunnerID, BaseSHA, HarnessID, ProfileID, ProfileVersion, ProfileCommitSHA string
	Status                                                                                model.ExecutionStatus
	TaskStatus                                                                            model.TaskStatus
	Title, Body, AcceptanceCriteria                                                       string
	Latest                                                                                bool
}

func GetContextExecution(ctx context.Context, reader projectReader, id string) (ContextExecution, error) {
	var execution ContextExecution
	err := reader.QueryRowContext(ctx, `SELECT e.id, e.task_id, COALESCE(e.runner_id, ''),
		COALESCE(e.base_sha, ''), COALESCE(e.harness_id, ''), COALESCE(e.profile_id, ''),
		COALESCE(e.profile_version, ''), COALESCE(e.profile_commit_sha, ''), e.status, t.status,
		t.title, t.body, t.acceptance_criteria,
		e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id)
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(
		&execution.ID, &execution.TaskID, &execution.RunnerID, &execution.BaseSHA,
		&execution.HarnessID, &execution.ProfileID, &execution.ProfileVersion, &execution.ProfileCommitSHA,
		&execution.Status, &execution.TaskStatus, &execution.Title, &execution.Body, &execution.AcceptanceCriteria, &execution.Latest)
	return execution, err
}
