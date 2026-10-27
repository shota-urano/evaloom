package notification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Dispatcher consumes only committed requests. Multiple dispatchers sharing
// the DB serialize reservations through the store's BEGIN IMMEDIATE setting.
type Dispatcher struct {
	DB       *sql.DB
	Channels []Channel
}

// Run checks at startup and every 250ms, like the persisted SSE event reader.
// It never holds a DB connection while a transport is sending.
func (d *Dispatcher) Run(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := d.DispatchPending(ctx); err != nil {
			logger.Error("notification dispatch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DispatchPending processes a bounded batch, attempting every registered
// transport even if another fails. With no transports, requests stay pending.
// Errors never roll back a lifecycle transition or cause delivery retries.
func (d *Dispatcher) DispatchPending(ctx context.Context) error {
	if len(d.Channels) == 0 {
		return nil
	}
	names := make(map[string]bool)
	for _, channel := range d.Channels {
		if channel.Name == "" || channel.Notifier == nil || names[channel.Name] {
			return fmt.Errorf("notification: channels require unique names and non-nil Notifiers")
		}
		names[channel.Name] = true
	}
	var errs error
	for range 100 {
		n, err := d.reserve(ctx)
		if err != nil || n == nil {
			return errors.Join(errs, err)
		}
		for _, channel := range d.Channels {
			kind := model.EventNotificationSent
			fields := map[string]string{
				"kind": string(n.Status), "channel": channel.Name,
			}
			if err := channel.Notifier.Notify(ctx, *n); err != nil {
				errs = errors.Join(errs, fmt.Errorf("notification: %s/%s via %s: %w", n.ExecutionID, n.Status, channel.Name, err))
				kind = model.EventNotificationFailed
				fields["reason"] = failureReason(err)
			} else {
				fields["status"] = string(n.Status)
			}
			payload, _ := json.Marshal(fields)
			// Persist the outcome even if cancellation caused the transport to
			// fail. Bound this final write so shutdown cannot wait indefinitely.
			eventCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, err := store.AppendExecutionEvent(eventCtx, d.DB, n.ExecutionID, kind, payload)
			cancel()
			if err != nil {
				errs = errors.Join(errs, err)
			}
		}
	}
	return errs
}

func (d *Dispatcher) reserve(ctx context.Context) (*Notification, error) {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var n Notification
	// Read metadata after the producer commits, including Context Builder and
	// recovery paths which write classification/block rows after transitioning.
	// Use the request's status: a blocked Execution may already have resumed.
	err = tx.QueryRowContext(ctx, `SELECT r.execution_id, r.status, t.id, t.title,
		coalesce(e.pr_url, ''), coalesce(e.error_class, ''), coalesce(e.error_message, ''),
		coalesce((SELECT cause FROM execution_blocks WHERE execution_id = e.id ORDER BY created_at, id LIMIT 1), ''),
		coalesce((SELECT json_extract(payload, '$.from') FROM execution_events
			WHERE execution_id = e.id AND type = 'status_changed' AND json_extract(payload, '$.to') = r.status
			ORDER BY id LIMIT 1), '')
		FROM notification_requests r JOIN executions e ON e.id = r.execution_id JOIN tasks t ON t.id = e.task_id
		WHERE r.attempted_at IS NULL AND NOT EXISTS (
			SELECT 1 FROM execution_events s WHERE s.execution_id = r.execution_id AND s.type = 'notification_sent'
			AND json_extract(s.payload, '$.status') = r.status)
		ORDER BY r.created_at, r.execution_id, r.status LIMIT 1`).Scan(
		&n.ExecutionID, &n.Status, &n.TaskID, &n.Title, &n.PRURL, &n.ErrorClass, &n.Reason, &n.BlockCause, &n.Stage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The reservation commits before the side effect. A crash between these
	// steps can lose a notification, but can never send it twice after restart.
	if _, err := tx.ExecContext(ctx, `UPDATE notification_requests SET attempted_at = ?
		WHERE execution_id = ? AND status = ?`, time.Now().UTC().Format(time.RFC3339Nano), n.ExecutionID, n.Status); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &n, nil
}
