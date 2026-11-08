package linearintake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Decision is a value of linear_issue_states.last_decision (spec 15 3.8).
type Decision string

const (
	DecisionStarted           Decision = "started"
	DecisionHeld              Decision = "held"
	DecisionCancelled         Decision = "cancelled"
	DecisionSkippedActiveTask Decision = "skipped_active_task"
	DecisionSkippedInitial    Decision = "skipped_initial"
	DecisionSkippedCancelled  Decision = "skipped_cancelled"
	DecisionSkippedRestored   Decision = "skipped_restored"
)

// CancelReason is the audit reason of a Linear-caused cancellation (spec 15
// 3.4 and 3.7).
type CancelReason string

const (
	ReasonLabelRemoved   CancelReason = "linear_label_removed"
	ReasonIssueCancelled CancelReason = "linear_issue_cancelled"
	ReasonIssueRemoved   CancelReason = "linear_issue_removed"
	ReasonIssueMoved     CancelReason = "linear_issue_moved"
)

// IssueFetcher re-reads one Issue that left the list (spec 15 3.4). It is
// the subset of *linear.Client used here; ErrIssueNotFound means deleted.
type IssueFetcher interface {
	Issue(ctx context.Context, id string) (linear.Issue, error)
}

// Starter performs routing and task creation (spec 15 3.6) for one detected
// attach. It runs inside tx, which also updates linear_issue_states, so a
// failure leaves the previous record unchanged (3.5 rule 1). It must use
// idempotency key linear:<Issue ID>:<attachSeq> and write linear_issue_links,
// and returns DecisionStarted or DecisionHeld.
type Starter interface {
	StartLinearTask(ctx context.Context, tx *sql.Tx, issue linear.Issue, attachSeq int) (Decision, error)
}

// Canceller stops the Issue's non-terminal tasks (spec 15 3.7) inside tx.
// Terminal tasks must be left alone.
type Canceller interface {
	CancelLinearTasks(ctx context.Context, tx *sql.Tx, issueID string, reason CancelReason) error
}

// HeldRechecker re-runs the 3.6 decision for a held task (spec 15 3.6.3). It
// is called inside tx for an Issue that stays attached and whose updatedAt
// is newer than the recorded one or that carries the recheck mark of an
// Evaloom setting change (Q81); it must not create a new task. It returns
// the new last_decision (DecisionStarted or DecisionHeld), or "" when nothing
// changed (no held task, or the same cause), which keeps the previous one.
type HeldRechecker interface {
	RecheckHeldTask(ctx context.Context, tx *sql.Tx, issue linear.Issue) (Decision, error)
}

// Intake decides attach and detach (spec 15 3.4) and implements
// IssueHandler. Starter and Canceller are required: recording an attach
// without acting on it would lose the attach for good.
type Intake struct {
	DB           *sql.DB
	Fetcher      IssueFetcher
	Starter      Starter
	Canceller    Canceller
	Rechecker    HeldRechecker // nil: held tasks are not rechecked
	TriggerLabel string        // empty: the Poller's round settings, then DefaultTriggerLabel
	Logger       *slog.Logger
	Now          func() time.Time
}

var _ IssueHandler = (*Intake)(nil)

