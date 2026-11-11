package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// workspaceMissingError makes a resumed candidate blocked (workspace_missing)
// instead of claimed (spec 04 3.3 step 5).
type workspaceMissingError struct {
	workspaceID, path string
}

func (e *workspaceMissingError) Error() string {
	return fmt.Sprintf("workspace %s was cleaned", e.workspaceID)
}

// pinnedExecution is what the first claim fixed on the Execution (spec 04 3.3
// step 4). A resumed claim reads these values and never rewrites them.
type pinnedExecution struct {
	routeKind, projectID, harnessID, profileID    sql.NullString
	params, workspaceID, workSessionID, contentID sql.NullString
	workspacePath, workspaceStatus, projectName   sql.NullString
}

// claimResumedExecution implements spec 04 3.3 step 5 for an Execution with
// resume_stage: the version, params and workspace pinned by its first claim
// are used as they are; a cleaned workspace returns workspaceMissingError.
func claimResumedExecution(ctx context.Context, tx *sql.Tx, input ClaimInput, c *pendingCandidate, settings map[string]string) (*ExecutionJob, error) {
	stage := model.Stage(c.resumeStage.String)
	job := &ExecutionJob{Kind: "execution", Execution: ClaimExecution{
		ID: c.executionID, TaskID: c.taskID, Seq: c.seq, Mode: c.mode, ResumeStage: &stage,
	}}
	var p pinnedExecution
	err := tx.QueryRowContext(ctx, `SELECT e.route_kind, e.project_id, e.harness_id, e.profile_id, e.params,
		e.workspace_id, e.work_session_id, e.content_id, w.path, w.status, pr.name
		FROM executions e LEFT JOIN task_workspaces w ON w.id = e.workspace_id
		LEFT JOIN projects pr ON pr.id = e.project_id
		WHERE e.id = ?`, c.executionID).
		Scan(&p.routeKind, &p.projectID, &p.harnessID, &p.profileID, &p.params,
			&p.workspaceID, &p.workSessionID, &p.contentID, &p.workspacePath, &p.workspaceStatus, &p.projectName)
	if err != nil {
		return nil, err
	}
	if !p.routeKind.Valid || !p.harnessID.Valid || !p.profileID.Valid || !p.params.Valid || !p.workspaceID.Valid || !p.workspacePath.Valid {
		return nil, configError("resumed Execution has no pinned version, params or workspace")
	}
	if p.workspaceStatus.String == "cleaned" {
		return nil, &workspaceMissingError{workspaceID: p.workspaceID.String, path: p.workspacePath.String}
	}
	switch model.RouteKind(p.routeKind.String) {
	case model.RouteProject:
		if !p.projectID.Valid || !p.projectName.Valid {
			return nil, configError("resumed Execution's Project is missing")
		}
		id, name := p.projectID.String, p.projectName.String
		job.Route = contract.RequestRoute{Kind: model.RouteProject, ProjectID: &id, ProjectName: &name}
	default:
		job.Route = contract.RequestRoute{Kind: model.RouteHarness}
	}
	job.Params = json.RawMessage(p.params.String)
	harness, err := GetHarness(ctx, tx, p.harnessID.String)
	if err != nil {
		return nil, err
	}
	profile, err := GetHarnessProfileByID(ctx, tx, p.profileID.String)
	if err != nil {
		return nil, err
	}
	job.Profile = ClaimProfile{ID: profile.ID, HarnessID: harness.ID, Version: profile.Version,
		CommitSHA: profile.CommitSHA, TagName: profile.TagName, RepoPath: harness.RepoPath, Definition: profile.Definition}
	if err := readClaimTask(ctx, tx, c.taskID, &job.Task); err != nil {
		return nil, err
	}
	var definition claimDefinition
	if err := json.Unmarshal(profile.Definition, &definition); err != nil {
		return nil, configError("Profile definition cannot be read")
	}
	// The pinned Profile's declared limits; the claim-time checks of step 4
	// (Q135 and the others) apply only when the version is pinned.
	if job.Limits.TimeoutSec, err = claimTimeouts(definition, settings, false); err != nil {
		return nil, err
	}
	job.Workspace = ClaimWorkspace{ID: p.workspaceID.String, Path: p.workspacePath.String}
	if job.Previous, err = previousExecution(ctx, tx, c.taskID, c.seq); err != nil {
		return nil, err
	}
	if job.Resume, err = claimResume(ctx, tx, c.executionID, stage, p); err != nil {
		return nil, err
	}
	return job, markClaimed(ctx, tx, input, c, job)
}

var stageOrder = []model.Stage{model.StagePrepare, model.StageWork, model.StageCheck, model.StagePublish}

