package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	eventBatchSize    = 200
	eventPollInterval = 250 * time.Millisecond
)

func parseLastEventID(r *http.Request) (*int64, error) {
	values := r.Header.Values("Last-Event-ID")
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, invalid("Last-Event-ID", "invalid_format")
	}
	id, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != values[0] {
		return nil, invalid("Last-Event-ID", "invalid_format")
	}
	return &id, nil
}

func executionEvents(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		after, err := parseLastEventID(r)
		if err == nil {
			err = store.CheckExecutionEventCursor(r.Context(), db, id, after)
		}
		if errors.Is(err, store.ErrExecutionNotFound) {
			err = &Error{Code: NotFound, Message: "Execution not found"}
		} else if errors.Is(err, store.ErrEventCursorUnavailable) {
			err = &Error{Code: PreconditionFailed, Message: "Event cursor is not available for this Execution",
				Details: []Detail{{Field: "Last-Event-ID", Reason: "out_of_range"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		var lastID int64
		if after != nil {
			lastID = *after
		}
		// Read before committing the response so initial DB failures use the
		// common JSON error envelope rather than a misleading successful SSE.
		events, err := store.ListExecutionEvents(r.Context(), db, id, lastID, eventBatchSize)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		flusher, ok := w.(http.Flusher)
		// ServeMux's GET pattern also matches HEAD. Return metadata without
		// retaining an invisible streaming request on a persistent connection.
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		if !ok {
			WriteError(w, logger, fmt.Errorf("SSE requires HTTP flushing"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush() // Establish even an empty stream without waiting for an event.
		ticker := time.NewTicker(eventPollInterval)
		defer ticker.Stop()
		for {
			for _, event := range events {
				if r.Context().Err() != nil {
					return
				}
				// Marshal RawMessage to compact pretty-printed JSON into a single
				// SSE data line. Escaped newlines in log text remain escaped.
				payload, err := json.Marshal(event.Payload)
				if err != nil {
					logger.Error("SSE payload encoding failed", "error", err)
					return
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, payload); err != nil {
					return
				}
				flusher.Flush()
				lastID = event.ID
			}
			// Drain full batches immediately. Only wait when caught up. Polling
			// the persisted log avoids a replay/subscription handover race and
			// works for producers committing through their own transactions.
			if len(events) < eventBatchSize {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
				}
			}
			events, err = store.ListExecutionEvents(r.Context(), db, id, lastID, eventBatchSize)
			if err != nil {
				if r.Context().Err() == nil {
					logger.Error("SSE read failed", "error", err)
				}
				// Headers are already committed: end the stream, allowing resume
				// from its last delivered ID. Do not put JSON errors into SSE.
				return
			}
		}
	}
}
