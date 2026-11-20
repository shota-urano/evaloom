package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Serialize the DB receipt check, delivery and receipt write across workers in
// this process. Restart deduplication uses only the DB receipts (spec 12 3.4,
// Q148), never this lock or a process-local attempt registry.
var decisionDispatchMu sync.Mutex

type decisionBatch struct {
	n   Notification
	ids []string
}

// Read only notification metadata. Decision detail, approval target, check
// findings and stage summaries never enter a Notification, even for a new
// transport. owner_decisions itself is the committed, durable request.
func (d *Dispatcher) pendingDecisions(ctx context.Context) ([]decisionBatch, error) {
	rows, err := d.DB.QueryContext(ctx, `SELECT o.id, coalesce(o.execution_id, ''), o.task_id, t.title, o.kind,
		coalesce(o.cause, ''), coalesce((SELECT json_extract(v.payload, '$.notificationBatch')
			FROM execution_events v WHERE v.execution_id = o.execution_id AND v.type = 'decision_opened'
			AND json_extract(v.payload, '$.decisionId') = o.id ORDER BY v.id LIMIT 1), o.id)
		FROM owner_decisions o JOIN tasks t ON t.id = o.task_id
		WHERE o.status = 'open' AND NOT EXISTS (
			SELECT 1 FROM execution_events s WHERE s.execution_id = o.execution_id AND s.type = 'notification_sent'
			AND json_extract(s.payload, '$.kind') = 'decision' AND json_extract(s.payload, '$.decisionId') = o.id)
		AND NOT EXISTS (SELECT 1 FROM audit_logs a WHERE a.action = 'notification.sent'
			AND a.target_type = 'owner_decision' AND a.target_id = o.id)
		ORDER BY o.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batches []decisionBatch
	indices := make(map[string]int)
	for rows.Next() {
		var n Notification
		var batch string
		if err := rows.Scan(&n.DecisionID, &n.ExecutionID, &n.TaskID, &n.Title, &n.DecisionKind, &n.DecisionCause, &batch); err != nil {
			return nil, err
		}
		if n.DecisionKind != model.DecisionApproval {
			batch = n.DecisionID
		}
		// Include the Execution even if old data happens to reuse a batch ID.
		key := n.ExecutionID + "/" + batch
		if index, ok := indices[key]; ok {
			batches[index].ids = append(batches[index].ids, n.DecisionID)
			continue
		}
		indices[key] = len(batches)
		batches = append(batches, decisionBatch{n: n, ids: []string{n.DecisionID}})
	}
	return batches, rows.Err()
}

func (d *Dispatcher) dispatchDecisions(ctx context.Context) error {
	decisionDispatchMu.Lock()
	defer decisionDispatchMu.Unlock()
	batches, err := d.pendingDecisions(ctx)
	if err != nil {
		return err
	}
	if d.attemptedDecisions == nil {
		d.attemptedDecisions = make(map[string]bool)
	}
	var errs error
	count := 0
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return errors.Join(errs, err)
		}
		var ids []string
		for _, id := range batch.ids {
			if !d.attemptedDecisions[id] {
				d.attemptedDecisions[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		batch.ids = ids
		batch.n.DecisionID = ids[0]
		_, batch.n.Body, err = notificationContent(batch.n)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		for _, channel := range d.Channels {
			sendErr := channel.Notifier.Notify(ctx, batch.n)
			if sendErr != nil {
				errs = errors.Join(errs, fmt.Errorf("notification: decision %s via %s: %w", batch.n.DecisionID, channel.Name, sendErr))
			}
			// Do not let cancellation discard the result of an attempted send.
			receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := d.recordDecisionResult(receiptCtx, batch, channel.Name, sendErr)
			cancel()
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("notification: persist decision %s via %s result (send_succeeded=%t): %w",
					batch.n.DecisionID, channel.Name, sendErr == nil, err))
			}
		}
		count++
		if count == 100 {
			break
		}
	}
	return errs
}

func (d *Dispatcher) recordDecisionResult(ctx context.Context, batch decisionBatch, channel string, sendErr error) error {
	if sendErr != nil {
		if batch.n.ExecutionID == "" {
			return nil // Q148: route failures go to the Control Plane log only.
		}
		payload, _ := json.Marshal(map[string]string{"kind": "decision", "channel": channel, "reason": failureReason(sendErr)})
		_, err := store.AppendExecutionEvent(ctx, d.DB, batch.n.ExecutionID, model.EventNotificationFailed, payload)
		return err
	}
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range batch.ids {
		if batch.n.ExecutionID == "" {
			payload, _ := json.Marshal(map[string]string{"kind": "decision", "channel": channel})
			_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs (actor, action, target_type, target_id, detail, created_at)
				VALUES ('system', 'notification.sent', 'owner_decision', ?, ?, ?)`, id, string(payload), time.Now().UTC().Format(time.RFC3339Nano))
		} else {
			payload, _ := json.Marshal(map[string]string{"kind": "decision", "channel": channel, "decisionId": id})
			_, err = store.AppendExecutionEvent(ctx, tx, batch.n.ExecutionID, model.EventNotificationSent, payload)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
