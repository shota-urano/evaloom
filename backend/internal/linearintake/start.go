package linearintake

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// DefaultRoutingLabelPrefix is the default of the global setting
// linear.routingLabelPrefix (spec 15 3.4, Q48).
const DefaultRoutingLabelPrefix = "route:"

// BlockCause is a value of tasks.intake_block_cause (spec 15 3.6.2, 02 5.7).
type BlockCause string

const (
	CauseMissingAcceptanceCriteria BlockCause = "missing_acceptance_criteria"
	CauseConfigMissingProfile      BlockCause = "config_missing_profile"
)

// RouteTrigger is a value of route_proposals.trigger: why the labels did not
// decide a destination (spec 15 3.6 step 3).
type RouteTrigger string

const (
	TriggerNoLabel        RouteTrigger = "no_label"
	TriggerUnknownLabel   RouteTrigger = "unknown_label"
	TriggerMultipleLabels RouteTrigger = "multiple_labels"
)

// Routing is the outcome of steps 2-5 of spec 15 3.6 for one Issue.
type Routing struct {
	// Trigger is set when the routing labels decide no destination; the task
	// then goes to a routing proposal (3.6.4) instead of being held.
	Trigger RouteTrigger
	// SeenLabels are the routing labels found on the Issue.
	SeenLabels []string
	// ProjectID is set for a Project destination.
	ProjectID string
	// HarnessID is set only for direct special-harness routing.
	HarnessID string
	// Criteria may be empty while routing, when optional, or when held.
	Criteria string
	// Cause is empty when the task can run. It is never set while routing:
	// the destination's Profile is not read before routing is decided (Q157).
	Cause BlockCause
}

// TaskStarter implements Starter: routing by the globally unique label,
// acceptance criteria extraction and task creation (spec 15 3.6). The
// Linear Team is not part of the decision.
type TaskStarter struct {
	RoutingLabelPrefix string // empty: the Poller's round settings, then DefaultRoutingLabelPrefix
	TriggerLabel       string // empty: the Poller's round settings, then DefaultTriggerLabel
	Now                func() time.Time
}

var _ Starter = (*TaskStarter)(nil)

