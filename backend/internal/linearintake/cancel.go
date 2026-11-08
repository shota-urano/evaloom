package linearintake

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// TaskCanceller implements Canceller (spec 15 3.7) on the owner's
// cancellation path (spec 04 3.5, store.CancelTaskTx): a queued or blocked
// task becomes cancelled, and a running or verifying task gets a cancel
// request that the Runner receives on its next heartbeat. A held task
// (blocked without an Execution) becomes cancelled. audit_logs records actor
// system and the 3.4 reason. Terminal tasks are left alone.
type TaskCanceller struct{}

var _ Canceller = TaskCanceller{}

// CancelLinearTasks stops the Issue's non-terminal tasks inside tx. Any
// failure is returned so the caller rolls back and the Issue is decided
// again next round (spec 15 3.5, 6章).
func (TaskCanceller) CancelLinearTasks(ctx context.Context, tx *sql.Tx, issueID string, reason CancelReason) error {
	ids, err := store.ListCancellableLinearTasks(ctx, tx, issueID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := store.CancelTaskTx(ctx, tx, store.TaskCancellation{
			TaskID: id, Actor: "system", Reason: string(reason),
		}); err != nil {
			return fmt.Errorf("linearintake: cancel task %s: %w", id, err)
		}
	}
	return nil
}
