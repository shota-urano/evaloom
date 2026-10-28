package api

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

type taskExecutionView struct {
	executionSummaryView
	ProfileVersion   *string    `json:"profileVersion"`
	ProfileCommitSHA *string    `json:"profileCommitSha"`
	StartedAt        *Timestamp `json:"startedAt"`
	FinishedAt       *Timestamp `json:"finishedAt"`
}

type taskDetailView struct {
	taskSummaryView
	Executions []taskExecutionView `json:"executions"`
}

func getTask(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, TaskID); err != nil {
			WriteError(w, logger, err)
			return
		}
		detail, err := store.GetTaskDetail(r.Context(), db, id)
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: NotFound, Message: "Task not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		view := taskDetailView{
			taskSummaryView: taskSummaryView{Task: detail.Task},
			Executions:      make([]taskExecutionView, 0, len(detail.Executions)),
		}
		for _, e := range detail.Executions {
			execution := taskExecutionView{
				executionSummaryView: executionSummaryView{e.ID, e.Seq, e.Mode, e.Status, e.ErrorClass},
				ProfileVersion:       e.ProfileVersion, ProfileCommitSHA: e.ProfileCommitSHA,
			}
			for _, field := range []struct {
				stored *string
				target **Timestamp
			}{{e.StartedAt, &execution.StartedAt}, {e.FinishedAt, &execution.FinishedAt}} {
				if field.stored == nil {
					continue
				}
				value, err := ParseTimestamp(*field.stored)
				if err != nil {
					WriteError(w, logger, fmt.Errorf("execution %q: invalid stored timestamp", e.ID))
					return
				}
				*field.target = &value
			}
			view.Executions = append(view.Executions, execution)
			view.LatestExecution = &execution.executionSummaryView
			// Match the owner task list: the highest seq with a recorded PR URL.
			if e.PRURL != nil {
				view.PRURL = e.PRURL
			}
		}
		writeJSON(w, http.StatusOK, view)
	}
}
