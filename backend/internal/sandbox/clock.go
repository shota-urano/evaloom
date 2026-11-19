package sandbox

import (
	"context"
	"time"
)

// executionClock controls only the execution budget and its termination grace.
// Runtime control commands retain independent, real-time upper bounds.
type executionClock interface {
	now() time.Time
	withDeadline(context.Context, time.Time) (context.Context, context.CancelFunc)
}

type wallClock struct{}

func (wallClock) now() time.Time { return time.Now() }

func (wallClock) withDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, deadline)
}
