package api

import (
	"database/sql"
	"errors"
	"fmt"
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
	PRURL            *string                        `json:"prUrl"`
	ContextRevisions []executionContextRevisionView `json:"contextRevisions"`
	GateResults      []executionGateResultView      `json:"gateResults"`
}

type executionContextRevisionView struct {
	ID              string `json:"id"`
	Layer           string `json:"layer"`
	Kind            string `json:"kind"`
	SourcePath      string `json:"sourcePath"`
	SourceCommitSHA string `json:"sourceCommitSha"`
	ContentHash     string `json:"contentHash"`
}

type executionGateResultView struct {
	ID        string          `json:"id"`
	Gate      int             `json:"gate"`
	Result    string          `json:"result"`
	BaseSHA   string          `json:"baseSha"`
	TargetSHA string          `json:"targetSha"`
	Findings  []store.Finding `json:"findings"`
	CreatedAt Timestamp       `json:"createdAt"`
}

func getExecution(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
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
				executionSummaryView: executionSummaryView{detail.ID, detail.Seq, detail.Mode, detail.Status, detail.ErrorClass},
				ProfileVersion:       detail.ProfileVersion, ProfileCommitSHA: detail.ProfileCommitSHA,
			},
			TaskID: detail.TaskID, RunnerID: detail.RunnerID, Role: detail.Role,
			HarnessID: detail.HarnessID, ProfileID: detail.ProfileID, PRURL: detail.PRURL,
			ContextRevisions: make([]executionContextRevisionView, 0, len(detail.ContextRevisions)),
			GateResults:      make([]executionGateResultView, 0, len(detail.GateResults)),
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
		for _, gate := range detail.GateResults {
			created, err := ParseTimestamp(gate.CreatedAt)
			if err != nil {
				WriteError(w, logger, fmt.Errorf("gate result %q: invalid stored timestamp", gate.ID))
				return
			}
			view.GateResults = append(view.GateResults, executionGateResultView{
				ID: gate.ID, Gate: gate.Gate, Result: gate.Result, BaseSHA: gate.BaseSHA,
				TargetSHA: gate.TargetSHA, Findings: gate.Findings, CreatedAt: created,
			})
		}
		writeJSON(w, http.StatusOK, view)
	}
}
