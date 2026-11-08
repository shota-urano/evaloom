// Package linearoutbox writes linear_outbox rows (spec 16 3.4 step 1, 3.7):
// one row per Linear comment, in the same transaction as the state change
// that causes it. Sending (3.4 steps 2-5) is not done here.
//
// This package must not import store or task: task calls it from the shared
// transition path, and store imports task.
package linearoutbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Kind is linear_outbox.kind: a trigger of spec 16 3.2.
type Kind string

const (
	KindIntakeBlocked             Kind = "intake_blocked"
	KindMissingAcceptanceCriteria Kind = "missing_acceptance_criteria"
	KindBlocked                   Kind = "blocked"
	KindWaitingApproval           Kind = "waiting_approval"
	KindSucceeded                 Kind = "succeeded"
	KindFailed                    Kind = "failed"
	KindMerged                    Kind = "merged"
	KindCancelled                 Kind = "cancelled"
)

// Trigger is one occurrence of a 3.2 trigger. TargetID is the "対象の ID" of
// the dedupe_key <task_id>:<kind>:<対象の ID> (3.7): the Execution ID for
// Execution triggers, the intake_block_cause for intake holds, the task ID
// for a cancelled held task without an Execution.
type Trigger struct {
	TaskID   string
	Kind     Kind
	TargetID string
	// RoutingLabelPrefix is the routing label prefix (spec 15 3.4) the
	// intake decision used. Only intake hold comments show it; it is not
	// part of the dedupe_key.
	RoutingLabelPrefix string
}

// DedupeKey is linear_outbox.dedupe_key (spec 16 3.7).
func (t Trigger) DedupeKey() string {
	return t.TaskID + ":" + string(t.Kind) + ":" + t.TargetID
}

// EnqueueTx writes one pending row for t inside tx when the task came from
// Linear (source=linear). A manual task writes nothing (spec 16 3.1, 7). A
// second occurrence of the same trigger is ignored by UNIQUE(dedupe_key).
// The caller must roll back tx on any error.
func EnqueueTx(ctx context.Context, tx *sql.Tx, t Trigger) error {
	if t.TaskID == "" || t.Kind == "" || t.TargetID == "" {
		return errors.New("linearoutbox: trigger needs a task, kind and target")
	}
	var source string
	var issueID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT t.source,
		(SELECT linear_issue_id FROM linear_issue_links WHERE task_id = t.id)
		FROM tasks t WHERE t.id = ?`, t.TaskID).Scan(&source, &issueID)
	if err != nil {
		return fmt.Errorf("linearoutbox: read task %s: %w", t.TaskID, err)
	}
	if source != "linear" {
		return nil
	}
	// A Linear task and its link are written in one transaction (spec 15
	// 3.6), so a missing link is inconsistent data, not a manual task.
	if !issueID.Valid {
		return fmt.Errorf("linearoutbox: linear task %s has no linked Issue", t.TaskID)
	}
	body, err := Body(ctx, tx, t)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind,
		dedupe_key, body, status, attempts, created_at) VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, ?)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		id.String(), t.TaskID, issueID.String, t.Kind, t.DedupeKey(), body, now.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("linearoutbox: insert: %w", err)
	}
	return nil
}
