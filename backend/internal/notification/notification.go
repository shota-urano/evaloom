// Package notification delivers the Control Plane's durable lifecycle requests.
package notification

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Notification contains only the metadata allowed by spec 12 3.1, never output
// URLs, decision detail, approval targets, findings, Context or logs.
type Notification struct {
	ExecutionID     string
	TaskID          string
	Title           string
	Status          model.ExecutionStatus
	ErrorClass      model.ErrorClass
	Reason          string
	Stage           model.ExecutionStatus
	DecisionID      string
	DecisionKind    model.DecisionKind
	DecisionCause   model.DecisionCause
	OutputLabel     string
	OutputRemaining int
	Body            string
}

type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Channel names the transport in notification_sent events. Registration is
// Control Plane configuration, never Runner input.
type Channel struct {
	Name     string
	Notifier Notifier
}