// HandleTeamIssues compares one Team's complete list with
// linear_issue_states. Each Issue is decided and written in its own
// transaction; a failed Issue keeps its previous record and is decided again
// next round (spec 15 3.5, 6章), while the other Issues proceed.
func (in *Intake) HandleTeamIssues(ctx context.Context, team store.LinearPollTeam, issues []linear.Issue) error {
	if in.DB == nil || in.Fetcher == nil || in.Starter == nil || in.Canceller == nil {
		return errors.New("linearintake: intake is not configured")
	}
	var errs error
	listed := make(map[string]bool, len(issues))
	baselineOK := true
	for _, issue := range issues {
		if ctx.Err() != nil {
			return errors.Join(errs, ctx.Err())
		}
		listed[issue.ID] = true
		if err := in.handleListed(ctx, team, issue); err != nil {
			baselineOK = false
			errs = errors.Join(errs, fmt.Errorf("linearintake: issue %s: %w", issue.ID, err))
		}
	}
	// The first poll is done only when every listed Issue was recorded;
	// otherwise the next round is treated as the first poll again (Q64).
	if !team.BaselineDone && baselineOK {
		if err := store.MarkLinearBaselineDone(ctx, in.DB, team.TeamID, in.now()); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	attached, err := store.ListAttachedLinearIssues(ctx, in.DB, team.TeamID)
	if err != nil {
		return errors.Join(errs, err)
	}
	for _, state := range attached {
		if listed[state.LinearIssueID] {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(errs, ctx.Err())
		}
		if err := in.handleMissing(ctx, state.LinearIssueID); err != nil {
			errs = errors.Join(errs, fmt.Errorf("linearintake: issue %s: %w", state.LinearIssueID, err))
		}
	}
	return errs
}

// handleListed applies the first table of spec 15 3.4 to an Issue in the list.
func (in *Intake) handleListed(ctx context.Context, team store.LinearPollTeam, issue linear.Issue) error {
	return in.inTx(ctx, func(tx *sql.Tx) error {
		prev, ok, err := store.GetLinearIssueState(ctx, tx, issue.ID)
		if err != nil {
			return err
		}
		now := in.stamp()
		next := prev
		if !ok {
			next = store.LinearIssueState{LinearIssueID: issue.ID, FirstSeenAt: now}
		}
		next.IssueIdentifier, next.TeamID = issue.Identifier, issue.TeamID
		next.IssueUpdatedAt, next.LastSeenAt = stamp(issue.UpdatedAt), now
		decide := func(d Decision) { next.LastDecision, next.LastDecisionAt = string(d), now }
		// The recheck mark (3.6.3, Q81) is consumed by this decision in the
		// same transaction, whichever row of the 3.4 table applies.
		marked := prev.RecheckRequestedAt != ""
		next.RecheckRequestedAt = ""

		switch {
		case prev.RemovedAt != "":
			// A deleted or archived Issue came back, cancelled or not:
			// record it as attached without starting (3.4, Q78). attach_seq
			// is kept, so removing and re-adding ai-ready later starts a
			// task with a new idempotency key.
			next.AIReady = true
			next.RemovedAt = ""
			decide(DecisionSkippedRestored)
		case !next.AIReady && !team.BaselineDone:
			// First poll of the Team: record only, never start (3.4, Q64).
			next.AIReady = true
			decide(DecisionSkippedInitial)
		case !next.AIReady && issue.Canceled():
			next.AIReady = true
			decide(DecisionSkippedCancelled)
		case !next.AIReady:
			// Attach detected (3.4). attach_seq counts every detection.
			next.AIReady = true
			next.AttachSeq++
			active, err := store.HasActiveLinearTask(ctx, tx, issue.ID)
			if err != nil {
				return err
			}
			if active {
				decide(DecisionSkippedActiveTask) // 3.5 rule 3, 6章
				break
			}
			d, err := in.Starter.StartLinearTask(ctx, tx, issue, next.AttachSeq)
			if err != nil {
				return err
			}
			decide(d)
		case issue.Canceled():
			// Still attached but cancelled: stop once, not every round. An
			// Issue first seen cancelled (skipped_cancelled) never started,
			// and a restored Issue (skipped_restored) was already stopped
			// when it was removed (3.7) and started nothing since.
			if d := Decision(prev.LastDecision); d != DecisionCancelled && d != DecisionSkippedCancelled &&
				d != DecisionSkippedRestored {
				if err := in.Canceller.CancelLinearTasks(ctx, tx, issue.ID, ReasonIssueCancelled); err != nil {
					return err
				}
				decide(DecisionCancelled)
			}
		default:
			// Still attached: body, label or state edits never start again
			// (requirements 6.2). Only a held task may be rechecked (3.6.3):
			// when the Issue was updated, or when an Evaloom setting change
			// marked it (Q81), using the Issue as listed in this round.
			if in.Rechecker != nil && (marked || newer(issue.UpdatedAt, prev.IssueUpdatedAt)) {
				d, err := in.Rechecker.RecheckHeldTask(ctx, tx, issue)
				if err != nil {
					return err
				}
				if d != "" {
					decide(d)
				}
			}
		}
		return store.SaveLinearIssueState(ctx, tx, next)
	})
}

// handleMissing applies the second table of spec 15 3.4 to an Issue that was
// attached in the previous poll but is not in this list.
func (in *Intake) handleMissing(ctx context.Context, issueID string) error {
	// Fetch outside the transaction: the store has a single connection.
	issue, err := in.Fetcher.Issue(ctx, issueID)
	removed := errors.Is(err, linear.ErrIssueNotFound)
	if err != nil && !removed {
		// Undecided: keep the row and check again next round (6章).
		in.logger().Warn("linear issue refetch failed", "issue", issueID, "class", string(linear.ClassOf(err)))
		return nil
	}
	label := in.TriggerLabel
	if label == "" {
		label = roundLabels(ctx).TriggerLabel
	}
	return in.inTx(ctx, func(tx *sql.Tx) error {
		prev, ok, err := store.GetLinearIssueState(ctx, tx, issueID)
		if err != nil || !ok || !prev.AIReady {
			return err
		}
		now := in.stamp()
		next := prev
		next.LastDecision, next.LastDecisionAt = string(DecisionCancelled), now
		next.RecheckRequestedAt = "" // the Issue left the list: 3.4 and 3.7 decide (3.6.3, Q81)
		if !removed {
			next.IssueIdentifier, next.IssueUpdatedAt, next.LastSeenAt = issue.Identifier, stamp(issue.UpdatedAt), now
		}
		var reason CancelReason
		switch {
		case removed || issue.Archived || issue.Trashed:
			// Keep the row with the removed mark and attach_seq (3.4, Q78).
			reason = ReasonIssueRemoved
			next.AIReady = false
			next.RemovedAt = now
		case !slices.Contains(issue.Labels, label):
			reason = ReasonLabelRemoved
			next.AIReady = false
		case issue.Canceled():
			if prev.LastDecision == string(DecisionCancelled) {
				// Already stopped; not every round. Only a recheck mark
				// is cleared (3.6.3, Q81).
				if prev.RecheckRequestedAt == "" {
					return nil
				}
				cleared := prev
				cleared.RecheckRequestedAt = ""
				return store.SaveLinearIssueState(ctx, tx, cleared)
			}
			reason = ReasonIssueCancelled
		default:
			polled, err := store.IsLinearTeamPolled(ctx, tx, issue.TeamID)
			if err != nil {
				return err
			}
			if polled {
				// Still attached in another registered Team: that Team's
				// list decides it (spec 15 3.4 does not cancel this case).
				return nil
			}
			reason = ReasonIssueMoved
			next.AIReady = false
			next.TeamID = issue.TeamID
		}
		if err := in.Canceller.CancelLinearTasks(ctx, tx, issueID, reason); err != nil {
			return err
		}
		return store.SaveLinearIssueState(ctx, tx, next)
	})
}

func (in *Intake) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := in.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// newer reports whether t is later than the recorded RFC 3339 time. An
// unreadable record counts as older so the recheck is not skipped.
func newer(t time.Time, recorded string) bool {
	prev, err := time.Parse(time.RFC3339Nano, recorded)
	return err != nil || t.After(prev)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (in *Intake) stamp() string { return stamp(in.now()) }

func (in *Intake) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in *Intake) logger() *slog.Logger {
	if in.Logger != nil {
		return in.Logger
	}
	return slog.Default()
}
