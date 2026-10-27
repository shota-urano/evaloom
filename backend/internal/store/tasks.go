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
