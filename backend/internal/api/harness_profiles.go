package api

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func listHarnessProfiles(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		harnessID := query.Get("harnessId")
		if values, ok := query["harnessId"]; ok {
			if len(values) != 1 {
				err = invalid("harnessId", "invalid")
			} else {
				err = ValidateID("harnessId", harnessID, SlugID)
			}
		}
		resource := "harness-profiles/" + harnessID
		var page PageRequest
		if err == nil {
			page, err = ParsePage(query, resource)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		profiles, err := store.ListHarnessProfiles(r.Context(), db, harnessID, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, NewPage(profiles, page.Limit, resource, func(p model.HarnessProfile) string { return p.ID }))
	}
}

func registerHarnessProfile(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			HarnessID string `json:"harnessId"`
			Version   string `json:"version"`
		}
		err := DecodeJSON(r, &input)
		if err == nil {
			err = ValidateID("harnessId", input.HarnessID, SlugID)
		}
		if err == nil && !registry.ValidProfileVersion(input.Version) {
			err = &Error{Code: PreconditionFailed, Message: "Profile version must be SemVer", Details: []Detail{{Field: "version", Reason: "invalid_format"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		before, err := store.GetHarness(r.Context(), db, input.HarnessID)
		if err != nil {
			WriteError(w, logger, harnessError(err))
			return
		}
		profile, definitionErr := registry.ReadProfile(r.Context(), before.RepoPath, input.Version, before.Adapter)
		if profile.CommitSHA == "" {
			WriteError(w, logger, definitionErr)
			return
		}
		// All external Git operations have finished. Recheck both the Harness
		// and (Harness, version) inside the serialized saving transaction.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		current, err := store.GetHarness(r.Context(), tx, before.ID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !reflect.DeepEqual(before, current)) {
			err = &Error{Code: Conflict, Message: "Harness changed during Profile validation; retry the request"}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		existing, err := store.GetHarnessProfile(r.Context(), tx, before.ID, input.Version)
		if err == nil {
			if existing.CommitSHA == profile.CommitSHA {
				writeJSON(w, http.StatusOK, existing)
				return
			}
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
				Actor: "owner", Action: "profile.register_conflict", TargetType: "harness_profile", TargetID: existing.ID,
				Detail: map[string]any{"harnessId": before.ID, "version": input.Version, "registeredCommitSha": existing.CommitSHA, "requestedCommitSha": profile.CommitSHA},
			})
			if err == nil {
				err = tx.Commit()
			}
			if err == nil {
				err = &Error{Code: Conflict, Message: "Profile version already registered with another commit SHA"}
			}
			WriteError(w, logger, err)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			WriteError(w, logger, err)
			return
		}
		if definitionErr != nil {
			WriteError(w, logger, definitionErr)
			return
		}
		now := time.Now().UTC()
		id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		profile.ID, profile.HarnessID = id.String(), before.ID
		profile.OperationStatus, profile.RegisteredAt = "provisional", now.Format(time.RFC3339Nano)
		err = store.InsertHarnessProfile(r.Context(), tx, profile)
		if err == nil {
			_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
				Actor: "owner", Action: "profile.register", TargetType: "harness_profile", TargetID: profile.ID,
				Detail: map[string]any{"harnessId": profile.HarnessID, "version": profile.Version, "commitSha": profile.CommitSHA, "definitionHash": profile.DefinitionHash, "operationStatus": profile.OperationStatus},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusCreated, profile)
	}
}
