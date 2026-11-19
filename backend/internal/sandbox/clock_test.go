package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Subprocess startup uses real time. Only the execution budget advances when
// the test has observed the state whose timeout behavior it wants to exercise.
type manualClock struct {
	mu        sync.Mutex
	current   time.Time
	deadlines []*manualDeadline
}

type manualDeadline struct {
	context.Context
	values context.Context
	limit  time.Time
	expire context.CancelCauseFunc
}

func newManualClock(c *Container) *manualClock {
	// Keep execution deadlines beyond real control-command deadlines, so
	// context.WithTimeout continues to bound each actual CLI subprocess.
	clock := &manualClock{current: time.Now().Add(24 * time.Hour)}
	c.clock = clock
	return clock
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *manualClock) withDeadline(parent context.Context, limit time.Time) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && deadline.Before(limit) {
		limit = deadline
	}
	ctx, cancel := context.WithCancelCause(parent)
	d := &manualDeadline{Context: ctx, values: context.WithoutCancel(parent), limit: limit, expire: cancel}
	c.mu.Lock()
	c.deadlines = append(c.deadlines, d)
	expired := !c.current.Before(limit)
	c.mu.Unlock()
	if expired {
		cancel(context.DeadlineExceeded)
	}
	return d, func() { cancel(context.Canceled) }
}

func (d *manualDeadline) Deadline() (time.Time, bool) { return d.limit, true }

func (d *manualDeadline) Err() error {
	if d.Context.Err() != nil && context.Cause(d.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return d.Context.Err()
}

// Hide the underlying cancel context so descendants propagate the wrapper's
// DeadlineExceeded Err, rather than the underlying context's Canceled Err.
func (d *manualDeadline) Value(key any) any { return d.values.Value(key) }

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	var expired []*manualDeadline
	for _, deadline := range c.deadlines {
		if !c.current.Before(deadline.limit) {
			expired = append(expired, deadline)
		}
	}
	c.mu.Unlock()
	for _, deadline := range expired {
		deadline.expire(context.DeadlineExceeded)
	}
}

// Assert the budget starts before preparation and survives the controlled
// ownership transfer unchanged. Advance to just before expiry first, so a
// premature deadline cannot satisfy the workload-stop assertions.
func (c *manualClock) beforeDeadline(t *testing.T, start time.Time, budget time.Duration) {
	t.Helper()
	c.advance(budget - time.Nanosecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.deadlines) == 0 {
		t.Fatal("execution deadline was not installed")
	}
	for _, d := range c.deadlines {
		if !d.limit.Equal(start.Add(budget)) {
			t.Fatalf("execution deadline = %v, want %v", d.limit, start.Add(budget))
		}
		if d.Err() == context.DeadlineExceeded {
			t.Fatal("execution deadline fired before its budget elapsed")
		}
	}
}
