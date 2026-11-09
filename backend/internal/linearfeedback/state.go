package linearfeedback

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// sendState shares delivery outcomes and retries with comments, but never
// marks outcome_unknown or reconciles comments: setting a state is idempotent
// (spec 16 3.6.3). Merely reading a Team or an Issue does not lift auth suppression.
func (w *Worker) sendState(ctx context.Context, r outboxRow, round *sendRound) error {
	err := w.setState(ctx, r)
	if err == nil {
		w.gate().Release()
		return w.markSent(ctx, r, "", r.attempts+1)
	}
	if ctx.Err() != nil {
		return err
	}
	return w.failAttempt(ctx, r, err, "linear state not set", false, round)
}

func (w *Worker) setState(ctx context.Context, r outboxRow) error {
	issue, err := w.Client.IssueState(ctx, r.issueID)
	if err != nil {
		return err
	}
	if issue.Archived || issue.Trashed {
		return linear.ErrIssueNotFound
	}
	// Read current state on every attempt, including after a lost response.
	// Never reopen a terminal Issue, even if the requested name is absent.
	if issue.State.Name == r.stateName || issue.State.Type == "completed" || issue.State.Type == "canceled" {
		return nil
	}
	// No cache is needed. A missing name always causes a second, fresh read
	// before failing; a read failure follows the ordinary retry policy.
	for range 2 {
		states, err := w.Client.TeamStates(ctx, issue.TeamID)
		if err != nil {
			return err
		}
		for _, state := range states {
			if state.Name == r.stateName {
				return w.Client.SetIssueState(ctx, r.issueID, state.ID)
			}
		}
	}
	return errStateNotFound
}
