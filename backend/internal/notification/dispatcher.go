package notification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Serialize receipt checks, delivery and recording within this process.
// A crash after delivery but before recording may resend (spec 12 3.4).
var lifecycleDispatchMu sync.Mutex

// Dispatcher consumes only committed requests and records results after sending.
type Dispatcher struct {
	DB       *sql.DB
	Channels []Channel
	// Suppress retry loops after failed sends or receipt writes by this worker.
	// This is not durable: only DB receipts suppress sends after a restart.
	attemptedDecisions  map[string]bool
	attemptedExecutions map[string]bool
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
// Errors never roll back a lifecycle transition. This worker does not retry;
// after a restart, an unrecorded send may be delivered again.
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
	return errors.Join(d.dispatchLifecycle(ctx), d.dispatchDecisions(ctx))
}

func (d *Dispatcher) dispatchLifecycle(ctx context.Context) error {
	lifecycleDispatchMu.Lock()
	defer lifecycleDispatchMu.Unlock()
	if d.attemptedExecutions == nil {
		d.attemptedExecutions = make(map[string]bool)
	}
	var errs error
	for range 100 {
		n, err := d.pendingExecution(ctx)
		if err != nil {
			return errors.Join(errs, err)
		}
		if n == nil {
			return errs
		}
		d.attemptedExecutions[n.ExecutionID+"/"+string(n.Status)] = true
		_, n.Body, _ = notificationContent(*n)
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
				errs = errors.Join(errs, fmt.Errorf("notification: persist %s/%s via %s result (send_succeeded=%t): %w",
					n.ExecutionID, n.Status, channel.Name, kind == model.EventNotificationSent, err))
			}
		}
	}
	return errs
}

func (d *Dispatcher) pendingExecution(ctx context.Context) (*Notification, error) {
	// Read metadata after the producer commits, including recovery paths
	// which write classification rows after transitioning. Use the request's
	// status: the Execution may already have moved on. Linear feedback has its
	// own worker and receipts, which must not suppress desktop notifications.
	rows, err := d.DB.QueryContext(ctx, `SELECT r.execution_id, r.status, t.id, t.title,
		coalesce(e.error_class, ''), coalesce(e.error_message, ''),
		coalesce((SELECT json_extract(payload, '$.from') FROM execution_events
			WHERE execution_id = e.id AND type = 'status_changed' AND json_extract(payload, '$.to') = r.status
			ORDER BY id LIMIT 1), ''),
		coalesce(json_extract(e.outputs, '$[0].label'), ''), max(coalesce(json_array_length(e.outputs), 0) - 1, 0)
		FROM notification_requests r JOIN executions e ON e.id = r.execution_id JOIN tasks t ON t.id = e.task_id
		WHERE r.status IN ('succeeded', 'failed', 'interrupted', 'timed_out')
		AND NOT EXISTS (
			SELECT 1 FROM execution_events s WHERE s.execution_id = r.execution_id
			AND s.type IN ('notification_sent', 'notification_failed')
			AND json_extract(s.payload, '$.channel') != 'linear'
			AND coalesce(json_extract(s.payload, '$.status'), json_extract(s.payload, '$.kind')) = r.status)
		ORDER BY r.created_at, r.execution_id, r.status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ExecutionID, &n.Status, &n.TaskID, &n.Title, &n.ErrorClass, &n.Reason, &n.Stage, &n.OutputLabel, &n.OutputRemaining); err != nil {
			return nil, err
		}
		if !d.attemptedExecutions[n.ExecutionID+"/"+string(n.Status)] {
			return &n, nil
		}
	}
	return nil, rows.Err()
}
