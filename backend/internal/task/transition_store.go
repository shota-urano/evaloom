package task

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Transition includes the caller's expectations, so a delayed report cannot
// change an Execution that has already moved to another state.
type Transition struct {
	TaskID, ExecutionID string
	FromTask            model.TaskStatus
	FromExecution       model.ExecutionStatus
	ToExecution         model.ExecutionStatus
}

// TransitionExecution atomically updates the latest Execution and its Task.
func TransitionExecution(ctx context.Context, db *sql.DB, change Transition) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := TransitionExecutionTx(ctx, tx, change); err != nil {
		return err
	}
	return tx.Commit()
}

// TransitionExecutionTx lets claim/result/cancel/resume handlers include their
// metadata, events and audit records in the same transaction. The caller must
// roll back the transaction on any error. It does not commit or notify.
func TransitionExecutionTx(ctx context.Context, tx *sql.Tx, change Transition) error {
	fromTask, err := TaskStatusForExecution(change.FromExecution)
	if err != nil {
		return err
	}
	if fromTask != change.FromTask {
		return conflict("task and execution state expectations do not match")
	}
	toTask, err := TaskStatusForExecution(change.ToExecution)
	if err != nil {
		return err
	}
	if fromTask != toTask {
		if err := ValidateTaskTransition(fromTask, toTask); err != nil {
			return err
		}
	}
	var resume sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT resume_point FROM executions
		WHERE id = ? AND task_id = ? AND status = ?
		AND seq = (SELECT max(seq) FROM executions WHERE task_id = ?)`,
		change.ExecutionID, change.TaskID, change.FromExecution, change.TaskID).Scan(&resume)
	if errors.Is(err, sql.ErrNoRows) {
		return conflict("execution is missing, stale, or not the latest execution")
	}
	if err != nil {
		return err
	}
	if err := ValidateExecutionTransition(change.FromExecution, change.ToExecution, resume.String); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE executions SET status = ?
		WHERE id = ? AND task_id = ? AND status = ?
		AND seq = (SELECT max(seq) FROM executions WHERE task_id = ?)`,
		change.ToExecution, change.ExecutionID, change.TaskID, change.FromExecution, change.TaskID)
	if err := expectOne(result, err); err != nil {
		return err
	}
	return updateTask(ctx, tx, change.TaskID, change.FromTask, toTask)
}

// QueueExecutionTx attaches an already inserted pending Execution to a received
// Task or a retriable Task. Insertion belongs to the create/retry API, which must
// use this same transaction and roll it back on any error. Previous Executions
// are kept unchanged; blocked resume uses TransitionExecutionTx instead.
func QueueExecutionTx(ctx context.Context, tx *sql.Tx, taskID, executionID string, from model.TaskStatus) error {
	if err := ValidateTaskTransition(from, model.TaskQueued); err != nil {
		return err
	}
	var seq int
	var status model.ExecutionStatus
	err := tx.QueryRowContext(ctx, `SELECT seq, status FROM executions
		WHERE id = ? AND task_id = ?
		AND seq = (SELECT max(seq) FROM executions WHERE task_id = ?)`,
		executionID, taskID, taskID).Scan(&seq, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return conflict("pending execution is missing or not the latest execution")
	}
	if err != nil {
		return err
	}
	if status != model.ExecutionPending {
		return conflict("new execution must be pending")
	}
	if from == model.TaskReceived {
		if seq != 1 {
			return conflict("initial execution must have seq 1")
		}
	} else {
		var previous model.ExecutionStatus
		err := tx.QueryRowContext(ctx, `SELECT status FROM executions WHERE task_id = ? AND seq = ?`, taskID, seq-1).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("retry requires the preceding execution")
		}
		if err != nil {
			return err
		}
		previousTask, err := TaskStatusForExecution(previous)
		if err != nil {
			return err
		}
		if previousTask != from {
			return conflict("previous execution does not match the task state")
		}
	}
	return updateTask(ctx, tx, taskID, from, model.TaskQueued)
}

func updateTask(ctx context.Context, tx *sql.Tx, id string, from, to model.TaskStatus) error {
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, time.Now().UTC().Format(time.RFC3339), id, from)
	return expectOne(result, err)
}

func expectOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return conflict("state changed before the transition could be applied")
	}
	return nil
}
