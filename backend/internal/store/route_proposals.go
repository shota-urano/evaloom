package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// ErrRouteProposalRunnerMismatch is returned when the reporting Runner is not
// the one the proposal was handed to.
var ErrRouteProposalRunnerMismatch = errors.New("store: route proposal belongs to another Runner")

// maxProposalReason is the limit of route_proposals.reason (spec 02 3.4).
const maxProposalReason = 2000

// RouteProposalResult is one POST /api/route-proposals/{id}/result: the label
// and reason the AI chose, or the Runner's error class (spec 15 3.6.4).
type RouteProposalResult struct {
	ProposalID, RunnerID string
	Label, Reason        *string
	ErrorClass           *string
}

type routeProposalRow struct {
	taskID, status                    string
	runnerID, label, reason, errClass sql.NullString
	decisionID                        sql.NullString
	candidates                        string
}

func readRouteProposal(ctx context.Context, tx *sql.Tx, id string) (routeProposalRow, error) {
	var p routeProposalRow
	err := tx.QueryRowContext(ctx, `SELECT task_id, status, runner_id, proposed_label, reason, error_class, decision_id, candidates
		FROM route_proposals WHERE id = ?`, id).Scan(&p.taskID, &p.status, &p.runnerID, &p.label, &p.reason, &p.errClass, &p.decisionID, &p.candidates)
	return p, err
}

func sameNullable(stored sql.NullString, value *string) bool {
	if value == nil {
		return !stored.Valid
	}
	return stored.Valid && stored.String == *value
}