// claimResume builds the resume object of spec 04 3.3.1.
func claimResume(ctx context.Context, tx *sql.Tx, executionID string, stage model.Stage, p pinnedExecution) (*ClaimResume, error) {
	resume := &ClaimResume{Stage: stage, CompletedStages: []model.Stage{}}
	// A stage before the resume stage counts as completed when one of its
	// runs ended in a state that let the Execution go on (check also goes on
	// through needs_approval and an owner-continued blocked).
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT stage FROM stage_runs
		WHERE execution_id = ? AND status IN ('succeeded', 'needs_approval', 'blocked')`, executionID)
	if err != nil {
		return nil, err
	}
	done := map[model.Stage]bool{}
	for rows.Next() {
		var s model.Stage
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		done[s] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, s := range stageOrder {
		if s == stage {
			break
		}
		if done[s] {
			resume.CompletedStages = append(resume.CompletedStages, s)
		}
	}
	switch stage {
	case model.StagePublish:
		if p.contentID.Valid {
			resume.CheckedContentID = &p.contentID.String
		}
		approvals, err := resumeApprovals(ctx, tx, executionID)
		if err != nil {
			return nil, err
		}
		resume.Approvals = approvals
	case model.StageWork:
		if p.workSessionID.Valid {
			resume.WorkSessionID = &p.workSessionID.String
		}
		answer, err := resumeAnswer(ctx, tx, executionID)
		if err != nil {
			return nil, err
		}
		resume.Answer = answer
	}
	return resume, nil
}

// resumeApprovals lists the Execution's approved approvals and the operations
// whose approval was waived (approval_waived events, spec 02 5.3, 17 3.3).
func resumeApprovals(ctx context.Context, tx *sql.Tx, executionID string) ([]contract.RequestApproval, error) {
	out := []contract.RequestApproval{}
	rows, err := tx.QueryContext(ctx, `SELECT id, operation, content_id FROM approvals
		WHERE execution_id = ? AND status = 'approved' ORDER BY created_at, id`, executionID)
	if err != nil {
		return nil, err
	}
	approved := map[string]bool{}
	for rows.Next() {
		var id, operation, content string
		if err := rows.Scan(&id, &operation, &content); err != nil {
			rows.Close()
			return nil, err
		}
		approved[operation] = true
		out = append(out, contract.RequestApproval{ID: &id, Operation: operation, ContentID: &content, Status: "approved"})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT payload FROM execution_events
		WHERE execution_id = ? AND type = 'approval_waived' ORDER BY id`, executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	waived := map[string]int{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var payload struct {
			Operation string  `json:"operation"`
			ContentID *string `json:"contentId"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.Operation == "" {
			return nil, fmt.Errorf("store: Execution %s has an invalid approval_waived event", executionID)
		}
		if approved[payload.Operation] {
			continue
		}
		entry := contract.RequestApproval{Operation: payload.Operation, ContentID: payload.ContentID, Status: "waived"}
		// The latest waiver of an operation wins.
		if i, ok := waived[payload.Operation]; ok {
			out[i] = entry
			continue
		}
		waived[payload.Operation] = len(out)
		out = append(out, entry)
	}
	return out, rows.Err()
}

// resumeAnswer is the owner's answer to the Execution's latest question
// (spec 02 5.6, contract 8).
func resumeAnswer(ctx context.Context, tx *sql.Tx, executionID string) (*contract.Answer, error) {
	var id string
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, answer FROM owner_decisions
		WHERE execution_id = ? AND kind = 'question' AND status = 'answered'
		ORDER BY answered_at DESC, id DESC LIMIT 1`, executionID).Scan(&id, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var answer struct {
		Text   string  `json:"text"`
		Option *string `json:"option"`
	}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &answer); err != nil {
			return nil, fmt.Errorf("store: decision %s has an invalid answer", id)
		}
	}
	return &contract.Answer{DecisionID: id, Text: answer.Text, Option: answer.Option}, nil
}

// blockWorkspaceMissing makes a resumed candidate whose workspace was cleaned
// blocked (cause=workspace_missing) with an open owner decision (spec 04 3.3
// step 5, 3.5; spec 02 5.6). The transition records the notification request.
func blockWorkspaceMissing(ctx context.Context, tx *sql.Tx, c *pendingCandidate, missing *workspaceMissingError) error {
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: c.taskID, ExecutionID: c.executionID,
		FromTask: model.TaskQueued, FromExecution: model.ExecutionPending, ToExecution: model.ExecutionBlocked,
	}); err != nil {
		return err
	}
	if _, err := AppendExecutionEvent(ctx, tx, c.executionID, model.EventStatusChanged,
		json.RawMessage(`{"from":"pending","to":"blocked","reason":"workspace_missing"}`)); err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]any{
		"message":     "再開に要る作業領域が削除済みです。中止して fresh で再実行してください。",
		"workspaceId": missing.workspaceID, "resumeStage": c.resumeStage.String,
	})
	if err != nil {
		return err
	}
	decisionID := ulid.Make().String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_decisions (id, kind, task_id, execution_id, cause, detail, status, created_at)
		VALUES (?, 'blocked', ?, ?, ?, ?, 'open', ?)`, decisionID, c.taskID, c.executionID,
		model.CauseWorkspaceMissing, string(detail), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"decisionId": decisionID, "kind": "blocked", "cause": model.CauseWorkspaceMissing})
	if err != nil {
		return err
	}
	_, err = AppendExecutionEvent(ctx, tx, c.executionID, model.EventDecisionOpened, payload)
	return err
}
