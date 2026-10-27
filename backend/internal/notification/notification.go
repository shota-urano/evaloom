// Package notification delivers the Control Plane's durable lifecycle requests.
package notification

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Notification contains persisted task metadata, not task bodies, Context or
// logs. Each transport must choose what it may display; macOS must restrict its
// visible content to the title, task ID and classification (spec 12, 3.3).
type Notification struct {
	ExecutionID string
	TaskID      string
	Title       string
	Status      model.ExecutionStatus
	PRURL       string
	ErrorClass  model.ErrorClass
	Reason      string
	Stage       model.ExecutionStatus
	BlockCause  model.BlockCause
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