// ReportRouteProposalResultTx applies spec 04 3.7 and 15 3.6.4 to a proposal
// result: a label from the candidates with a reason makes the proposal
// proposed; anything else makes it failed (a label outside the candidates or
// an over-long reason is contract_error). Either way a route decision is
// opened while the task is still routing. A result arriving after the
// proposal was cancelled or superseded is only recorded (15 3.6.4). The same
// result sent again is accepted without writes; a different one is a
// conflict. The caller owns tx and must roll back on any error.
func ReportRouteProposalResultTx(ctx context.Context, tx *sql.Tx, in RouteProposalResult) error {
	p, err := readRouteProposal(ctx, tx, in.ProposalID)
	if err != nil {
		return err
	}
	if !p.runnerID.Valid || p.runnerID.String != in.RunnerID {
		return ErrRouteProposalRunnerMismatch
	}
	status, label, reason, errorClass := model.ProposalProposed, in.Label, in.Reason, in.ErrorClass
	if errorClass != nil {
		status = model.ProposalFailed
	} else {
		var candidates []struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal([]byte(p.candidates), &candidates); err != nil {
			return fmt.Errorf("store: proposal %s candidates cannot be read: %w", in.ProposalID, err)
		}
		listed := false
		for _, c := range candidates {
			listed = listed || (label != nil && c.Label == *label)
		}
		if !listed || reason == nil || utf8.RuneCountInString(*reason) > maxProposalReason {
			// The label is discarded unless it is a candidate (spec 02 3.4).
			contractError := string(model.ErrorContract)
			status, label, reason, errorClass = model.ProposalFailed, nil, nil, &contractError
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	switch model.RouteProposalStatus(p.status) {
	case model.ProposalRunning:
	case model.ProposalCancelled, model.ProposalSuperseded:
		if p.label.Valid || p.reason.Valid || p.errClass.Valid {
			if sameNullable(p.label, label) && sameNullable(p.reason, reason) && sameNullable(p.errClass, errorClass) {
				return nil
			}
			return conflictf("Route proposal already has a different result")
		}
		_, err := tx.ExecContext(ctx, `UPDATE route_proposals SET proposed_label = ?, reason = ?, error_class = ?,
			finished_at = coalesce(finished_at, ?) WHERE id = ?`, label, reason, errorClass, now, in.ProposalID)
		return err
	case model.ProposalProposed, model.ProposalFailed, model.ProposalDecided:
		if sameNullable(p.label, label) && sameNullable(p.reason, reason) && sameNullable(p.errClass, errorClass) {
			return nil
		}
		return conflictf("Route proposal already has a different result")
	default:
		return conflictf("Route proposal is not running")
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = ?, proposed_label = ?, reason = ?, error_class = ?, finished_at = ?
		WHERE id = ? AND status = 'running'`, status, label, reason, errorClass, now, in.ProposalID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	var taskStatus model.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, p.taskID).Scan(&taskStatus); err != nil {
		return err
	}
	if taskStatus != model.TaskRouting {
		return nil
	}
	return OpenRouteDecisionTx(ctx, tx, p.taskID, in.ProposalID)
}

// OpenRouteDecisionTx opens the kind=route decision of a proposed or failed
// proposal (spec 04 3.7, 02 3.4): subject_id and basis are the proposal ID,
// and the proposal records the decision.
func OpenRouteDecisionTx(ctx context.Context, tx *sql.Tx, taskID, proposalID string) error {
	id := ulid.Make().String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_decisions (id, kind, task_id, subject_id, basis, detail, status, created_at)
		VALUES (?, 'route', ?, ?, ?, '{}', 'open', ?)`, id, taskID, proposalID, proposalID,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET decision_id = ? WHERE id = ? AND task_id = ?`, id, proposalID, taskID)
	return expectOneRow(result, err)
}

// ApplyRouteAnswerTx performs the route rows of spec 04 3.6 and 3.7. The
// caller has checked the decision is open, the answer's shape and basis, and
// that the approved label is in routes; it writes the answer and its audit.
//
// approve: the task gets the label's destination, a seq=1 continue pending
// Execution and becomes queued; the proposal becomes decided and the label
// addition is written to linear_outbox. reject: the proposal becomes decided
// and the task cancelled.
func ApplyRouteAnswerTx(ctx context.Context, tx *sql.Tx, d OwnerDecision, action, label string) error {
	if d.SubjectID == nil {
		return fmt.Errorf("store: route decision %s has no proposal", d.ID)
	}
	p, err := readRouteProposal(ctx, tx, *d.SubjectID)
	if errors.Is(err, sql.ErrNoRows) {
		// Not a missing decision (404): the decision row is inconsistent.
		return fmt.Errorf("store: route decision %s has no proposal row", d.ID)
	}
	if err != nil {
		return err
	}
	if p.taskID != d.TaskID || !p.decisionID.Valid || p.decisionID.String != d.ID {
		return fmt.Errorf("store: proposal does not belong to decision %s", d.ID)
	}
	if p.status != string(model.ProposalProposed) && p.status != string(model.ProposalFailed) {
		return conflictf("Route proposal is not waiting for a decision")
	}
	var taskStatus model.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, d.TaskID).Scan(&taskStatus); err != nil {
		return err
	}
	if taskStatus != model.TaskRouting {
		return conflictf("Route decision requires the task to be routing")
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = 'decided' WHERE id = ? AND status = ?`, *d.SubjectID, p.status)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	switch action {
	case "approve":
		return approveRoute(ctx, tx, d, label)
	case "reject":
		if err := task.TransitionTaskTx(ctx, tx, task.TaskTransition{TaskID: d.TaskID, From: model.TaskRouting, To: model.TaskCancelled}); err != nil {
			return err
		}
		// The owner's cancellation of a task without an Execution (spec 16
		// 3.2 and 3.6.1): the task is the dedupe target.
		trigger := linearoutbox.Trigger{TaskID: d.TaskID, Kind: linearoutbox.KindCancelled, TargetID: d.TaskID}
		if err := linearoutbox.EnqueueTx(ctx, tx, trigger); err != nil {
			return err
		}
		return linearoutbox.EnqueueStateTx(ctx, tx, trigger)
	}
	return fmt.Errorf("store: no lifecycle change for route action %q", action)
}

func approveRoute(ctx context.Context, tx *sql.Tx, d OwnerDecision, label string) error {
	route, err := GetRoute(ctx, tx, label)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET project_id = ?, harness_id = ?, updated_at = ?
		WHERE id = ? AND status = 'routing'`, route.ProjectID, route.HarnessID, now.Format(time.RFC3339), d.TaskID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	executionID := ulid.Make().String()
	if err := InsertInitialExecution(ctx, tx, executionID, d.TaskID, now.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := task.QueueHeldTaskTx(ctx, tx, d.TaskID, executionID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, executionID, model.EventStatusChanged,
		map[string]any{"from": model.TaskRouting, "to": model.TaskQueued, "reason": "route_approved"}); err != nil {
		return err
	}
	return linearoutbox.EnqueueRouteLabelTx(ctx, tx, d.TaskID, d.ID, label)
}
