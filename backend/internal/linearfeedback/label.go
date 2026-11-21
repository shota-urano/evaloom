package linearfeedback

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
)

// sendLabel shares the retry policy and auth suppression with the other
// actions, but never marks outcome_unknown: addition is idempotent (3.6.5).
func (w *Worker) sendLabel(ctx context.Context, r outboxRow, round *sendRound) error {
	err := w.addLabel(ctx, r)
	if err == nil {
		w.gate().Release()
		return w.markSent(ctx, r, "", r.attempts+1)
	}
	if ctx.Err() != nil {
		return err
	}
	return w.failAttempt(ctx, r, err, "linear label not added", false, round)
}

func (w *Worker) labelAllowed(ctx context.Context, label string) (bool, error) {
	var exists bool
	var triggerJSON, prefixJSON string
	// Read the route and both settings from one snapshot, on every attempt.
	err := w.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM routes WHERE label = ?),
		coalesce((SELECT value FROM settings WHERE key = 'linear.triggerLabel'), 'null'),
		coalesce((SELECT value FROM settings WHERE key = 'linear.routingLabelPrefix'), 'null')`,
		label).Scan(&exists, &triggerJSON, &prefixJSON)
	if err != nil {
		return false, err
	}
	labels := linearintake.DefaultLabels()
	for _, setting := range []struct {
		raw    string
		target *string
	}{{triggerJSON, &labels.TriggerLabel}, {prefixJSON, &labels.RoutingLabelPrefix}} {
		var value string
		if json.Unmarshal([]byte(setting.raw), &value) == nil && strings.TrimSpace(value) != "" {
			*setting.target = value
		}
	}
	// Evaloom never adds ai-ready, even if settings later give another
	// label the trigger role (spec 16 3.5, 3.6.5).
	return exists && label != linearintake.DefaultTriggerLabel && label != labels.TriggerLabel &&
		strings.HasPrefix(label, labels.RoutingLabelPrefix), nil
}

func (w *Worker) addLabel(ctx context.Context, r outboxRow) error {
	allowed, err := w.labelAllowed(ctx, r.labelName)
	if err != nil {
		return err
	}
	if !allowed {
		return errLabelNotAllowed
	}
	issue, err := w.Client.Issue(ctx, r.issueID)
	if err != nil {
		return err
	}
	if issue.Archived || issue.Trashed {
		return linear.ErrIssueNotFound
	}
	for _, name := range issue.Labels {
		if name == r.labelName {
			return nil
		}
	}
	labels, err := w.Client.TeamLabels(ctx, issue.TeamID)
	if err != nil {
		return err
	}
	for _, label := range labels {
		if label.Name == r.labelName {
			// The network reads above can take time. Recheck immediately
			// before the mutation as well (spec 16 3.6.5 step 2).
			allowed, err := w.labelAllowed(ctx, r.labelName)
			if err != nil {
				return err
			}
			if !allowed {
				return errLabelNotAllowed
			}
			return w.Client.AddIssueLabel(ctx, r.issueID, label.ID)
		}
	}
	return errLabelNotFound
}
