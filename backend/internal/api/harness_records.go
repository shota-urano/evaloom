package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

type harnessRecordResponse struct {
	ID               string  `json:"id"`
	HarnessID        string  `json:"harnessId"`
	ArtifactID       string  `json:"artifactId"`
	ExecutionID      string  `json:"executionId"`
	TaskID           string  `json:"taskId"`
	SourceIdentifier *string `json:"sourceIdentifier"`
	FileName         string  `json:"fileName"`
	CreatedAt        string  `json:"createdAt"`
	DeletedAt        *string `json:"deletedAt"`
}

func harnessRecordJSON(record store.HarnessRecord) harnessRecordResponse {
	return harnessRecordResponse{record.ID, record.HarnessID, record.ArtifactID, record.ExecutionID,
		record.TaskID, record.SourceIdentifier, record.FileName, record.CreatedAt, record.DeletedAt}
}

func listHarnessRecords(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, SlugID); err != nil {
			WriteError(w, logger, err)
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		resource := "harness-records/" + id
		page, err := ParsePage(query, resource)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		var afterTime, afterID string
		if page.After != "" {
			var ok bool
			afterTime, afterID, ok = strings.Cut(page.After, "|")
			_, timeErr := ParseTimestamp(afterTime)
			if !ok || timeErr != nil || ValidateID("id", afterID, ULID) != nil {
				WriteError(w, logger, invalid("cursor", "invalid"))
				return
			}
		}
		if _, err := store.GetHarness(r.Context(), db, id); err != nil {
			WriteError(w, logger, harnessError(err))
			return
		}
		records, err := store.ListHarnessRecords(r.Context(), db, id, afterTime, afterID, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		items := make([]harnessRecordResponse, 0, len(records))
		for _, record := range records {
			items = append(items, harnessRecordJSON(record))
		}
		writeJSON(w, http.StatusOK, NewPage(items, page.Limit, resource, func(record harnessRecordResponse) string {
			return record.CreatedAt + "|" + record.ID
		}))
	}
}

func harnessRecordError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Harness record not found"}
	}
	return err
}

func deleteHarnessRecord(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		before, err := store.GetHarnessRecord(r.Context(), tx, id)
		if err == nil && before.DeletedAt == nil {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_, err = tx.ExecContext(r.Context(), `UPDATE harness_records SET deleted_at=? WHERE id=?`, now, id)
			if err == nil {
				after := before
				after.DeletedAt = &now
				_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
					Actor: "owner", Action: "harness_record.delete", TargetType: "harness_record", TargetID: id,
					Detail: map[string]any{"before": harnessRecordJSON(before), "after": harnessRecordJSON(after)},
				})
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, harnessRecordError(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
