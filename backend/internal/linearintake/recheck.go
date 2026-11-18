package linearintake

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

var _ HeldRechecker = (*TaskStarter)(nil)

// RecheckHeldTask redoes the 3.6 decision for the Issue's held task (spec 15
// 3.6.3) with the same Route as task creation, so creation and recheck never
// disagree. When it passes, the same task gets its Project and acceptance
// criteria, its first Execution and the queued status; no task is created.
// When the cause is unchanged nothing is written (no second comment for the
// same cause, spec 16 3.2). A different cause is written to the task and
// stays held. A held task whose labels no longer decide a destination goes
// back to routing with a new routing proposal (Q219). An Issue without a held
// task is left alone.
func (s *TaskStarter) RecheckHeldTask(ctx context.Context, tx *sql.Tx, issue linear.Issue) (Decision, error) {
	held, err := store.ListHeldLinearTasks(ctx, tx, issue.ID)
	if err != nil || len(held) == 0 {
		return "", err
	}
	// At most one task of an Issue is non-terminal (spec 15 3.5 rule 3).
	h := held[len(held)-1]
	r, err := s.Route(ctx, tx, issue)
	if err != nil {
		return "", err
	}
	// ListHeldLinearTasks reports a routing task as "routing".
	comparisonCause := string(r.Cause)
	if r.Trigger != "" {
		comparisonCause = "routing"
	}
	if comparisonCause == h.Cause {
		return "", nil
	}
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339)
	detail := map[string]any{"source": "linear", "linearIssueId": issue.ID,
		"issueIdentifier": issue.Identifier, "fromIntakeBlockCause": h.Cause}
	if r.ProjectID != "" {
		detail["projectId"] = r.ProjectID
	}
	if r.HarnessID != "" {
		detail["harnessId"] = r.HarnessID
	}
	from := model.TaskBlocked
	if h.Cause == "routing" {
		from = model.TaskRouting
		if r.Trigger == "" {
			// The owner labelled the Issue in Linear: ordinary routing, not
			// an approval (本人決定 3). Open proposals are superseded and
			// the route decision is withdrawn (superseded).
			superseded, err := store.SupersedeRouteProposalsTx(ctx, tx, h.TaskID)
			if err != nil {
				return "", err
			}
			if len(superseded) > 0 {
				detail["supersededRouteProposalIds"] = superseded
			}
		}
	}
	if err := store.UpdateHeldLinearTask(ctx, tx, h.TaskID, r.ProjectID, r.HarnessID, r.Criteria, comparisonCause, stamp); err != nil {
		return "", err
	}
	decision := DecisionHeld
	if r.Trigger != "" {
		decision = DecisionRouting
		detail["routeTrigger"] = string(r.Trigger)
		if from == model.TaskBlocked {
			// The held task's routing labels no longer decide a destination:
			// it is back to routing and the routing proposal is made again
			// as in 3.6.4 (2026-11-18 本人決定, Q219), with the candidates of
			// now. A routing task never gets here (same cause, Q159).
			proposalID, err := s.remakeRouteProposal(ctx, tx, h.TaskID, r, now, stamp)
			if err != nil {
				return "", err
			}
			detail["routeProposalId"], detail["from"], detail["to"] = proposalID, from, model.TaskRouting
		}
	} else if r.Cause != "" {
		detail["intakeBlockCause"] = string(r.Cause)
		if err := enqueueHold(ctx, tx, h.TaskID, r.Cause, s.routingLabelPrefix(ctx)); err != nil {
			return "", err
		}
	} else {
		decision = DecisionStarted
		executionID, err := newID(now)
		if err != nil {
			return "", err
		}
		detail["executionId"], detail["from"], detail["to"] = executionID, from, model.TaskQueued
		if err := store.InsertInitialExecution(ctx, tx, executionID, h.TaskID, stamp); err != nil {
			return "", fmt.Errorf("linearintake: insert execution: %w", err)
		}
		if err := task.QueueHeldTaskTx(ctx, tx, h.TaskID, executionID); err != nil {
			return "", err
		}
		payload, err := json.Marshal(struct {
			From   model.TaskStatus `json:"from"`
			To     model.TaskStatus `json:"to"`
			Reason string           `json:"reason"`
		}{from, model.TaskQueued, "intake_recheck"})
		if err != nil {
			return "", err
		}
		if _, err := store.AppendExecutionEvent(ctx, tx, executionID, model.EventStatusChanged, payload); err != nil {
			return "", err
		}
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{Actor: "system", Action: "task.intake_recheck",
		TargetType: "task", TargetID: h.TaskID, Detail: detail}); err != nil {
		return "", err
	}
	return decision, nil
}

// remakeRouteProposal writes a new queued routing proposal (spec 15 3.6.4
// step 1) for a held task that went back to routing (3.6.3, Q219). Any
// proposal still open is superseded first and its route decision withdrawn
// (superseded), so the task keeps at most one open route decision; a running
// one is not stopped at the Runner and its later result is only recorded.
// Without a routing.proposer model the new proposal fails at once and the
// route decision opens (steps 2 and 5); otherwise a Runner claims it.
func (s *TaskStarter) remakeRouteProposal(ctx context.Context, tx *sql.Tx, taskID string, r Routing, now time.Time, stamp string) (string, error) {
	if _, err := store.SupersedeRouteProposalsTx(ctx, tx, taskID); err != nil {
		return "", err
	}
	proposalID, err := newID(now)
	if err != nil {
		return "", err
	}
	if err := insertRouteProposal(ctx, tx, taskID, proposalID, r, stamp); err != nil {
		return "", err
	}
	return proposalID, nil
}
