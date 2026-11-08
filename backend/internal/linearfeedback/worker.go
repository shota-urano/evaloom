// Package linearfeedback sends the Linear comments queued in linear_outbox
// (spec 16 3.4 steps 2-5, 6章). It is the Linear transport of spec 16 3.1:
// rows are written by linearoutbox in the transaction of the state change,
// and the Worker here delivers them with retries.
//
// The Linear transport is not registered as a notification.Dispatcher
// channel. The Dispatcher gives each Execution request one attempt without
// retry (spec 12 3.4), while Linear comments need the outbox's retry
// schedule and also cover intake holds, which have no Execution.
//
// The Worker never changes tasks or Executions: a failed send leaves the
// state transition as it is (3.4 step 5).
package linearfeedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Channel is the channel value of notification_sent / notification_failed
// for Linear comments (spec 16 5).
const Channel = "linear"

// MaxAttempts and RetryDelays are the retry policy of spec 16 3.4 and 4
// (Q80, decided 2026-11-08): at most 5 sends, waiting 1, 2, 4 and 8 minutes
// after the 1st to 4th failures. The 5th failure marks the row failed.
const MaxAttempts = 5

var RetryDelays = [MaxAttempts - 1]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// DefaultInterval is how often Run looks for due rows. It is an
// implementation choice (the spec fixes only the retry delays), equal to the
// notification Dispatcher's interval.
const DefaultInterval = 250 * time.Millisecond

// batchSize bounds one SendDue call, like the notification Dispatcher.
const batchSize = 100

// timeLayout has a fixed width so that next_attempt_at compares correctly
// as text in SQL.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// Client is the subset of *linear.Client used by the Worker.
type Client interface {
	// CheckKey reports whether the API key file is usable without sending.
	CheckKey() error
	CreateComment(ctx context.Context, issueID, body string) (string, error)
	// Issue re-reads the Issue after a failed send, to tell a deleted or
	// archived Issue from a temporary failure (6章).
	Issue(ctx context.Context, id string) (linear.Issue, error)
}

