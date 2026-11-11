package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// routeProposalResultInput is POST /api/route-proposals/{id}/result (spec 01
// 5.2, 15 3.6.4): the chosen label and reason, or the error class. Every
// field is required; the unused ones must be present as null.
type routeProposalResultInput struct {
	Label      *string `json:"label"`
	Reason     *string `json:"reason"`
	ErrorClass *string `json:"errorClass"`
}

// Runner-reported error classes (spec 02 3.4). interrupted is set by the
// Control Plane on a Runner restart, not reported.
var reportableProposalErrors = []string{"sandbox_error", "timeout", "contract_error"}

func validateProposalResult(fields map[string]json.RawMessage, in routeProposalResultInput) error {
	known := []string{"label", "reason", "errorClass"}
	for _, field := range known {
		if _, ok := fields[field]; !ok {
			return invalid(field, "required")
		}
	}
	for field := range fields {
		if !slices.Contains(known, field) {
			return invalid(field, "not_allowed")
		}
	}
	if in.ErrorClass != nil {
		if !slices.Contains(reportableProposalErrors, *in.ErrorClass) {
			return invalid("errorClass", "invalid")
		}
		if in.Label != nil {
			return invalid("label", "not_allowed")
		}
		if in.Reason != nil {
			return invalid("reason", "not_allowed")
		}
		return nil
	}
	if in.Label == nil {
		return invalid("label", "required")
	}
	if in.Reason == nil {
		return invalid("reason", "required")
	}
	return nil
}

func reportRouteProposalResult(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		var fields map[string]json.RawMessage
		if err := DecodeJSON(r, &fields); err != nil {
			WriteError(w, logger, err)
			return
		}
		var in routeProposalResultInput
		encoded, err := json.Marshal(fields)
		if err == nil {
			if json.Unmarshal(encoded, &in) != nil {
				err = invalid("body", "invalid_json")
			}
		}
		if err == nil {
			err = validateProposalResult(fields, in)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		ctx := r.Context()
		runner, _ := ctx.Value(runnerIdentityKey{}).(string)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		err = store.ReportRouteProposalResultTx(ctx, tx, store.RouteProposalResult{
			ProposalID: id, RunnerID: runner, Label: in.Label, Reason: in.Reason, ErrorClass: in.ErrorClass,
		})
		if errors.Is(err, store.ErrRouteProposalRunnerMismatch) {
			tx.Rollback() // Release the connection before recording the denial.
			auth.reject(w, r, Forbidden, "route_proposal_runner_mismatch", "Runner cannot report this route proposal")
			return
		}
		var proposal json.RawMessage
		if err == nil {
			proposal, err = store.GetRouteProposalJSON(ctx, tx, id)
		}
		if err == nil {
			err = tx.Commit()
		}
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = &Error{Code: NotFound, Message: "Route proposal not found"}
		case errors.Is(err, task.ErrConflict):
			err = &Error{Code: Conflict, Message: err.Error()}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, proposal)
	}
}
