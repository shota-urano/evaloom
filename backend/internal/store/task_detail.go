package store

import (
	"context"
	"database/sql"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

type TaskDetail struct {
	model.Task
	Executions []TaskExecution
}

type TaskExecution struct {
	ExecutionSummary
	ProfileVersion   *string
	ProfileCommitSHA *string
	StartedAt        *string
	FinishedAt       *string
	PRURL            *string
}

// GetTaskDetail reads the task and its complete history in a single SQLite
// snapshot, so a concurrent retry cannot mix task state with older history.
func GetTaskDetail(ctx context.Context, db *sql.DB, id string) (TaskDetail, error) {
	result := TaskDetail{Executions: []TaskExecution{}}
	rows, err := db.QueryContext(ctx, `SELECT t.id, t.project_id, t.title, t.body, t.acceptance_criteria,
		t.short_name, t.task_type, t.collaboration_mode, t.status, t.created_at, t.updated_at,
		e.id, e.seq, e.mode, e.status, e.error_class, e.profile_version, e.profile_commit_sha,
		e.started_at, e.finished_at, e.pr_url
		FROM tasks t LEFT JOIN executions e ON e.task_id = t.id
		WHERE t.id = ? ORDER BY e.seq ASC`, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var e TaskExecution
		var executionID, mode, status sql.NullString
		var seq sql.NullInt64
		if err := rows.Scan(&result.ID, &result.ProjectID, &result.Title, &result.Body, &result.AcceptanceCriteria,
			&result.ShortName, &result.TaskType, &result.CollaborationMode, &result.Status, &result.CreatedAt, &result.UpdatedAt,
			&executionID, &seq, &mode, &status, &e.ErrorClass, &e.ProfileVersion, &e.ProfileCommitSHA,
			&e.StartedAt, &e.FinishedAt, &e.PRURL); err != nil {
			return result, err
		}
		if executionID.Valid {
			e.ID, e.Seq, e.Mode, e.Status = executionID.String, int(seq.Int64), mode.String, status.String
			result.Executions = append(result.Executions, e)
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if result.ID == "" {
		return result, sql.ErrNoRows
	}
	return result, nil
}
