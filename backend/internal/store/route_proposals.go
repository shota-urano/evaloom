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
	var taskStatus model.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, p.taskID).Scan(&taskStatus); err != nil {
		return err
	}
	if taskStatus == model.TaskCancelled {
		// Spec 04 3.7/3.8 (Q133): the task was cancelled while the Runner
		// ran the proposal; the Runner stopped it, so it ends cancelled and
		// its result is only recorded.
		status = model.ProposalCancelled
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = ?, proposed_label = ?, reason = ?, error_class = ?, finished_at = ?
		WHERE id = ? AND status = 'running'`, status, label, reason, errorClass, now, in.ProposalID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	if taskStatus != model.TaskRouting {
		return nil
	}
	return OpenRouteDecisionTx(ctx, tx, p.taskID, in.ProposalID)
}

// OpenRouteDecisionTx opens the kind=route decision of a proposed or failed
// proposal (spec 04 3.7, 02 3.4): subject_id and basis are the proposal ID,
// and the proposal records the decision. A Linear task gets the
// "管理画面で判断待ち" comment (spec 15 3.6.4 step 5, 16 3.2) without an
// Execution (16 3.4.2).
func OpenRouteDecisionTx(ctx context.Context, tx *sql.Tx, taskID, proposalID string) error {
	id := ulid.Make().String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_decisions (id, kind, task_id, subject_id, basis, detail, status, created_at)
		VALUES (?, 'route', ?, ?, ?, '{}', 'open', ?)`, id, taskID, proposalID, proposalID,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET decision_id = ? WHERE id = ? AND task_id = ?`, id, proposalID, taskID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	return linearoutbox.EnqueueTx(ctx, tx, linearoutbox.Trigger{TaskID: taskID, Kind: linearoutbox.KindDecisionOpen, TargetID: id})
}

// DefaultRouteProposer is the default of the global setting routing.proposer
// (spec 02 3.8).
const DefaultRouteProposer = `{"microHarness":"claude_code","model":null}`

// RouteProposerJSON is the routing.proposer setting, or its default.
func RouteProposerJSON(settings map[string]string) json.RawMessage {
	if raw, ok := settings["routing.proposer"]; ok && json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	return json.RawMessage(DefaultRouteProposer)
}

// RouteProposerModelIsNull reports whether routing.proposer has no model, in
// which case no proposal is made (spec 15 3.6.4 step 2, 02 3.4).
func RouteProposerModelIsNull(settings map[string]string) bool {
	var proposer struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(RouteProposerJSON(settings), &proposer); err != nil {
		return true
	}
	return proposer.Model == nil
}

// FailRouteProposalWithoutRunnerTx makes a queued proposal failed without
// handing it to a Runner, because routing.proposer has no model or candidates
// is empty (spec 15 3.6.4 step 2, Q222): error_class stays NULL. While the task
// is routing, a route decision without a proposal is opened (step 5).
func FailRouteProposalWithoutRunnerTx(ctx context.Context, tx *sql.Tx, proposalID string) error {
	var taskID string
	var taskStatus model.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT rp.task_id, t.status FROM route_proposals rp JOIN tasks t ON t.id = rp.task_id
		WHERE rp.id = ?`, proposalID).Scan(&taskID, &taskStatus); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = 'failed', error_class = NULL, finished_at = ?
		WHERE id = ? AND status = 'queued'`, time.Now().UTC().Format(time.RFC3339Nano), proposalID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	if taskStatus != model.TaskRouting {
		return nil
	}
	return OpenRouteDecisionTx(ctx, tx, taskID, proposalID)
}

// ApplyRouteAnswerTx performs the route rows of spec 04 3.6 and 3.7. The
// caller has checked the decision is open, the answer's shape and basis, and
// that the approved label is in routes; it writes the answer and its audit.
//
// approve: the task gets the label's destination and is checked against its
// Profile before creating a seq=1 pending Execution; the proposal is decided
// and the label addition is written to linear_outbox. reject: it is decided
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
	// Criteria were extracted when the Linear task was received, even while
	// routing. Only now may the destination's declaration be applied (19 3.9).
	var projectID, harnessID, criteria string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(project_id, ''), coalesce(harness_id, ''), acceptance_criteria
		FROM tasks WHERE id = ?`, d.TaskID).Scan(&projectID, &harnessID, &criteria); err != nil {
		return err
	}
	cause, err := LinearIntakeBlockCause(ctx, tx, projectID, harnessID, criteria)
	if err != nil {
		return err
	}
	if err := linearoutbox.EnqueueRouteLabelTx(ctx, tx, d.TaskID, d.ID, label); err != nil {
		return err
	}
	if cause != "" {
		if err := task.TransitionTaskTx(ctx, tx, task.TaskTransition{TaskID: d.TaskID,
			From: model.TaskRouting, To: model.TaskBlocked, IntakeBlockCause: cause}); err != nil {
			return err
		}
		if cause == model.IntakeMissingAcceptanceCriteria {
			return linearoutbox.EnqueueTx(ctx, tx, linearoutbox.Trigger{TaskID: d.TaskID,
				Kind: linearoutbox.KindMissingAcceptanceCriteria, TargetID: string(cause)})
		}
		return nil
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
	return nil
}

