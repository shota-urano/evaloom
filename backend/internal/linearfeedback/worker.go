// Package linearfeedback sends Linear comments and Workflow state changes queued in linear_outbox
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
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Channel is the channel value of notification_sent / notification_failed
// for Linear comments and state changes (spec 16 5).
const Channel = "linear"

// The notification_failed reasons for channel=linear, also written to
// linear_outbox.failure_reason (spec 16 3.4.2, Q85). ReasonOther is for a
// failure none of the others describes; the Worker's own failures are
// always one of the specific reasons, so only rows migrated by 0018 carry
// it.
const (
	ReasonAuth             = "auth"
	ReasonIssueRemoved     = "issue_removed"
	ReasonRetriesExhausted = "retries_exhausted"
	ReasonStateNotFound    = "state_not_found"
	ReasonOther            = "other"
)

var errStateNotFound = errors.New("linearfeedback: state name not found")

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
	// IssueComments reads the Issue's comments before a resend whose
	// previous result is unknown (3.4, 6章).
	IssueComments(ctx context.Context, issueID string) ([]linear.Comment, error)
	IssueState(ctx context.Context, issueID string) (linear.IssueState, error)
	TeamStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error)
	SetIssueState(ctx context.Context, issueID, stateID string) error
}

// AuthAlerter tells the owner that a comment could not be sent because of an
// authentication error (spec 16 3.4.3, spec 12 3.5: the macOS notification
// that opens the Dashboard). It carries nothing but the fact.
type AuthAlerter interface {
	AlertLinearCommentAuth(ctx context.Context) error
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
	// AuthGate is the authentication error notification suppression shared
	// with the Linear Poller (3.4.3, Q84). nil: the Worker uses its own.
	AuthGate *linear.AuthAlertGate
	// Alerter delivers the authentication error notification. nil: it is
	// only logged.
	Alerter AuthAlerter
	// The Worker keeps no per-row state in memory: whether a row must be
	// matched against Linear before it is sent again is read from
	// linear_outbox.outcome_unknown, so it survives a restart (3.4.1, Q83).

	ownGateOnce sync.Once
	ownGate     *linear.AuthAlertGate
}

// gate returns the shared suppression, or the Worker's own when none is set.
func (w *Worker) gate() *linear.AuthAlertGate {
	if w.AuthGate != nil {
		return w.AuthGate
	}
	w.ownGateOnce.Do(func() { w.ownGate = &linear.AuthAlertGate{} })
	return w.ownGate
}

// sendRound is the state of one SendDue call.
type sendRound struct {
	// alertAuth is set when this call took the shared suppression for an
	// authentication error; the notification is sent once, at the end.
	alertAuth bool
}

// authFailed records an authentication error. The suppression is checked
// and taken in one step (3.4.3 step 3), so however many rows fail in this
// call, and whatever the Poller detects at the same time, one notification
// is sent.
func (w *Worker) authFailed(round *sendRound) {
	if w.gate().Acquire() {
		round.alertAuth = true
	}
}

// alertAuth sends the notification taken by authFailed. Its failure is only
// logged (spec 12 3.5): it changes no task, no outbox row and writes no
// execution event, because the notification has no Execution.
func (w *Worker) alertAuth(ctx context.Context) {
	w.logger().Warn("linear comment authentication failed; alerting the owner", "class", string(linear.ClassAuth))
	if w.Alerter == nil {
		return
	}
	if err := w.Alerter.AlertLinearCommentAuth(context.WithoutCancel(ctx)); err != nil {
		w.logger().Error("linear comment auth alert failed", "error", err)
	}
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
	action, stateName                          string
	attempts                                   int
	// executionID is linear_outbox.execution_id, "" for triggers without
	// an Execution (3.4.2, 3.7).
	executionID string
	// outcomeUnknown is linear_outbox.outcome_unknown (3.4.1, 3.7).
	outcomeUnknown bool
}

