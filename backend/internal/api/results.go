package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// Status strings are validated as transitions, including unknown values (409).
// Other enum values are input validation errors (400).
type resultInput struct {
	FromStatus       string           `json:"fromStatus"`
	Status           string           `json:"status"`
	HarnessSessionID *string          `json:"harnessSessionId"`
	ErrorClass       model.ErrorClass `json:"errorClass"`
	ErrorMessage     *string          `json:"errorMessage"`
	Usage            *resultUsage     `json:"usage"`
}

// The result body is not specified in specs 01/04/05. These fields carry step
// 14's existing measurements without adding an endpoint or schema columns.
type resultUsage struct {
	WallTimeMS        *int64              `json:"wallTimeMs"`
	HarnessWallTimeMS *int64              `json:"harnessWallTimeMs"`
	HarnessUsage      *resultHarnessUsage `json:"harnessUsage"`
}

type resultHarnessUsage struct {
	InputTokens  *int64 `json:"inputTokens"`
	OutputTokens *int64 `json:"outputTokens"`
	WallTimeMS   int64  `json:"wallTimeMs"`
}

func reportExecutionResult(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		var input resultInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteError(w, logger, err)
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil) // BEGIN IMMEDIATE, as in claim.
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		execution, err := store.GetContextExecution(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		runner, _ := ctx.Value(runnerIdentityKey{}).(string)
		if runner == "" || execution.RunnerID != runner {
			tx.Rollback() // Release the connection before recording the denial.
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot report this Execution")
			return
		}
		if !execution.Latest || input.FromStatus != string(execution.Status) || !runnerResultSource(execution.Status) || !runnerResultTarget(input.Status) {
			WriteError(w, logger, &Error{Code: Conflict, Message: "Result does not match an active, latest Execution transition"})
			return
		}
		// Validate the transition before accepting any metadata or side effects.
		err = task.ValidateExecutionTransition(execution.Status, model.ExecutionStatus(input.Status))
		if err == nil {
			err = validateResult(r, tx, execution, input)
		}
		if err == nil {
			err = saveResult(r, tx, execution, input)
		}
		to, reason := model.ExecutionStatus(input.Status), "runner_result"
		if err == nil {
			err = task.TransitionExecutionTx(ctx, tx, task.Transition{
				TaskID: execution.TaskID, ExecutionID: id, FromTask: execution.TaskStatus,
				FromExecution: execution.Status, ToExecution: to,
			})
		}
		if err == nil {
			payload, _ := json.Marshal(map[string]string{"from": input.FromStatus, "to": string(to), "reason": reason})
			_, err = store.AppendExecutionEvent(ctx, tx, id, model.EventStatusChanged, payload)
		}
		if err == nil {
			_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
				Actor: "runner:" + runner, Action: "execution.result", TargetType: "execution", TargetID: id,
				Detail: map[string]any{"taskId": execution.TaskID, "from": input.FromStatus, "to": string(to)},
			})
		}
		if err == nil {
			err = tx.Commit()
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

func runnerResultSource(status model.ExecutionStatus) bool {
	switch status {
	case model.ExecutionClaimed, model.ExecutionPreparing, model.ExecutionRunning, model.ExecutionVerifying, model.ExecutionPublishing:
		return true
	}
	return false
}

func runnerResultTarget(status string) bool {
	switch model.ExecutionStatus(status) {
	case model.ExecutionPreparing, model.ExecutionRunning, model.ExecutionVerifying, model.ExecutionPublishing,
		model.ExecutionSucceeded, model.ExecutionFailed, model.ExecutionCancelled, model.ExecutionTimedOut:
		return true
	}
	return false
}

func validateResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if input.Status == "succeeded" {
		if input.Usage == nil || input.Usage.WallTimeMS == nil {
			return invalid("usage.wallTimeMs", "required")
		}
	} else if input.Usage != nil {
		return invalid("usage", "only_for_succeeded")
	}
	if usage := input.Usage; usage != nil {
		for _, value := range []*int64{usage.WallTimeMS, usage.HarnessWallTimeMS} {
			if value != nil && *value < 0 {
				return invalid("usage", "out_of_range")
			}
		}
		if harness := usage.HarnessUsage; harness != nil {
			if harness.WallTimeMS < 0 || harness.InputTokens != nil && *harness.InputTokens < 0 ||
				harness.OutputTokens != nil && *harness.OutputTokens < 0 {
				return invalid("usage.harnessUsage", "out_of_range")
			}
		}
	}
	if input.Status == "failed" && input.ErrorClass == "" {
		return invalid("errorClass", "required")
	}
	if input.ErrorClass != "" && input.Status != "failed" && input.Status != "cancelled" && input.Status != "timed_out" {
		return invalid("errorClass", "only_for_failure")
	}
	if input.Status == "timed_out" && input.ErrorClass != "" && input.ErrorClass != model.ErrorTimeout ||
		input.Status == "cancelled" && input.ErrorClass != "" && input.ErrorClass != model.ErrorCancelled {
		return invalid("errorClass", "status_mismatch")
	}
	return nil
}

func saveResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if usage := input.Usage; usage != nil {
		record := store.UsageRecord{
			ID: ulid.Make().String(), ExecutionID: execution.ID, WallTimeMS: *usage.WallTimeMS,
		}
		if usage.HarnessUsage != nil {
			record.InputTokens = usage.HarnessUsage.InputTokens
			record.OutputTokens = usage.HarnessUsage.OutputTokens
		}
		// The existing schema has a single unlabelled duration column. Store
		// total Execution time; retain both harness measurements at the input
		// boundary until specs define how to distinguish them in usage_records.
		if err := store.RecordUsage(r.Context(), tx, record); err != nil {
			return err
		}
	}
	// Identifiers and column names are fixed in code; values remain parameters.
	columns := []string{}
	values := []any{}
	for _, field := range []struct {
		column string
		value  *string
	}{{"harness_session_id", input.HarnessSessionID}, {"error_message", input.ErrorMessage}} {
		if field.value != nil {
			columns = append(columns, field.column+" = ?")
			values = append(values, *field.value)
		}
	}
	classification := input.ErrorClass
	if input.Status == "timed_out" {
		classification = model.ErrorTimeout
	} else if input.Status == "cancelled" {
		classification = model.ErrorCancelled
	}
	if classification != "" {
		columns = append(columns, "error_class = ?")
		values = append(values, classification)
	}
	if len(columns) > 0 {
		values = append(values, execution.ID)
		if _, err := tx.ExecContext(r.Context(), `UPDATE executions SET `+strings.Join(columns, ", ")+` WHERE id = ?`, values...); err != nil {
			return err
		}
	}
	return nil
}
