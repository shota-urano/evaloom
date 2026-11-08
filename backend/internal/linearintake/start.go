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
const DefaultRoutingLabelPrefix = "repo:"

// BlockCause is a value of tasks.intake_block_cause (spec 15 3.6 and 3.8).
type BlockCause string

const (
	CauseRoutingMultipleLabels     BlockCause = "routing_multiple_labels"
	CauseRoutingUnknownLabel       BlockCause = "routing_unknown_label"
	CauseRoutingTeamMismatch       BlockCause = "routing_team_mismatch"
	CauseRoutingNoLabel            BlockCause = "routing_no_label"
	CauseMissingAcceptanceCriteria BlockCause = "missing_acceptance_criteria"
	CauseConfigMissingAssignment   BlockCause = "config_missing_assignment"
)

// Routing is the outcome of steps 1-6 of spec 15 3.6 for one Issue.
type Routing struct {
	// ProjectID is empty when no Project was decided (3.6, Q45).
	ProjectID string
	// Criteria is the extracted acceptance criteria; empty only with
	// CauseMissingAcceptanceCriteria.
	Criteria string
	// Cause is empty when the task can run.
	Cause BlockCause
}

// TaskStarter implements Starter: routing by Linear Team and routing label
// (Linear Project is never used, requirements 6.1), acceptance criteria
// extraction and task creation (spec 15 3.6).
type TaskStarter struct {
	RoutingLabelPrefix string // empty: the Poller's round settings, then DefaultRoutingLabelPrefix
	Now                func() time.Time
}

var _ Starter = (*TaskStarter)(nil)

// Route decides where the Issue runs (spec 15 3.6 steps 1-6) without writing.
func (s *TaskStarter) Route(ctx context.Context, tx *sql.Tx, issue linear.Issue) (Routing, error) {
	criteria, written := ExtractAcceptanceCriteria(issue.Description)
	r := Routing{Criteria: criteria}
	prefix := s.RoutingLabelPrefix
	if prefix == "" {
		prefix = roundLabels(ctx).RoutingLabelPrefix
	}
	var labels []string
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, prefix) && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	switch len(labels) {
	case 0:
		r.Cause = CauseRoutingNoLabel
	case 1:
		routes, err := store.ListLinearRoutesByLabel(ctx, tx, labels[0])
		if err != nil {
			return Routing{}, err
		}
		r.Cause = CauseRoutingUnknownLabel
		for _, route := range routes {
			r.Cause = CauseRoutingTeamMismatch
			if route.TeamID == issue.TeamID {
				// UNIQUE(linear_team_id, linear_routing_label): at most one.
				r.ProjectID, r.Cause = route.ProjectID, ""
				break
			}
		}
	default:
		r.Cause = CauseRoutingMultipleLabels
	}
	if !written {
		// tasks.acceptance_criteria may be empty only with this cause (3.8),
		// so it also replaces a routing cause when both apply.
		r.Cause = CauseMissingAcceptanceCriteria
		return r, nil
	}
	if r.Cause != "" {
		return r, nil
	}
	project, err := store.GetProject(ctx, tx, r.ProjectID)
	if err != nil {
		return Routing{}, fmt.Errorf("linearintake: read project: %w", err)
	}
	if _, err := store.GetProjectHarness(ctx, tx, project.ID, project.DefaultRole); errors.Is(err, sql.ErrNoRows) {
		r.Cause = CauseConfigMissingAssignment
	} else if err != nil {
		return Routing{}, fmt.Errorf("linearintake: read assignment: %w", err)
	}
	return r, nil
}

// StartLinearTask creates the task for one attach inside tx (spec 15 3.6
// steps 7-9). A held task is blocked and has no Execution; otherwise the task
// and its first Execution are created as in manual creation (spec 04 3.1)
// and the task is queued.
func (s *TaskStarter) StartLinearTask(ctx context.Context, tx *sql.Tx, issue linear.Issue, attachSeq int) (Decision, error) {
	r, err := s.Route(ctx, tx, issue)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339)
	created := model.Task{
		ProjectID: r.ProjectID, Title: issue.Title, Body: issue.Description, AcceptanceCriteria: r.Criteria,
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
	decision := DecisionStarted
	if r.Cause != "" {
		decision = DecisionHeld
		created.Status = model.TaskBlocked
		detail["intakeBlockCause"] = string(r.Cause)
		detail["to"] = model.TaskBlocked
		if err := store.InsertLinearTask(ctx, tx, &created, string(r.Cause)); err != nil {
			return "", fmt.Errorf("linearintake: insert held task: %w", err)
		}
	} else {
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
		if err := enqueueHold(ctx, tx, created.ID, r.Cause); err != nil {
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
// commented once per task (3.5 rule 3).
func enqueueHold(ctx context.Context, tx *sql.Tx, taskID string, cause BlockCause) error {
	kind := linearoutbox.KindIntakeBlocked
	if cause == CauseMissingAcceptanceCriteria {
		kind = linearoutbox.KindMissingAcceptanceCriteria
	}
	return linearoutbox.EnqueueTx(ctx, tx, linearoutbox.Trigger{TaskID: taskID, Kind: kind, TargetID: string(cause)})
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
