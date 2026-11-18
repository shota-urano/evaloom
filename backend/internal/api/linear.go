package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// LinearSettings is the store of the global settings linear.triggerLabel,
// linear.routingLabelPrefix, routing.proposer and routing.defaultRunnerId
// (spec 15 5章, 02 3.8, Q76), shared with the Linear Poller.
// *linearintake.LabelSettings implements it with the settings table.
type LinearSettings interface {
	Settings(ctx context.Context) linearintake.Settings
	UpdateSettings(ctx context.Context, patch linearintake.SettingsPatch) (linearintake.Settings, error)
}

// LinearTeamStatus reports why the Linear Poller could not choose the
// polling Team (uslab, spec 15 3.2, 6章, Q216). When the LinearSettings
// given to the handler also implements it, GET /api/linear/status shows the
// problem. *linearintake.PollTeamStatus implements it.
type LinearTeamStatus interface {
	PollTeamProblem() linearintake.TeamProblem
}

func getSettings(settings LinearSettings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, settings.Settings(r.Context()))
	}
}

// requiredText decodes a settings string that must not be null, empty or
// blank (spec 15 5章, Q76).
func requiredText(field string, raw json.RawMessage) (*string, error) {
	var text *string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, invalid(field, "invalid")
	}
	if text == nil || strings.TrimSpace(*text) == "" {
		return nil, invalid(field, "required")
	}
	return text, nil
}

// settingsObject decodes a top-level group of the settings body.
func settingsObject(field string, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &object) != nil {
		return nil, invalid(field, "invalid")
	}
	return object, nil
}

// decodeProposer decodes routing.proposer, which replaces the whole value:
// microHarness is a non-blank string and model is a non-blank string or
// null (spec 02 3.8).
func decodeProposer(raw json.RawMessage) (*linearintake.RouteProposer, error) {
	const field = "routing.proposer"
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, invalid(field, "required")
	}
	object, err := settingsObject(field, raw)
	if err != nil {
		return nil, err
	}
	var proposer linearintake.RouteProposer
	for name, value := range object {
		switch name {
		case "microHarness":
			text, err := requiredText(field+".microHarness", value)
			if err != nil {
				return nil, err
			}
			proposer.MicroHarness = *text
		case "model":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue // no model: no proposals (spec 15 3.6.4)
			}
			text, err := requiredText(field+".model", value)
			if err != nil {
				return nil, err
			}
			proposer.Model = text
		default:
			return nil, invalid("body", "invalid_json")
		}
	}
	for _, name := range []string{"microHarness", "model"} {
		if _, ok := object[name]; !ok {
			return nil, invalid(field+"."+name, "required")
		}
	}
	return &proposer, nil
}

// decodeSettingsPatch reads a PATCH /api/settings body. Unknown keys
// (including linear.teamId, Q216), null and blank values are rejected
// before anything is written (spec 15 5章, Q76).
func decodeSettingsPatch(body map[string]json.RawMessage) (linearintake.SettingsPatch, error) {
	var changes linearintake.SettingsPatch
	for group, value := range body {
		switch group {
		case "linear":
			linear, err := settingsObject(group, value)
			if err != nil {
				return changes, err
			}
			for name, raw := range linear {
				var target **string
				switch name {
				case "triggerLabel":
					target = &changes.TriggerLabel
				case "routingLabelPrefix":
					target = &changes.RoutingLabelPrefix
				default:
					return changes, invalid("body", "invalid_json")
				}
				// An empty label would match nothing (or, as a prefix,
				// every label), so a value is required.
				text, err := requiredText("linear."+name, raw)
				if err != nil {
					return changes, err
				}
				*target = text
			}
		case "routing":
			routing, err := settingsObject(group, value)
			if err != nil {
				return changes, err
			}
			for name, raw := range routing {
				switch name {
				case "proposer":
					if changes.Proposer, err = decodeProposer(raw); err != nil {
						return changes, err
					}
				case "defaultRunnerId":
					if changes.DefaultRunnerID, err = requiredText("routing.defaultRunnerId", raw); err != nil {
						return changes, err
					}
				default:
					return changes, invalid("body", "invalid_json")
				}
			}
		default:
			return changes, invalid("body", "invalid_json")
		}
	}
	return changes, nil
}

