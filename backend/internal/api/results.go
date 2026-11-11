package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// maxResultBody bounds one stage report. A result JSON carries at most 500
// findings of 1000 characters and a 4000-character summary (contract 6.5).
const maxResultBody = 4 << 20

// stageStartedInput is the start of a stage (spec 04 3.4.1).
type stageStartedInput struct {
	Type              string      `json:"type"`
	Stage             model.Stage `json:"stage"`
	RequestArtifactID string      `json:"requestArtifactId"`
}

// stageFinishedInput is the end of a stage (spec 04 3.4.1). Every field is
// required; nullable fields must be present as null.
type stageFinishedInput struct {
	Type             string           `json:"type"`
	Stage            model.Stage      `json:"stage"`
	Attempt          *int             `json:"attempt"`
	ExitCode         *int             `json:"exitCode"`
	RunnerError      *string          `json:"runnerError"`
	Result           json.RawMessage  `json:"result"`
	ResultArtifactID *string          `json:"resultArtifactId"`
	Work             *stageWorkInput  `json:"work"`
	Usage            *stageUsageInput `json:"usage"`
}

type stageWorkInput struct {
	SessionID         *string         `json:"sessionId"`
	SummaryArtifactID *string         `json:"summaryArtifactId"`
	Question          json.RawMessage `json:"question"`
}

type stageUsageInput struct {
	WallTimeMS   *int64   `json:"wallTimeMs"`
	InputTokens  *int64   `json:"inputTokens"`
	OutputTokens *int64   `json:"outputTokens"`
	CostAmount   *float64 `json:"costAmount"`
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

func reportExecutionResult(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResultBody))
		if err != nil {
			WriteError(w, logger, invalid("body", "invalid_json"))
			return
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			WriteError(w, logger, invalid("body", "invalid_json"))
			return
		}
		var started stageStartedInput
		var finished stageFinishedInput
		var reportHash string
		switch head.Type {
		case "stage_started":
			err = decodeReport(data, &started)
			if err == nil {
				err = validateStarted(started)
			}
		case "stage_finished":
			err = decodeReport(data, &finished)
			if err == nil {
				err = validateFinished(finished)
			}
			if err == nil {
				reportHash, err = canonicalHash(data)
			}
		default:
			err = invalid("type", "invalid")
		}
		if err != nil {
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
		var response any
		if head.Type == "stage_started" {
			var attempt int
			attempt, err = store.StartStageTx(ctx, tx, store.StageStart{
				ExecutionID: id, RunnerID: runner, Stage: started.Stage, RequestArtifactID: started.RequestArtifactID,
			})
			response = map[string]int{"attempt": attempt}
		} else {
			response, err = store.FinishStageTx(ctx, tx, finishedReport(id, runner, reportHash, finished))
		}
		if err == nil {
			err = tx.Commit()
		}
		var invalidReport *store.StageReportInvalid
		switch {
		case errors.As(err, &invalidReport):
			err = invalid(invalidReport.Field, invalidReport.Reason)
		case errors.Is(err, task.ErrConflict):
			err = &Error{Code: Conflict, Message: err.Error()}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// decodeReport rejects unknown fields, including the removed development
// items (spec 19 3.11), and missing required fields.
func decodeReport(data []byte, dst any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return invalid("body", "invalid_json")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return invalid("body", "invalid_json")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid("body", "invalid_json")
	}
	var required []string
	switch dst.(type) {
	case *stageStartedInput:
		required = []string{"stage", "requestArtifactId"}
	case *stageFinishedInput:
		required = []string{"stage", "attempt", "exitCode", "runnerError", "result", "resultArtifactId", "work", "usage"}
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return invalid(field, "required")
		}
	}
	return nil
}

func validateStarted(in stageStartedInput) error {
	if err := in.Stage.Validate(); err != nil {
		return invalid("stage", "invalid")
	}
	if in.RequestArtifactID == "" {
		return invalid("requestArtifactId", "required")
	}
	return nil
}

func validateFinished(in stageFinishedInput) error {
	if err := in.Stage.Validate(); err != nil {
		return invalid("stage", "invalid")
	}
	if in.Attempt == nil || *in.Attempt < 1 {
		return invalid("attempt", "out_of_range")
	}
	if in.RunnerError != nil && !slices.Contains(store.RunnerErrors, *in.RunnerError) {
		return invalid("runnerError", "invalid")
	}
	work := in.Stage == model.StageWork
	// The Runner sends the result JSON it collected from a program stage, and
	// null when it decided the failure itself (runnerError) or for work.
	if (work || in.RunnerError != nil) != isNull(in.Result) {
		return invalid("result", "stage_mismatch")
	}
	if work && in.RunnerError == nil {
		if in.Work == nil {
			return invalid("work", "required")
		}
		if in.ExitCode == nil {
			return invalid("exitCode", "required")
		}
	} else if !work && in.Work != nil {
		return invalid("work", "stage_mismatch")
	}
	if work && in.ResultArtifactID != nil {
		return invalid("resultArtifactId", "stage_mismatch")
	}
	if in.Work != nil && in.Work.SessionID != nil && *in.Work.SessionID == "" {
		return invalid("work.sessionId", "invalid")
	}
	u := in.Usage
	if u == nil || u.WallTimeMS == nil {
		return invalid("usage.wallTimeMs", "required")
	}
	for _, value := range []*int64{u.WallTimeMS, u.InputTokens, u.OutputTokens} {
		if value != nil && *value < 0 {
			return invalid("usage", "out_of_range")
		}
	}
	if u.CostAmount != nil && (*u.CostAmount < 0 || math.IsNaN(*u.CostAmount) || math.IsInf(*u.CostAmount, 0)) {
		return invalid("usage", "out_of_range")
	}
	return nil
}

// canonicalHash identifies a report's content independent of key order and
// whitespace, so a resend matches the saved report (spec 04 3.4.1).
func canonicalHash(data []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", invalid("body", "invalid_json")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func finishedReport(id, runner, hash string, in stageFinishedInput) store.StageFinish {
	out := store.StageFinish{
		ExecutionID: id, RunnerID: runner, Stage: in.Stage, Attempt: *in.Attempt, ExitCode: in.ExitCode,
		ResultArtifactID: in.ResultArtifactID, ReportSHA256: hash,
		Usage: store.StageUsage{WallTimeMS: *in.Usage.WallTimeMS, InputTokens: in.Usage.InputTokens,
			OutputTokens: in.Usage.OutputTokens, CostAmount: in.Usage.CostAmount},
	}
	if in.RunnerError != nil {
		out.RunnerError = *in.RunnerError
	}
	if !isNull(in.Result) {
		out.Result = in.Result
	}
	if in.Work != nil {
		out.Work = &store.StageWork{SessionID: in.Work.SessionID, SummaryArtifactID: in.Work.SummaryArtifactID}
		if !isNull(in.Work.Question) {
			out.Work.Question = in.Work.Question
		}
	}
	return out
}