// Worker sends pending linear_outbox rows oldest first (3.4 step 2).
type Worker struct {
	DB       *sql.DB
	Client   Client
	Logger   *slog.Logger
	Interval time.Duration
	// Now is the clock; nil uses time.Now. Tests advance it to pass the
	// retry delays.
	Now func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// Run sends due rows at startup and then every Interval until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.SendDue(ctx); err != nil && ctx.Err() == nil {
			w.logger().Error("linear comment send failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type outboxRow struct {
	id, taskID, issueID, kind, dedupeKey, body string
	attempts                                   int
}

// SendDue sends up to batchSize due rows, one at a time, oldest first. No
// DB transaction is held while Linear is called. When the API key file is
// unusable nothing is sent and the rows stay pending (as the Poller does,
// spec 15 6章); the reason is logged by the client, never the key.
func (w *Worker) SendDue(ctx context.Context) error {
	if w.DB == nil || w.Client == nil {
		return errors.New("linearfeedback: worker needs a DB and a client")
	}
	keyChecked := false
	for range batchSize {
		row, err := w.next(ctx)
		if err != nil || row == nil {
			return err
		}
		if !keyChecked {
			if err := w.Client.CheckKey(); err != nil {
				return nil
			}
			keyChecked = true
		}
		// Stop the batch on any error: if the row could not be updated it
		// is still due, and sending it again in this batch would repeat
		// the comment.
		if err := w.send(ctx, *row); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) next(ctx context.Context) (*outboxRow, error) {
	var r outboxRow
	err := w.DB.QueryRowContext(ctx, `SELECT id, task_id, linear_issue_id, kind, dedupe_key, body, attempts
		FROM linear_outbox WHERE status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY created_at, id LIMIT 1`, w.now().Format(timeLayout)).Scan(
		&r.id, &r.taskID, &r.issueID, &r.kind, &r.dedupeKey, &r.body, &r.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// send makes one attempt for r. A Linear failure is handled here (retry or
// failed) and logged; only DB errors and shutdown are returned.
func (w *Worker) send(ctx context.Context, r outboxRow) error {
	commentID, sendErr := w.Client.CreateComment(ctx, r.issueID, r.body)
	if sendErr == nil {
		return w.markSent(ctx, r, commentID)
	}
	if ctx.Err() != nil {
		// Shutdown interrupted the send: it is not counted as an attempt.
		return sendErr
	}
	attempts := r.attempts + 1
	class := linear.ClassOf(sendErr)
	w.logger().Warn("linear comment not sent", "outbox_id", r.id, "task_id", r.taskID,
		"kind", r.kind, "attempt", attempts, "class", string(class))
	switch {
	case class == linear.ClassAuth:
		// 6章: an authentication error is not retried.
		return w.markFailed(ctx, r, attempts, "auth")
	case w.issueRemoved(ctx, r.issueID):
		// 6章: a deleted or archived Issue fails with reason=other and does
		// not affect the task.
		return w.markFailed(ctx, r, attempts, "issue_removed")
	case attempts >= MaxAttempts:
		return w.markFailed(ctx, r, attempts, "retries_exhausted")
	default:
		next := w.now().Add(RetryDelays[attempts-1]).Format(timeLayout)
		_, err := w.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE linear_outbox
			SET attempts = ?, next_attempt_at = ? WHERE id = ? AND status = 'pending'`, attempts, next, r.id)
		return err
	}
}

// issueRemoved reports whether Linear says the Issue was deleted, trashed or
// archived. A failed lookup counts as not removed, so the row is retried.
func (w *Worker) issueRemoved(ctx context.Context, issueID string) bool {
	issue, err := w.Client.Issue(ctx, issueID)
	if errors.Is(err, linear.ErrIssueNotFound) {
		return true
	}
	return err == nil && (issue.Archived || issue.Trashed)
}

func (w *Worker) markSent(ctx context.Context, r outboxRow, commentID string) error {
	// The comment exists in Linear now: record it even during shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return w.finish(ctx, r, `UPDATE linear_outbox SET status = 'sent', attempts = ?, next_attempt_at = NULL,
		linear_comment_id = ?, sent_at = ? WHERE id = ? AND status = 'pending'`,
		[]any{r.attempts + 1, commentID, w.now().Format(time.RFC3339Nano), r.id},
		model.EventNotificationSent, map[string]string{"kind": r.kind, "channel": Channel})
}

// markFailed sets status=failed and writes notification_failed. The payload
// reason is "other" for every Linear failure: spec 12 3.4 defines the other
// reasons for the macOS helper only, and 6章 names "other" for removed
// Issues. cause goes only to the Control Plane log.
func (w *Worker) markFailed(ctx context.Context, r outboxRow, attempts int, cause string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	w.logger().Error("linear comment failed", "outbox_id", r.id, "task_id", r.taskID,
		"kind", r.kind, "attempts", attempts, "cause", cause)
	return w.finish(ctx, r, `UPDATE linear_outbox SET status = 'failed', attempts = ?, next_attempt_at = NULL
		WHERE id = ? AND status = 'pending'`, []any{attempts, r.id},
		model.EventNotificationFailed, map[string]string{"kind": r.kind, "channel": Channel, "reason": "other"})
}

// finish updates the row and writes the event in one transaction. The event
// payload has no "status" field, so it never suppresses the macOS
// Dispatcher's request for the same Execution.
func (w *Worker) finish(ctx context.Context, r outboxRow, update string, args []any,
	event model.ExecutionEventType, payload map[string]string) error {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, update, args...)
	if err != nil {
		return fmt.Errorf("linearfeedback: update %s: %w", r.id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("linearfeedback: outbox row %s is no longer pending", r.id))
	}
	executionID, err := eventExecution(ctx, tx, r)
	if err != nil {
		return err
	}
	if executionID != "" {
		raw, _ := json.Marshal(payload)
		if _, err := store.AppendExecutionEvent(ctx, tx, executionID, event, raw); err != nil {
			return err
		}
	} else {
		// execution_events needs an Execution. Intake holds and the
		// cancellation of a held task have none, so their outcome is kept
		// in linear_outbox.status and the Control Plane log only.
		w.logger().Info("linear comment outcome has no Execution for an event", "outbox_id", r.id,
			"task_id", r.taskID, "kind", r.kind, "event", string(event))
	}
	return tx.Commit()
}

// eventExecution returns the Execution the event belongs to: the target of
// the dedupe_key <task_id>:<kind>:<target> when it is an Execution of the
// row's task (Execution triggers), or "" otherwise.
func eventExecution(ctx context.Context, tx *sql.Tx, r outboxRow) (string, error) {
	target, ok := strings.CutPrefix(r.dedupeKey, r.taskID+":"+r.kind+":")
	if !ok || target == "" {
		return "", nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM executions WHERE id = ? AND task_id = ?`, target, r.taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