// RouteCandidate is one entry of route_proposals.candidates (spec 02 3.4, 04
// 3.3): a routes label, its destination kind and ID, and the description of
// the destination's Profile (a Project's pinned Profile or a special
// harness's default Profile). Description is nil when the destination has no
// Profile to read it from.
type RouteCandidate struct {
	Label       string  `json:"label"`
	Kind        string  `json:"kind"`
	ID          string  `json:"id"`
	Description *string `json:"description"`
}

// RouteCandidatesTx reads the routes rows as candidates at the time of the
// routing decision (spec 15 3.6.4 step 1), ordered by label. A row routing to
// a special harness without a default Profile (harnesses.default_profile_id)
// is left out (spec 15 3.6.4 step 1, Q218); Project rows are always kept.
func RouteCandidatesTx(ctx context.Context, tx *sql.Tx) ([]RouteCandidate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.label,
		CASE WHEN r.project_id IS NOT NULL THEN 'project' ELSE 'harness' END,
		coalesce(r.project_id, r.harness_id),
		json_extract(hp.definition, '$.description')
		FROM routes r
		LEFT JOIN projects p ON p.id = r.project_id
		LEFT JOIN harnesses h ON h.id = r.harness_id
		LEFT JOIN harness_profiles hp ON hp.id = coalesce(p.profile_id, h.default_profile_id)
		WHERE r.project_id IS NOT NULL OR h.default_profile_id IS NOT NULL
		ORDER BY r.label`)
	if err != nil {
		return nil, fmt.Errorf("store: list route candidates: %w", err)
	}
	defer rows.Close()
	out := []RouteCandidate{}
	for rows.Next() {
		var c RouteCandidate
		var description sql.NullString
		if err := rows.Scan(&c.Label, &c.Kind, &c.ID, &description); err != nil {
			return nil, err
		}
		if description.Valid {
			c.Description = &description.String
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NewRouteProposal is a queued route_proposals row (spec 15 3.6.4 step 1).
type NewRouteProposal struct {
	ID, TaskID, Trigger string
	SeenLabels          []string
	Candidates          []RouteCandidate
	CreatedAt           string
}

// InsertRouteProposalTx writes a queued proposal for a routing task. The
// caller owns tx, which also creates the task.
func InsertRouteProposalTx(ctx context.Context, tx *sql.Tx, p NewRouteProposal) error {
	seen := p.SeenLabels
	if seen == nil {
		seen = []string{}
	}
	candidates := p.Candidates
	if candidates == nil {
		candidates = []RouteCandidate{}
	}
	seenJSON, err := json.Marshal(seen)
	if err != nil {
		return err
	}
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'queued', ?)`, p.ID, p.TaskID, p.Trigger, string(seenJSON), string(candidatesJSON), p.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: insert route proposal: %w", err)
	}
	return nil
}

// SupersedeRouteProposalsTx applies spec 15 3.6.3 (本人決定 3) to a routing
// task whose Linear labels now decide its destination: every open proposal
// (queued, running, proposed or failed) becomes superseded and every open
// route decision of the task is withdrawn with the reason superseded. A
// result arriving later for a superseded proposal is only recorded (3.6.4).
// It returns the IDs of the superseded proposals.
func SupersedeRouteProposalsTx(ctx context.Context, tx *sql.Tx, taskID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM route_proposals
		WHERE task_id = ? AND status IN ('queued', 'running', 'proposed', 'failed') ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list open route proposals: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = 'superseded', finished_at = coalesce(finished_at, ?)
		WHERE task_id = ? AND status IN ('queued', 'running', 'proposed', 'failed')`, now, taskID); err != nil {
		return nil, fmt.Errorf("store: supersede route proposals: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE owner_decisions SET status = 'withdrawn', withdrawn_reason = 'superseded'
		WHERE task_id = ? AND kind = 'route' AND status = 'open'`, taskID); err != nil {
		return nil, fmt.Errorf("store: withdraw route decisions: %w", err)
	}
	return ids, nil
}
