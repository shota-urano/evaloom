package linearintake

import (
	"context"
	"sync"
)

// Labels are the global settings linear.triggerLabel and
// linear.routingLabelPrefix (spec 15 3.4 and 5章, Q48).
type Labels struct {
	TriggerLabel       string `json:"triggerLabel"`
	RoutingLabelPrefix string `json:"routingLabelPrefix"`
}

// DefaultLabels are the defaults of the global settings: requirements 6.2's
// recommended ai-ready and repo: (spec 15 3.4, Q48).
func DefaultLabels() Labels {
	return Labels{TriggerLabel: DefaultTriggerLabel, RoutingLabelPrefix: DefaultRoutingLabelPrefix}
}

// LabelSource supplies the current global label settings. The Poller reads
// it once per round.
type LabelSource interface {
	LinearLabels() Labels
}

// LabelSettings holds the label settings shared by the settings API and the
// Poller. Spec 15 does not define where the global settings are stored, so
// this keeps them in the Control Plane process: a restart returns to the
// defaults.
type LabelSettings struct {
	mu     sync.RWMutex
	labels Labels
}

// NewLabelSettings starts with DefaultLabels.
func NewLabelSettings() *LabelSettings {
	return &LabelSettings{labels: DefaultLabels()}
}

// LinearLabels returns the current values.
func (s *LabelSettings) LinearLabels() Labels {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.labels
}

// SetLinearLabels replaces the values. The caller validates them.
func (s *LabelSettings) SetLinearLabels(labels Labels) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels = labels
}

type roundLabelsKey struct{}

// withRoundLabels carries the labels the Poller read at the start of a round
// to Intake and TaskStarter, so one round lists, detaches and routes with the
// same values even if the settings change meanwhile.
func withRoundLabels(ctx context.Context, labels Labels) context.Context {
	return context.WithValue(ctx, roundLabelsKey{}, labels)
}

// roundLabels returns the labels the Poller put in ctx, or the defaults.
// Empty values fall back to the defaults one by one.
func roundLabels(ctx context.Context) Labels {
	labels, _ := ctx.Value(roundLabelsKey{}).(Labels)
	if labels.TriggerLabel == "" {
		labels.TriggerLabel = DefaultTriggerLabel
	}
	if labels.RoutingLabelPrefix == "" {
		labels.RoutingLabelPrefix = DefaultRoutingLabelPrefix
	}
	return labels
}
