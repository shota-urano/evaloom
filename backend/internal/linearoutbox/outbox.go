// Package linearoutbox writes linear_outbox rows (spec 16 3.4 step 1, 3.7):
// one row per Linear comment or Workflow state change, in the same transaction
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

// Kind is linear_outbox.kind: a trigger of spec 16 3.2 or 3.6.1.
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
	KindStarted                   Kind = "started"
	KindCompleted                 Kind = "completed"
)

// Trigger is one occurrence of a 3.2 trigger. TargetID is the "対象の ID" of
// the dedupe_key <task_id>:<kind>:<対象の ID> (3.7): the Execution ID for
// Execution triggers, the intake_block_cause for intake holds, the task ID
// for a cancelled held task without an Execution.
type Trigger struct {
	TaskID   string
	Kind     Kind
	TargetID string
	// ExecutionID is linear_outbox.execution_id: the Execution of the
	// trigger, or "" for triggers without one (intake_blocked,
	// missing_acceptance_criteria, the cancellation of a held task). The
	// Worker writes notification_failed only for rows with an Execution;
	// failed rows without one are listed from linear_outbox (spec 16 3.4.2,
	// Q82).
	ExecutionID string
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
	return enqueueTx(ctx, tx, t, "")
}

// EnqueueStateTx writes a Workflow state change (spec 16 3.6.1, 3.6.3).
// Only a newly inserted trigger supersedes pending state changes; replaying
// an old trigger must not skip a newer one. Comments are never superseded.
func EnqueueStateTx(ctx context.Context, tx *sql.Tx, t Trigger) error {
	var name string
	switch t.Kind {
	case KindStarted:
		name = "In Progress"
	case KindWaitingApproval:
		name = "PR Review"
	case KindCompleted:
		name = "Done"
	case KindCancelled:
		name = "Canceled"
	default:
		return fmt.Errorf("linearoutbox: unsupported state trigger %q", t.Kind)
	}
	return enqueueTx(ctx, tx, t, name)
}

func enqueueTx(ctx context.Context, tx *sql.Tx, t Trigger, stateName string) error {
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
	if t.ExecutionID != "" {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM executions WHERE id = ?`, t.ExecutionID).Scan(&owner)
		if err != nil || owner != t.TaskID {
			return errors.Join(err, fmt.Errorf("linearoutbox: execution %s is not of task %s", t.ExecutionID, t.TaskID))
		}
	}
	action, key := "comment", t.DedupeKey()
	var body, state sql.NullString
	if stateName == "" {
		body.String, err = Body(ctx, tx, t)
		if err != nil {
			return err
		}
		body.Valid = true
	} else {
		action = "set_state"
		state = sql.NullString{String: stateName, Valid: true}
		key = t.TaskID + ":state_" + string(t.Kind) + ":" + t.TargetID
	}
	now := time.Now().UTC()
	id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		return err
	}
	executionID := sql.NullString{String: t.ExecutionID, Valid: t.ExecutionID != ""}
	result, err := tx.ExecContext(ctx, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind,
		dedupe_key, body, status, attempts, created_at, execution_id, action, state_name) VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		id.String(), t.TaskID, issueID.String, t.Kind, key, body, now.Format(time.RFC3339Nano), executionID, action, state)
	if err != nil {
		return fmt.Errorf("linearoutbox: insert: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if stateName != "" && inserted == 1 {
		_, err := tx.ExecContext(ctx, `UPDATE linear_outbox SET status = 'skipped'
			WHERE task_id = ? AND action = 'set_state' AND status = 'pending' AND id != ?`, t.TaskID, id.String())
		return err
	}
	return nil
}

// EnqueueRouteLabelTx writes the routing label addition of spec 16 3.6.5 for
// an approved route decision: action=add_label, kind=route_approved, the
// approved label, dedupe_key <task_id>:label:<decision_id>. The table's
// triggers reject a label that is not in routes or is the trigger label. A
// manual task writes nothing.
func EnqueueRouteLabelTx(ctx context.Context, tx *sql.Tx, taskID, decisionID, label string) error {
	if taskID == "" || decisionID == "" || label == "" {
		return errors.New("linearoutbox: label addition needs a task, decision and label")
	}
	var source string
	var issueID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT t.source,
		(SELECT linear_issue_id FROM linear_issue_links WHERE task_id = t.id)
		FROM tasks t WHERE t.id = ?`, taskID).Scan(&source, &issueID)
	if err != nil {
		return fmt.Errorf("linearoutbox: read task %s: %w", taskID, err)
	}
	if source != "linear" {
		return nil
	}
	if !issueID.Valid {
		return fmt.Errorf("linearoutbox: linear task %s has no linked Issue", taskID)
	}
	now := time.Now().UTC()
	id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind,
		dedupe_key, status, attempts, created_at, action, label_name) VALUES (?, ?, ?, 'route_approved', ?, 'pending', 0, ?, 'add_label', ?)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		id.String(), taskID, issueID.String, taskID+":label:"+decisionID, now.Format(time.RFC3339Nano), label)
	if err != nil {
		return fmt.Errorf("linearoutbox: insert label: %w", err)
	}
	return nil
}
