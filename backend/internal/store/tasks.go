package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func GetTaskByIdempotencyKey(ctx context.Context, reader projectReader, key string) (model.Task, error) {
	var t model.Task
	err := reader.QueryRowContext(ctx, `SELECT id, project_id, title, body, acceptance_criteria,
		short_name, task_type, collaboration_mode, status, idempotency_key, created_at, updated_at
		FROM tasks WHERE idempotency_key = ?`, key).Scan(&t.ID, &t.ProjectID, &t.Title, &t.Body,
		&t.AcceptanceCriteria, &t.ShortName, &t.TaskType, &t.CollaborationMode, &t.Status,
		&t.IdempotencyKey, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// InsertTask allocates the next numeric task ID inside the store's serialized
// creation transaction. It must be committed with the initial Execution.
func InsertTask(ctx context.Context, tx *sql.Tx, t *model.Task) error {
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(CAST(substr(id, 6) AS INTEGER)), 0) + 1 FROM tasks`).Scan(&next); err != nil {
		return err
	}
	t.ID = fmt.Sprintf("task-%03d", next)
	_, err := tx.ExecContext(ctx, `INSERT INTO tasks
		(id, project_id, title, body, acceptance_criteria, short_name, task_type,
		collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, t.ProjectID, t.Title, t.Body,
		t.AcceptanceCriteria, t.ShortName, t.TaskType, t.CollaborationMode, t.Status,
		t.IdempotencyKey, t.CreatedAt, t.UpdatedAt)
	return err
}

func InsertInitialExecution(ctx context.Context, tx *sql.Tx, id, taskID, createdAt string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES (?, ?, 1, 'continue', 'pending', ?)`, id, taskID, createdAt)
	return err
}

// TaskSummary is one row of the owner task list: the task, its latest
// Execution (highest seq) and the newest PR URL reported by any Execution.
type TaskSummary struct {
	model.Task
	LatestExecution *ExecutionSummary
	PRURL           *string
}

type ExecutionSummary struct {
	ID         string
	Seq        int
	Mode       string
	Status     string
	ErrorClass *string
}

type TaskFilter struct {
	Status    string
	ProjectID string
}

// ListTasks returns tasks newest first. Task IDs are task-<number> with a
// minimum of three digits, so ordering and the exclusive cursor boundary use
// the numeric part (task-1000 follows task-999). before <= 0 starts at the top.
func ListTasks(ctx context.Context, db *sql.DB, filter TaskFilter, before int64, limit int) ([]TaskSummary, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id, t.project_id, t.title, t.body, t.acceptance_criteria,
		t.short_name, t.task_type, t.collaboration_mode, t.status, t.created_at, t.updated_at,
		e.id, e.seq, e.mode, e.status, e.error_class,
		(SELECT pr_url FROM executions WHERE task_id = t.id AND pr_url IS NOT NULL ORDER BY seq DESC LIMIT 1)
		FROM tasks t
		LEFT JOIN executions e ON e.task_id = t.id AND e.seq = (SELECT max(seq) FROM executions WHERE task_id = t.id)
		WHERE (? = '' OR t.status = ?) AND (? = '' OR t.project_id = ?)
		AND (? <= 0 OR CAST(substr(t.id, 6) AS INTEGER) < ?)
		ORDER BY CAST(substr(t.id, 6) AS INTEGER) DESC LIMIT ?`,
		filter.Status, filter.Status, filter.ProjectID, filter.ProjectID, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []TaskSummary{}
	for rows.Next() {
		var s TaskSummary
		var executionID, mode, status sql.NullString
		var seq sql.NullInt64
		var errorClass, prURL sql.NullString
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.Title, &s.Body, &s.AcceptanceCriteria,
			&s.ShortName, &s.TaskType, &s.CollaborationMode, &s.Status, &s.CreatedAt, &s.UpdatedAt,
			&executionID, &seq, &mode, &status, &errorClass, &prURL); err != nil {
			return nil, err
		}
		if executionID.Valid {
			s.LatestExecution = &ExecutionSummary{ID: executionID.String, Seq: int(seq.Int64), Mode: mode.String, Status: status.String}
			if errorClass.Valid {
				s.LatestExecution.ErrorClass = &errorClass.String
			}
		}
		if prURL.Valid {
			s.PRURL = &prURL.String
		}
		tasks = append(tasks, s)
	}
	return tasks, rows.Err()
}