// patchSettings replaces the supplied keys and keeps the omitted ones. One
// rejected key leaves every key unchanged (spec 15 5章, Q76). Label changes
// follow Q160: a trigger label starting with the prefix is 400, a prefix
// that some routes label does not start with is 422.
func patchSettings(settings LinearSettings, logger *slog.Logger) http.HandlerFunc {
	var mu sync.Mutex // read-modify-write of concurrent PATCHes
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := DecodeJSON(r, &body); err != nil {
			WriteError(w, logger, err)
			return
		}
		changes, err := decodeSettingsPatch(body)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		updated, err := settings.UpdateSettings(r.Context(), changes)
		var hasPrefix *linearintake.ErrTriggerLabelHasPrefix
		var outside *linearintake.ErrRoutesOutsidePrefix
		switch {
		case errors.As(err, &hasPrefix):
			err = invalid(hasPrefix.Field, "starts_with_routing_label_prefix")
		case errors.As(err, &outside):
			public := &Error{Code: PreconditionFailed, Message: "routes labels must start with the routing label prefix"}
			for _, label := range outside.Labels {
				public.Details = append(public.Details, Detail{Field: "routes." + label, Reason: "outside_routing_label_prefix"})
			}
			err = public
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

type linearTeamStatusView struct {
	TeamID              string     `json:"teamId"`
	LastSuccessAt       *Timestamp `json:"lastSuccessAt"`
	LastError           *string    `json:"lastError"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
}

type linearIssueStatusView struct {
	LinearIssueID   string     `json:"linearIssueId"`
	IssueIdentifier string     `json:"issueIdentifier"`
	TeamID          string     `json:"teamId"`
	LastDecision    *string    `json:"lastDecision"`
	LastDecisionAt  *Timestamp `json:"lastDecisionAt"`
}

// linearTeamProblemView says why intake is stopped: no Team or several
// Teams have the key or name uslab (spec 15 3.2, 6章, Q216).
type linearTeamProblemView struct {
	Reason  linearintake.TeamProblem `json:"reason"`
	Message string                   `json:"message"`
}

type linearStatusView struct {
	Teams       []linearTeamStatusView  `json:"teams"`
	TeamProblem *linearTeamProblemView  `json:"teamProblem"`
	Issues      []linearIssueStatusView `json:"issues"`
}

// getLinearStatus serves GET /api/linear/status (spec 15 5章): per Team the
// last successful poll, the failure classification and the consecutive
// failure count, and the Issues carrying the trigger label in the last poll
// with their last decision. It reads only linear_poll_teams and
// linear_issue_states; neither holds the API key or Linear response bodies.
// The polling Team problem comes from teams when it is set.
func getLinearStatus(db *sql.DB, logger *slog.Logger, teams LinearTeamStatus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		polled, err := store.ListLinearPollTeams(r.Context(), db)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		issues, err := store.ListAttachedLinearIssues(r.Context(), db, "")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		view := linearStatusView{
			Teams:  make([]linearTeamStatusView, 0, len(polled)),
			Issues: make([]linearIssueStatusView, 0, len(issues)),
		}
		if teams != nil {
			if problem := teams.PollTeamProblem(); problem != "" {
				view.TeamProblem = &linearTeamProblemView{Reason: problem, Message: problem.Message()}
			}
		}
		for _, team := range polled {
			item := linearTeamStatusView{TeamID: team.TeamID, ConsecutiveFailures: team.ConsecutiveFailures}
			if team.LastError != "" {
				item.LastError = &team.LastError
			}
			if item.LastSuccessAt, err = optionalTimestamp(team.LastSuccessAt); err != nil {
				WriteError(w, logger, fmt.Errorf("linear team %q: invalid stored success time", team.TeamID))
				return
			}
			view.Teams = append(view.Teams, item)
		}
		for _, issue := range issues {
			item := linearIssueStatusView{LinearIssueID: issue.LinearIssueID, IssueIdentifier: issue.IssueIdentifier, TeamID: issue.TeamID}
			if issue.LastDecision != "" {
				item.LastDecision = &issue.LastDecision
			}
			if item.LastDecisionAt, err = optionalTimestamp(issue.LastDecisionAt); err != nil {
				WriteError(w, logger, fmt.Errorf("linear issue %q: invalid stored decision time", issue.LinearIssueID))
				return
			}
			view.Issues = append(view.Issues, item)
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func optionalTimestamp(stored string) (*Timestamp, error) {
	if stored == "" {
		return nil, nil
	}
	parsed, err := ParseTimestamp(stored)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
