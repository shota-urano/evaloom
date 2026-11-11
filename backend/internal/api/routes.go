package api

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func routeError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Route not found"}
	}
	if errors.Is(err, store.ErrRouteConflict) {
		return &Error{Code: Conflict, Message: err.Error()}
	}
	return err
}

func listRoutes(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		page, err := ParsePage(query, "routes")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		// Labels may contain control characters rejected by the shared cursor
		// parser. Encode the boundary separately and restore it before querying.
		if page.After != "" {
			label, err := base64.RawURLEncoding.Strict().DecodeString(page.After)
			if err != nil || len(label) == 0 || !utf8.Valid(label) {
				WriteError(w, logger, invalid("cursor", "invalid"))
				return
			}
			page.After = string(label)
		}
		routes, err := store.ListRoutes(r.Context(), db, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, NewPage(routes, page.Limit, "routes", func(route store.Route) string {
			return base64.RawURLEncoding.EncodeToString([]byte(route.Label))
		}))
	}
}

func writeRoute(db *sql.DB, logger *slog.Logger, create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input map[string]json.RawMessage
		if err := DecodeJSON(r, &input); err != nil {
			WriteError(w, logger, err)
			return
		}
		label := r.PathValue("label")
		if create {
			if err := json.Unmarshal(input["label"], &label); err != nil {
				WriteError(w, logger, invalid("label", "required"))
				return
			}
		}
		for key := range input {
			if key != "projectId" && key != "harnessId" && !(create && key == "label") {
				WriteError(w, logger, invalid(key, "not_allowed"))
				return
			}
		}
		var route store.Route
		route.Label = label
		for _, field := range []struct {
			key    string
			target **string
		}{{"projectId", &route.ProjectID}, {"harnessId", &route.HarnessID}} {
			if value, ok := input[field.key]; ok {
				var id string
				if json.Unmarshal(value, &id) != nil || ValidateID(field.key, id, SlugID) != nil {
					WriteError(w, logger, invalid(field.key, "invalid"))
					return
				}
				*field.target = &id
			}
		}
		if (route.ProjectID == nil) == (route.HarnessID == nil) {
			WriteError(w, logger, invalid("body", "exactly_one_destination"))
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		var before any
		if !create {
			old, err := store.GetRoute(r.Context(), tx, label)
			if err != nil {
				WriteError(w, logger, routeError(err))
				return
			}
			route.CreatedAt, before = old.CreatedAt, old
		}
		if create {
			settings, err := store.ReadSettings(r.Context(), tx)
			if err != nil {
				WriteError(w, logger, err)
				return
			}
			prefix, trigger := "route:", "ai-ready"
			for _, setting := range []struct {
				key   string
				value *string
			}{{"linear.routingLabelPrefix", &prefix}, {"linear.triggerLabel", &trigger}} {
				if raw, ok := settings[setting.key]; ok {
					if err := json.Unmarshal([]byte(raw), setting.value); err != nil {
						WriteError(w, logger, err)
						return
					}
				}
			}
			if !strings.HasPrefix(label, prefix) || label == prefix || label == trigger || strings.TrimSpace(label) != label || utf8.RuneCountInString(label) > 100 {
				WriteError(w, logger, invalid("label", "invalid"))
				return
			}
		}
		if route.ProjectID != nil {
			_, err = store.GetProject(r.Context(), tx, *route.ProjectID)
		} else {
			_, err = store.GetHarness(r.Context(), tx, *route.HarnessID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			err = &Error{Code: PreconditionFailed, Message: "Route destination not found"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		route.UpdatedAt = now
		action, status := "route.update", http.StatusOK
		if create {
			route.CreatedAt = now
			action, status = "route.create", http.StatusCreated
		}
		err = store.SaveRoute(r.Context(), tx, route, create)
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{Actor: "owner", Action: action, TargetType: "route", TargetID: label, Detail: map[string]any{"before": before, "after": route}})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, routeError(err))
			return
		}
		writeJSON(w, status, route)
	}
}

func deleteRoute(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		before, err := store.GetRoute(r.Context(), tx, r.PathValue("label"))
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `DELETE FROM routes WHERE label=?`, before.Label)
		}
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{Actor: "owner", Action: "route.delete", TargetType: "route", TargetID: before.Label, Detail: map[string]any{"before": before, "after": nil}})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, routeError(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