// RoutingLabels collects the Issue's routing labels (spec 15 3.6 step 2):
// labels starting with prefix, without duplicates, never the trigger label.
func RoutingLabels(labels []string, prefix, trigger string) []string {
	var out []string
	for _, label := range labels {
		if label != trigger && strings.HasPrefix(label, prefix) && !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

// Route decides where the Issue runs (spec 15 3.6 steps 2-5) without writing.
func (s *TaskStarter) Route(ctx context.Context, tx *sql.Tx, issue linear.Issue) (Routing, error) {
	criteria, _ := ExtractAcceptanceCriteria(issue.Description)
	labels := RoutingLabels(issue.Labels, s.routingLabelPrefix(ctx), s.triggerLabel(ctx))
	r := Routing{Criteria: criteria, SeenLabels: labels}
	switch len(labels) {
	case 0:
		r.Trigger = TriggerNoLabel
	case 1:
		route, err := store.GetRoute(ctx, tx, labels[0])
		if errors.Is(err, sql.ErrNoRows) {
			r.Trigger = TriggerUnknownLabel
		} else if err != nil {
			return Routing{}, err
		} else {
			if route.ProjectID != nil {
				r.ProjectID = *route.ProjectID
			}
			if route.HarnessID != nil {
				r.HarnessID = *route.HarnessID
			}
		}
	default:
		r.Trigger = TriggerMultipleLabels
	}
	if r.Trigger != "" {
		return r, nil
	}
	cause, err := store.LinearIntakeBlockCause(ctx, tx, r.ProjectID, r.HarnessID, criteria)
	if err != nil {
		return Routing{}, fmt.Errorf("linearintake: check destination: %w", err)
	}
	r.Cause = BlockCause(cause)
	return r, nil
}

// StartLinearTask creates the task for one attach inside tx (spec 15 3.6
// steps 1-7). An undecided routing makes a routing task with a queued route
// proposal (3.6.4 step 1); a held task is blocked with its cause. Neither has
// an Execution. Otherwise the task and its first Execution are created as in
// manual creation (spec 04 3.1) and the task is queued.
func (s *TaskStarter) StartLinearTask(ctx context.Context, tx *sql.Tx, issue linear.Issue, attachSeq int) (Decision, error) {
	r, err := s.Route(ctx, tx, issue)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339)
	created := model.Task{
		ProjectID: r.ProjectID, HarnessID: r.HarnessID, Title: issue.Title, Body: issue.Description, AcceptanceCriteria: r.Criteria,
		ShortName: strings.ToLower(issue.Identifier), TaskType: "development", CollaborationMode: "solo",
		Status:         model.TaskReceived,
		IdempotencyKey: fmt.Sprintf("linear:%s:%d", issue.ID, attachSeq),
		CreatedAt:      stamp, UpdatedAt: stamp,
	}
	detail := map[string]any{"source": "linear", "linearIssueId": issue.ID,
		"issueIdentifier": issue.Identifier, "attachSeq": attachSeq}
	if r.ProjectID != "" {
		detail["projectId"] = r.ProjectID
	}
	if r.HarnessID != "" {
		detail["harnessId"] = r.HarnessID
	}
	decision := DecisionStarted
	switch {
	case r.Trigger != "":
		decision = DecisionRouting
		created.Status = model.TaskRouting
		detail["routeTrigger"] = string(r.Trigger)
		detail["to"] = model.TaskRouting
		if err := store.InsertLinearTask(ctx, tx, &created, ""); err != nil {
			return "", fmt.Errorf("linearintake: insert routing task: %w", err)
		}
		candidates, err := store.RouteCandidatesTx(ctx, tx)
		if err != nil {
			return "", err
		}
		proposalID, err := newID(now)
		if err != nil {
			return "", err
		}
		detail["routeProposalId"] = proposalID
		if err := store.InsertRouteProposalTx(ctx, tx, store.NewRouteProposal{ID: proposalID, TaskID: created.ID,
			Trigger: string(r.Trigger), SeenLabels: r.SeenLabels, Candidates: candidates, CreatedAt: stamp}); err != nil {
			return "", err
		}
	case r.Cause != "":
		decision = DecisionHeld
		created.Status = model.TaskBlocked
		detail["intakeBlockCause"] = string(r.Cause)
		detail["to"] = model.TaskBlocked
		if err := store.InsertLinearTask(ctx, tx, &created, string(r.Cause)); err != nil {
			return "", fmt.Errorf("linearintake: insert held task: %w", err)
		}
	default:
		executionID, err := newID(now)
		if err != nil {
			return "", err
		}
		detail["executionId"], detail["from"], detail["to"] = executionID, model.TaskReceived, model.TaskQueued
		if err := store.InsertLinearTask(ctx, tx, &created, ""); err != nil {
			return "", fmt.Errorf("linearintake: insert task: %w", err)
		}
		if err := store.InsertInitialExecution(ctx, tx, executionID, created.ID, stamp); err != nil {
			return "", fmt.Errorf("linearintake: insert execution: %w", err)
		}
		if err := task.QueueExecutionTx(ctx, tx, created.ID, executionID, model.TaskReceived); err != nil {
			return "", err
		}
		if _, err := store.AppendExecutionEvent(ctx, tx, executionID, model.EventStatusChanged,
			json.RawMessage(`{"from":"received","to":"queued","reason":"task_created"}`)); err != nil {
			return "", err
		}
	}
	linkID, err := newID(now)
	if err != nil {
		return "", err
	}
	if err := store.InsertLinearIssueLink(ctx, tx, store.LinearIssueLink{ID: linkID, LinearIssueID: issue.ID,
		IssueIdentifier: issue.Identifier, IssueURL: issue.URL, TeamID: issue.TeamID, TaskID: created.ID,
		AttachSeq: attachSeq, CreatedAt: stamp}); err != nil {
		return "", err
	}
	// After the link: the outbox row needs the linked Issue.
	if r.Cause != "" {
		if err := enqueueHold(ctx, tx, created.ID, r.Cause, s.routingLabelPrefix(ctx)); err != nil {
			return "", err
		}
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{Actor: "system", Action: "task.create",
		TargetType: "task", TargetID: created.ID, Detail: detail}); err != nil {
		return "", err
	}
	return decision, nil
}

// enqueueHold writes the hold comment of spec 16 3.2 for a held task in the
// hold's transaction. The cause is the dedupe target, so the same cause is
// commented once per task (3.5 rule 3). The comment carries the reason and
// the fix, with the routing label prefix the decision used.
func enqueueHold(ctx context.Context, tx *sql.Tx, taskID string, cause BlockCause, prefix string) error {
	if cause == CauseConfigMissingProfile {
		return nil
	}
	kind := linearoutbox.KindIntakeBlocked
	if cause == CauseMissingAcceptanceCriteria {
		kind = linearoutbox.KindMissingAcceptanceCriteria
	}
	return linearoutbox.EnqueueTx(ctx, tx, linearoutbox.Trigger{TaskID: taskID, Kind: kind, TargetID: string(cause),
		RoutingLabelPrefix: prefix})
}

// routingLabelPrefix is the prefix Route uses: the starter's own, else the
// Poller's round settings, else the default.
func (s *TaskStarter) routingLabelPrefix(ctx context.Context) string {
	if s.RoutingLabelPrefix != "" {
		return s.RoutingLabelPrefix
	}
	return roundLabels(ctx).RoutingLabelPrefix
}

// triggerLabel is the label never counted as a routing label: the starter's
// own, else the Poller's round settings, else the default.
func (s *TaskStarter) triggerLabel(ctx context.Context) string {
	if s.TriggerLabel != "" {
		return s.TriggerLabel
	}
	return roundLabels(ctx).TriggerLabel
}

func newID(now time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (s *TaskStarter) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