// SendDue sends up to batchSize due rows, one at a time, oldest first. No
// DB transaction is held while Linear is called. When the API key file is
// unusable nothing is sent and the rows stay pending (as the Poller does,
// spec 15 6章); the reason is logged by the client, never the key. It counts
// as an authentication error for the shared notification suppression
// (spec 16 3.4.3, Q86).
func (w *Worker) SendDue(ctx context.Context) error {
	if w.DB == nil || w.Client == nil {
		return errors.New("linearfeedback: worker needs a DB and a client")
	}
	var round sendRound
	defer func() {
		if round.alertAuth {
			w.alertAuth(ctx)
		}
	}()
	keyChecked := false
	for range batchSize {
		row, err := w.next(ctx)
		if err != nil || row == nil {
			return err
		}
		if !keyChecked {
			if err := w.Client.CheckKey(); err != nil {
				w.authFailed(&round)
				return nil
			}
			keyChecked = true
		}
		// Stop the batch on any error: if the row could not be updated it
		// is still due, and sending it again in this batch would repeat
		// the comment.
		if err := w.send(ctx, *row, &round); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) next(ctx context.Context) (*outboxRow, error) {
	var r outboxRow
	var executionID sql.NullString
	err := w.DB.QueryRowContext(ctx, `SELECT id, task_id, linear_issue_id, kind, dedupe_key, coalesce(body, ''), attempts,
		outcome_unknown != 0, execution_id, action, coalesce(state_name, '') FROM linear_outbox
		WHERE status = 'pending' AND action IN ('comment', 'set_state')
		AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY created_at, id LIMIT 1`, w.now().Format(timeLayout)).Scan(
		&r.id, &r.taskID, &r.issueID, &r.kind, &r.dedupeKey, &r.body, &r.attempts, &r.outcomeUnknown, &executionID, &r.action, &r.stateName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.executionID = executionID.String
	return &r, nil
}

// send makes one attempt for r. A Linear failure is handled here (retry or
// failed) and logged; only DB errors and shutdown are returned.
//
// The result of each send is tracked in linear_outbox.outcome_unknown
// (3.4.1, Q83):
//   - Before the comment is sent, outcome_unknown is set to 1 and committed.
//   - It goes back to 0 when the result is known: the send succeeded (in the
//     same transaction as status=sent), Linear answered with a failure, or
//     the request failed before a connection was made.
//   - It stays 1 when the connection broke or timed out after it was opened,
//     when shutdown interrupted the send, and when the process died.
//
// A row whose outcome_unknown is 1 is first matched against the Issue's
// comments by dedupe_key. If the comment is there it is not sent again and
// the row is recorded as sent with that comment's ID. If the comments cannot
// be read, nothing is sent (a resend could duplicate the comment), the read
// failure counts as the attempt's failure and outcome_unknown stays 1.
func (w *Worker) send(ctx context.Context, r outboxRow, round *sendRound) error {
	if r.action == "set_state" {
		return w.sendState(ctx, r, round)
	}
	if r.outcomeUnknown {
		commentID, err := w.findSent(ctx, r)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			return w.failAttempt(ctx, r, err, "linear comments not read before resend", true, round)
		}
		if commentID != "" {
			// The comment was sent: the key is valid (3.4.3 step 4).
			w.gate().Release()
			w.logger().Info("linear comment already on the Issue; not sent again", "outbox_id", r.id,
				"task_id", r.taskID, "kind", r.kind)
			return w.markSent(ctx, r, commentID, r.attempts)
		}
	}
	if err := w.markOutcomeUnknown(ctx, r); err != nil {
		return err
	}
	commentID, sendErr := w.Client.CreateComment(ctx, r.issueID, r.body)
	if sendErr == nil {
		// A send succeeded, so the key is valid again: lift the shared
		// suppression (3.4.3 step 4).
		w.gate().Release()
		return w.markSent(ctx, r, commentID, r.attempts+1)
	}
	if ctx.Err() != nil {
		// Shutdown interrupted the send: it is not counted as an attempt,
		// and outcome_unknown stays 1 so the next send checks first.
		return sendErr
	}
	return w.failAttempt(ctx, r, sendErr, "linear comment not sent", linear.OutcomeUnknown(sendErr), round)
}

// markOutcomeUnknown commits outcome_unknown=1 before the comment is sent
// (3.4.1 step 1). No transaction is held while Linear is called.
func (w *Worker) markOutcomeUnknown(ctx context.Context, r outboxRow) error {
	result, err := w.DB.ExecContext(ctx, `UPDATE linear_outbox SET outcome_unknown = 1
		WHERE id = ? AND status = 'pending'`, r.id)
	if err != nil {
		return fmt.Errorf("linearfeedback: mark %s outcome unknown: %w", r.id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("linearfeedback: outbox row %s is no longer pending", r.id))
	}
	return nil
}

// findSent returns the ID of the Issue's comment that carries r's
// dedupe_key on its last line (linearoutbox.Body), or "" when there is none.
func (w *Worker) findSent(ctx context.Context, r outboxRow) (string, error) {
	comments, err := w.Client.IssueComments(ctx, r.issueID)
	if err != nil {
		return "", err
	}
	for _, c := range comments {
		if lastLineIs(c.Body, r.dedupeKey) {
			return c.ID, nil
		}
	}
	return "", nil
}

// lastLineIs reports whether the last non-blank line of body is key. Linear
// returns comment bodies as Markdown and may escape characters such as "_"
// with a backslash, so backslashes are removed before comparing.
func lastLineIs(body, key string) bool {
	body = strings.TrimRight(body, " \t\r\n")
	if i := strings.LastIndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	}
	return strings.TrimSpace(strings.ReplaceAll(body, `\`, "")) == key
}

// failAttempt counts a failed attempt for r: auth errors and removed Issues
// fail at once (6章), the 5th failure fails, and otherwise the row waits
// for the next retry delay (3.4 step 4). unknown is the new
// outcome_unknown: true when the attempt's result is not known (3.4.1).
// A row failed for an authentication error alerts the owner through the
// shared suppression (3.4.3); other failures, including those Linear
// returned after authenticating, leave the suppression as it is.
func (w *Worker) failAttempt(ctx context.Context, r outboxRow, failure error, message string, unknown bool, round *sendRound) error {
	attempts := r.attempts + 1
	class := linear.ClassOf(failure)
	w.logger().Warn(message, "outbox_id", r.id, "task_id", r.taskID,
		"kind", r.kind, "attempt", attempts, "class", string(class),
		"outcome_unknown", unknown)
	switch {
	case class == linear.ClassAuth:
		// 6章: an authentication error is not retried.
		if err := w.markFailed(ctx, r, attempts, unknown, ReasonAuth); err != nil {
			return err
		}
		w.authFailed(round)
		return nil
	case errors.Is(failure, errStateNotFound):
		return w.markFailed(ctx, r, attempts, false, ReasonStateNotFound)
	case errors.Is(failure, linear.ErrIssueNotFound) || w.issueRemoved(ctx, r.issueID):
		// 6章: a deleted or archived Issue fails at once and does not
		// affect the task.
		return w.markFailed(ctx, r, attempts, unknown, ReasonIssueRemoved)
	case attempts >= MaxAttempts:
		return w.markFailed(ctx, r, attempts, unknown, ReasonRetriesExhausted)
	default:
		next := w.now().Add(RetryDelays[attempts-1]).Format(timeLayout)
		_, err := w.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE linear_outbox
			SET attempts = ?, next_attempt_at = ?, outcome_unknown = ? WHERE id = ? AND status = 'pending'`,
			attempts, next, flag(unknown), r.id)
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

func (w *Worker) markSent(ctx context.Context, r outboxRow, commentID string, attempts int) error {
	// Delivery succeeded: record it even during shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return w.finish(ctx, r, `UPDATE linear_outbox SET status = 'sent', attempts = ?, next_attempt_at = NULL,
		linear_comment_id = ?, sent_at = ?, outcome_unknown = 0 WHERE id = ? AND status = 'pending'`,
		[]any{attempts, sql.NullString{String: commentID, Valid: r.action == "comment"}, w.now().Format(time.RFC3339Nano), r.id},
		model.EventNotificationSent, map[string]string{"kind": r.kind, "channel": Channel})
}

// markFailed sets status=failed with failure_reason and failed_at, and
// writes notification_failed with the same reason (3.4.2, Q85). Both happen
// in one transaction. The error text goes nowhere but the log class.
func (w *Worker) markFailed(ctx context.Context, r outboxRow, attempts int, unknown bool, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	w.logger().Error("linear delivery failed", "outbox_id", r.id, "task_id", r.taskID,
		"kind", r.kind, "attempts", attempts, "reason", reason)
	return w.finish(ctx, r, `UPDATE linear_outbox SET status = 'failed', attempts = ?, next_attempt_at = NULL,
		outcome_unknown = ?, failure_reason = ?, failed_at = ? WHERE id = ? AND status = 'pending'`,
		[]any{attempts, flag(unknown), reason, w.now().Format(time.RFC3339Nano), r.id},
		model.EventNotificationFailed, map[string]string{"kind": r.kind, "channel": Channel, "reason": reason})
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
	// linear_outbox.execution_id decides where the outcome is recorded
	// (3.4.2, Q82). Intake holds and the cancellation of a held task have
	// no Execution: a failure is listed from the row itself (status=failed,
	// failure_reason, failed_at), so no event is written and the same
	// failure is never recorded twice.
	if r.executionID != "" {
		raw, _ := json.Marshal(payload)
		if _, err := store.AppendExecutionEvent(ctx, tx, r.executionID, event, raw); err != nil {
			return err
		}
	} else {
		w.logger().Info("linear delivery outcome has no Execution for an event", "outbox_id", r.id,
			"task_id", r.taskID, "kind", r.kind, "event", string(event))
	}
	return tx.Commit()
}

// flag is the INTEGER value of a boolean column.
func flag(b bool) int {
	if b {
		return 1
	}
	return 0
}
