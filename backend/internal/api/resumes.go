package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

type resumeInput struct {
	BlockID  string `json:"blockId"`
	Decision string `json:"decision"`
	HeadSHA  string `json:"headSha"`
}

func resumeTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := ValidateID("id", id, TaskID)
		var input resumeInput
		if err == nil {
			err = DecodeJSON(r, &input)
		}
		if err == nil {
			err = ValidateID("blockId", input.BlockID, ULID)
		}
		if err == nil {
			switch input.Decision {
			case "continue", "abort":
			case "":
				err = invalid("decision", "required")
			default:
				err = invalid("decision", "invalid")
			}
		}
		if err == nil && input.HeadSHA == "" {
			err = invalid("headSha", "required")
		}
		if err == nil && !fullCommitSHA(input.HeadSHA) {
			err = invalid("headSha", "invalid_format")
		}
		if err == nil {
			err = saveBlockDecision(r, db, id, input)
		}
		if errors.Is(err, task.ErrConflict) {
			err = &Error{Code: Conflict, Message: err.Error()}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func saveBlockDecision(r *http.Request, db *sql.DB, id string, input resumeInput) error {
	ctx := r.Context()
	tx, err := db.BeginTx(ctx, nil) // BEGIN IMMEDIATE serializes resume/cancel/retry/claim.
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status model.TaskStatus
	err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Task not found"}
	}
	if err != nil {
		return err
	}
	var executionID, headSHA string
	var cause model.BlockCause
	var decision sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT b.execution_id, b.cause, b.head_sha, b.decision
		FROM execution_blocks b JOIN executions e ON e.id = b.execution_id
		WHERE b.id = ? AND e.task_id = ?`, input.BlockID, id).
		Scan(&executionID, &cause, &headSHA, &decision)
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Block not found for Task"}
	}
	if err != nil {
		return err
	}
	execution, err := store.GetContextExecution(ctx, tx, executionID)
	if err != nil {
		return err
	}
	if status != model.TaskBlocked || execution.Status != model.ExecutionBlocked || !execution.Latest || decision.Valid {
		return &Error{Code: Conflict, Message: "Decision requires an undecided Block on the latest blocked Execution"}
	}
	if input.HeadSHA != headSHA {
		return &Error{Code: PreconditionFailed, Message: "Head SHA must equal the Block's recorded SHA"}
	}
	var resumePoint string
	to := model.ExecutionCancelled
	if input.Decision == "continue" {
		// Spec 10 section 3.5: Gate 3 must be checked again, never skipped.
		switch cause {
		case model.BlockGate1TestChange, model.BlockGate1Unverifiable:
			resumePoint = "gate2"
		case model.BlockGate2MissingTests, model.BlockGate2Unverifiable, model.BlockGate3Mismatch:
			resumePoint = "gate3"
		default:
			return &Error{Code: PreconditionFailed, Message: "Block cause does not allow continuation"}
		}
		to = model.ExecutionPending
		_, err = tx.ExecContext(ctx, `UPDATE executions SET resume_point = ? WHERE id = ? AND status = 'blocked'`, resumePoint, executionID)
	} else {
		// Share cancellation metadata, finish time and worktree retention effects.
		err = saveResult(r, tx, execution, resultInput{Status: string(to)})
	}
	if err == nil {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE execution_blocks SET decision = ?, decided_at = ?
			WHERE id = ? AND execution_id = ? AND head_sha = ? AND decision IS NULL`,
			input.Decision, time.Now().UTC().Format(time.RFC3339Nano), input.BlockID, executionID, input.HeadSHA)
		if err == nil {
			var n int64
			n, err = result.RowsAffected()
			if err == nil && n != 1 {
				err = &Error{Code: Conflict, Message: "Block decision changed before it could be recorded"}
			}
		}
	}
	if err == nil {
		err = task.TransitionExecutionTx(ctx, tx, task.Transition{
			TaskID: id, ExecutionID: executionID, FromTask: status,
			FromExecution: model.ExecutionBlocked, ToExecution: to,
		})
	}
	if err == nil {
		payload, _ := json.Marshal(map[string]string{"from": "blocked", "to": string(to), "reason": "block_" + input.Decision})
		_, err = store.AppendExecutionEvent(ctx, tx, executionID, model.EventStatusChanged, payload)
	}
	if err == nil {
		toTask, _ := task.TaskStatusForExecution(to)
		_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
			Actor: "owner", Action: "task.resume", TargetType: "task", TargetID: id,
			Detail: map[string]any{"executionId": executionID, "blockId": input.BlockID, "decision": input.Decision,
				"headSha": input.HeadSHA, "from": status, "to": toTask},
		})
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
