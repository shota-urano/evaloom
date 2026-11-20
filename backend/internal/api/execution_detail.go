package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

type executionDetailView struct {
	taskExecutionView
	TaskID           string                         `json:"taskId"`
	RunnerID         *string                        `json:"runnerId"`
	Role             *string                        `json:"role"`
	HarnessID        *string                        `json:"harnessId"`
	ProfileID        *string                        `json:"profileId"`
	ContextRevisions []executionContextRevisionView `json:"contextRevisions"`
	Params           json.RawMessage                `json:"params"`
	ContractVersion  *string                        `json:"contractVersion"`
	ResumeStage      *string                        `json:"resumeStage"`
	StageRuns        []executionStageRunView        `json:"stageRuns"`
	CheckResults     []executionCheckResultView     `json:"checkResults"`
	Artifacts        []store.ExecutionArtifact      `json:"artifacts"`
}

type executionStageRunView struct {
	ID         string     `json:"id"`
	Stage      string     `json:"stage"`
	Attempt    int        `json:"attempt"`
	Status     string     `json:"status"`
	ErrorClass *string    `json:"errorClass"`
	StartedAt  Timestamp  `json:"startedAt"`
	FinishedAt *Timestamp `json:"finishedAt"`
	Message    *string    `json:"message"`
	Summary    *string    `json:"summary"`
}

type executionCheckResultView struct {
	ID        string          `json:"id"`
	Result    string          `json:"result"`
	ContentID *string         `json:"contentId"`
	Summary   *string         `json:"summary"`
	Findings  json.RawMessage `json:"findings"`
}

type executionContextRevisionView struct {
	ID              string `json:"id"`
	Layer           string `json:"layer"`
	Kind            string `json:"kind"`
	SourcePath      string `json:"sourcePath"`
	SourceCommitSHA string `json:"sourceCommitSha"`
	ContentHash     string `json:"contentHash"`
}

func getExecution(db *sql.DB, logger *slog.Logger, artifacts ArtifactConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		detail, err := store.GetExecutionDetail(r.Context(), db, id)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		view := executionDetailView{
			taskExecutionView: taskExecutionView{
				executionSummaryView: newExecutionSummaryView(detail.ExecutionSummary),
				ProfileVersion:       detail.ProfileVersion, ProfileCommitSHA: detail.ProfileCommitSHA,
			},
			TaskID: detail.TaskID, RunnerID: detail.RunnerID, Role: detail.Role,
			HarnessID: detail.HarnessID, ProfileID: detail.ProfileID,
			ContextRevisions: make([]executionContextRevisionView, 0, len(detail.ContextRevisions)),
			Params:           detail.Params, ContractVersion: detail.ContractVersion, ResumeStage: detail.ResumeStage,
			StageRuns:    make([]executionStageRunView, 0, len(detail.StageRuns)),
			CheckResults: make([]executionCheckResultView, 0, len(detail.CheckResults)),
			Artifacts:    detail.Artifacts,
		}
		for _, field := range []struct {
			stored *string
			target **Timestamp
		}{{detail.StartedAt, &view.StartedAt}, {detail.FinishedAt, &view.FinishedAt}} {
			if field.stored == nil {
				continue
			}
			value, err := ParseTimestamp(*field.stored)
			if err != nil {
				WriteError(w, logger, fmt.Errorf("execution %q: invalid stored timestamp", id))
				return
			}
			*field.target = &value
		}
		for _, revision := range detail.ContextRevisions {
			view.ContextRevisions = append(view.ContextRevisions, executionContextRevisionView{
				ID: revision.ID, Layer: revision.Layer, Kind: revision.Kind, SourcePath: revision.SourcePath,
				SourceCommitSHA: revision.SourceCommitSHA, ContentHash: revision.ContentHash,
			})
		}
		for _, run := range detail.StageRuns {
			stage := executionStageRunView{
				ID: run.ID, Stage: run.Stage, Attempt: run.Attempt, Status: run.Status, ErrorClass: run.ErrorClass,
			}
			stage.StartedAt, err = ParseTimestamp(run.StartedAt)
			if err == nil && run.FinishedAt != nil {
				var finished Timestamp
				finished, err = ParseTimestamp(*run.FinishedAt)
				stage.FinishedAt = &finished
			}
			if err != nil {
				WriteError(w, logger, fmt.Errorf("stage run %q: invalid stored timestamp", run.ID))
				return
			}
			if run.ResultArtifactID != nil {
				if artifacts.Store == nil {
					WriteError(w, logger, fmt.Errorf("artifact storage must be configured"))
					return
				}
				_, file, err := artifacts.Store.Open(r.Context(), id, *run.ResultArtifactID)
				if err != nil {
					WriteError(w, logger, err)
					return
				}
				data, readErr := io.ReadAll(io.LimitReader(file, maxResultBody+1))
				if err := errors.Join(readErr, file.Close()); err != nil {
					WriteError(w, logger, err)
					return
				}
				var result struct {
					Message *string `json:"message"`
					Summary *string `json:"summary"`
				}
				// Invalid result JSON is retained for contract_error diagnostics.
				// Keep the detail readable even when these fields cannot be decoded.
				if len(data) <= maxResultBody && json.Unmarshal(data, &result) == nil {
					stage.Message, stage.Summary = result.Message, result.Summary
				}
			}
			view.StageRuns = append(view.StageRuns, stage)
		}
		for _, check := range detail.CheckResults {
			view.CheckResults = append(view.CheckResults, executionCheckResultView{
				ID: check.ID, Result: check.Result, ContentID: check.ContentID,
				Summary: check.Summary, Findings: check.Findings,
			})
		}
		writeJSON(w, http.StatusOK, view)
	}
}
