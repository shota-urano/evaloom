package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// LinearSettings is the store of the global settings linear.triggerLabel and
// linear.routingLabelPrefix (spec 15 5章, Q48), shared with the Linear
// Poller. *linearintake.LabelSettings implements it.
type LinearSettings interface {
	LinearLabels() linearintake.Labels
	SetLinearLabels(linearintake.Labels)
}

// settingsView is GET/PATCH /api/settings. Spec 15 5章 adds the linear
// object; the rest of the global settings has no defined shape yet.
type settingsView struct {
	Linear linearintake.Labels `json:"linear"`
}

func getSettings(settings LinearSettings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, settingsView{Linear: settings.LinearLabels()})
	}
}

// patchSettings replaces the supplied keys of linear and keeps the omitted
// ones. Unknown keys, null and blank values are rejected.
func patchSettings(settings LinearSettings, logger *slog.Logger) http.HandlerFunc {
	var mu sync.Mutex // read-modify-write of concurrent PATCHes
	return func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]json.RawMessage
		if err := DecodeJSON(r, &patch); err != nil {
			WriteError(w, logger, err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		labels := settings.LinearLabels()
		for key, value := range patch {
			if key != "linear" {
				WriteError(w, logger, invalid("body", "invalid_json"))
				return
			}
			var linear map[string]json.RawMessage
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &linear) != nil {
				WriteError(w, logger, invalid("linear", "invalid"))
				return
			}
			for name, raw := range linear {
				var target *string
				switch name {
				case "triggerLabel":
					target = &labels.TriggerLabel
				case "routingLabelPrefix":
					target = &labels.RoutingLabelPrefix
				default:
					WriteError(w, logger, invalid("body", "invalid_json"))
					return
				}
				field := "linear." + name
				var text *string
				if err := json.Unmarshal(raw, &text); err != nil {
					WriteError(w, logger, invalid(field, "invalid"))
					return
				}
				// An empty label would match nothing (or, as a prefix, every
				// label), so a value is required.
				if text == nil || strings.TrimSpace(*text) == "" {
					WriteError(w, logger, invalid(field, "required"))
					return
				}
				*target = *text
			}
		}
		settings.SetLinearLabels(labels)
		writeJSON(w, http.StatusOK, settingsView{Linear: labels})
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

type linearStatusView struct {
	Teams  []linearTeamStatusView  `json:"teams"`
	Issues []linearIssueStatusView `json:"issues"`
}

// getLinearStatus serves GET /api/linear/status (spec 15 5章): per Team the
// last successful poll, the failure classification and the consecutive
// failure count, and the Issues carrying the trigger label in the last poll
// with their last decision. It reads only linear_poll_teams and
// linear_issue_states; neither holds the API key or Linear response bodies.
func getLinearStatus(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teams, err := store.ListLinearPollTeams(r.Context(), db)
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
			Teams:  make([]linearTeamStatusView, 0, len(teams)),
			Issues: make([]linearIssueStatusView, 0, len(issues)),
		}
		for _, team := range teams {
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
