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
	WorktreeID       *string          `json:"worktreeId"`
	BranchName       *string          `json:"branchName"`
	BaseSHA          *string          `json:"baseSha"`
	HarnessHeadSHA   *string          `json:"harnessHeadSha"`
	VerifiedSHA      *string          `json:"verifiedSha"`
	PushedSHA        *string          `json:"pushedSha"`
	PRNumber         *int             `json:"prNumber"`
	PRURL            *string          `json:"prUrl"`
	HarnessSessionID *string          `json:"harnessSessionId"`
	ErrorClass       model.ErrorClass `json:"errorClass"`
	ErrorMessage     *string          `json:"errorMessage"`
	Block            *resultBlock     `json:"block"`
	Usage            *resultUsage     `json:"usage"`
	// Gate results are sent with the next transition report, because Gates
	// 2 and 3 run in verifying and a report must change the state.
	GateResults []resultGateResult `json:"gateResults"`
}

type resultGateResult struct {
	Gate      int             `json:"gate"`
	Result    string          `json:"result"`
	BaseSHA   string          `json:"baseSha"`
	TargetSHA string          `json:"targetSha"`
	Findings  []store.Finding `json:"findings"`
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

type resultBlock struct {
	Cause        model.BlockCause `json:"cause"`
	GateResultID *string          `json:"gateResultId"`
	HeadSHA      string           `json:"headSha"`
	Detail       map[string]any   `json:"detail"`
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
		var resume sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT resume_point FROM executions WHERE id = ?`, id).Scan(&resume)
		if err == nil {
			err = task.ValidateExecutionTransition(execution.Status, model.ExecutionStatus(input.Status), resume.String)
		}
		if err == nil {
			err = validateResult(r, tx, execution, input)
		}
		if err == nil {
			err = saveResult(r, tx, execution, input)
		}
		if err == nil {
			err = task.TransitionExecutionTx(ctx, tx, task.Transition{
				TaskID: execution.TaskID, ExecutionID: id, FromTask: execution.TaskStatus,
				FromExecution: execution.Status, ToExecution: model.ExecutionStatus(input.Status),
			})
		}
		if err == nil {
			payload, _ := json.Marshal(map[string]string{"from": input.FromStatus, "to": input.Status, "reason": "runner_result"})
			_, err = store.AppendExecutionEvent(ctx, tx, id, model.EventStatusChanged, payload)
		}
		if err == nil {
			_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
				Actor: "runner:" + runner, Action: "execution.result", TargetType: "execution", TargetID: id,
				Detail: map[string]any{"taskId": execution.TaskID, "from": input.FromStatus, "to": input.Status},
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
		model.ExecutionSucceeded, model.ExecutionBlocked, model.ExecutionFailed, model.ExecutionCancelled, model.ExecutionTimedOut:
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
	for _, field := range []struct {
		name string
		sha  *string
	}{{"baseSha", input.BaseSHA}, {"harnessHeadSha", input.HarnessHeadSHA}, {"verifiedSha", input.VerifiedSHA}, {"pushedSha", input.PushedSHA}} {
		if field.sha != nil && !fullCommitSHA(*field.sha) {
			return invalid(field.name, "invalid_format")
		}
	}
	// The worktree start commit is immutable once recorded (spec 06, 3.2).
	// Project Context is pinned to this SHA (spec 07, 3.2). This snapshot was
	// read under the same BEGIN IMMEDIATE transaction used to save the result.
	if input.BaseSHA != nil && execution.BaseSHA != "" && *input.BaseSHA != execution.BaseSHA {
		return &Error{Code: PreconditionFailed, Message: "Base SHA must equal the recorded start commit"}
	}
	if input.PRNumber != nil && *input.PRNumber < 1 {
		return invalid("prNumber", "out_of_range")
	}
	// Spec 02 requires the pushed commit to be the verified commit. Check
	// the effective values, including metadata saved by an earlier report.
	if input.PushedSHA != nil || input.VerifiedSHA != nil {
		var verified, pushed string
		if err := tx.QueryRowContext(r.Context(), `SELECT coalesce(?, verified_sha, ''), coalesce(?, pushed_sha, '')
			FROM executions WHERE id = ?`, input.VerifiedSHA, input.PushedSHA, execution.ID).Scan(&verified, &pushed); err != nil {
			return err
		}
		if pushed != "" && pushed != verified {
			return &Error{Code: PreconditionFailed, Message: "Pushed SHA must equal the verified SHA"}
		}
	}
	if input.Status == "blocked" {
		if input.Block == nil || input.Block.Cause.Validate() != nil || !fullCommitSHA(input.Block.HeadSHA) {
			return invalid("block", "cause_and_head_sha_required")
		}
	} else if input.Block != nil {
		return invalid("block", "only_for_blocked")
	}
	if err := validateGateResults(r, tx, execution, input); err != nil {
		return err
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
	if input.WorktreeID != nil {
		if err := ValidateID("worktreeId", *input.WorktreeID, ULID); err != nil {
			return err
		}
		var own bool
		err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM worktrees w JOIN tasks t ON t.id = w.task_id
			WHERE w.id = ? AND w.task_id = ? AND w.project_id = t.project_id AND w.runner_id = ? AND w.status != 'cleaned')`,
			*input.WorktreeID, execution.TaskID, execution.RunnerID).Scan(&own)
		if err != nil {
			return err
		}
		if !own {
			return &Error{Code: PreconditionFailed, Message: "Worktree does not belong to this Execution's Task and Runner"}
		}
	}
	if input.Block != nil && input.Block.GateResultID != nil {
		if err := ValidateID("block.gateResultId", *input.Block.GateResultID, ULID); err != nil {
			return err
		}
		var own bool
		err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM gate_results WHERE id = ? AND execution_id = ?)`, *input.Block.GateResultID, execution.ID).Scan(&own)
		if err != nil {
			return err
		}
		if !own {
			return &Error{Code: PreconditionFailed, Message: "Gate result does not belong to this Execution"}
		}
	}
	return nil
}

// blockGate maps a Gate Block cause to its Gate number; 0 for other causes.
func blockGate(cause model.BlockCause) int {
	switch cause {
	case model.BlockGate2MissingTests, model.BlockGate2Unverifiable:
		return 2
	case model.BlockGate3Mismatch:
		return 3
	}
	return 0
}

func validateGateResults(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	var stopping *resultGateResult
	for i, gate := range input.GateResults {
		if gate.Gate < 2 || gate.Gate > 3 {
			return invalid("gateResults.gate", "out_of_range")
		}
		switch gate.Result {
		case "pass":
		case "blocked", "failed":
			// A blocking or failing Gate must stop the Execution in the same
			// report, and only one Gate can be the one that stopped it.
			if input.Status != gate.Result || stopping != nil {
				return invalid("gateResults.result", "status_mismatch")
			}
			stopping = &input.GateResults[i]
		default:
			return invalid("gateResults.result", "invalid_value")
		}
		if !fullCommitSHA(gate.BaseSHA) || !fullCommitSHA(gate.TargetSHA) {
			return invalid("gateResults", "invalid_sha")
		}
		for _, finding := range gate.Findings {
			if finding.Rule == "" || finding.Line < 0 {
				return invalid("gateResults.findings", "invalid_format")
			}
		}
	}
	if input.Block != nil {
		cause := blockGate(input.Block.Cause)
		if stopping != nil && (input.Block.GateResultID != nil || stopping.Gate != cause) {
			return invalid("block", "gate_mismatch")
		}
		if input.Block.GateResultID != nil {
			var gate int
			err := tx.QueryRowContext(r.Context(), `SELECT gate FROM gate_results WHERE id = ? AND execution_id = ?`,
				*input.Block.GateResultID, execution.ID).Scan(&gate)
			if err == nil && gate != cause {
				return &Error{Code: PreconditionFailed, Message: "Gate result does not match the block cause"}
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	if len(input.GateResults) == 0 {
		return nil
	}
	// Compare the effective harness head, including this report's value.
	var head string
	if err := tx.QueryRowContext(r.Context(), `SELECT coalesce(?, harness_head_sha, '')
		FROM executions WHERE id = ?`, input.HarnessHeadSHA, execution.ID).Scan(&head); err != nil {
		return err
	}
	for _, gate := range input.GateResults {
		if head == "" || gate.TargetSHA != head {
			return &Error{Code: PreconditionFailed, Message: "Gate result target SHA must equal the Execution's harness head SHA"}
		}
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
	}{{"worktree_id", input.WorktreeID}, {"branch_name", input.BranchName}, {"base_sha", input.BaseSHA},
		{"harness_head_sha", input.HarnessHeadSHA}, {"verified_sha", input.VerifiedSHA},
		{"pushed_sha", input.PushedSHA}, {"pr_url", input.PRURL}, {"harness_session_id", input.HarnessSessionID}, {"error_message", input.ErrorMessage}} {
		if field.value != nil {
			columns = append(columns, field.column+" = ?")
			values = append(values, *field.value)
		}
	}
	if input.PRNumber != nil {
		columns = append(columns, "pr_number = ?")
		values = append(values, *input.PRNumber)
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
	var blockingGate *string
	for _, gate := range input.GateResults {
		id := ulid.Make().String()
		if err := store.RecordGateResult(r.Context(), tx, store.GateResult{
			ID: id, ExecutionID: execution.ID, Gate: gate.Gate, Result: gate.Result,
			BaseSHA: gate.BaseSHA, TargetSHA: gate.TargetSHA, Findings: gate.Findings,
		}); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"gate": gate.Gate, "result": gate.Result, "findingCount": len(gate.Findings)})
		if _, err := store.AppendExecutionEvent(r.Context(), tx, execution.ID, model.EventGateResult, payload); err != nil {
			return err
		}
		if gate.Result == "blocked" {
			blockingGate = &id
		}
	}
	if input.Block != nil {
		reference := input.Block.GateResultID
		if reference == nil {
			// Link the Block to the Gate that caused it in this report.
			reference = blockingGate
		}
		return store.SaveExecutionBlockTx(r.Context(), tx, store.ExecutionBlock{
			ExecutionID: execution.ID, Cause: string(input.Block.Cause), GateResultID: reference,
			HeadSHA: input.Block.HeadSHA, Detail: input.Block.Detail,
		})
	}
	return nil
}
