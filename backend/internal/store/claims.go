package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// Executions in these states occupy a slot (spec 04 3.3 step 1, 4). pending,
// waiting_approval, blocked and terminal Executions do not.
const activeExecutionStatuses = `('claimed', 'preparing', 'working', 'checking', 'publishing')`

// RouteProposalTimeoutSec is the fixed proposal limit (spec 15 Q201).
const RouteProposalTimeoutSec = 300

// ParamsValidator checks params against the Profile's params.schema read from
// the recorded commit (spec 04 3.3 step 4, Q115). A non-context error makes the
// Execution failed (config_error).
type ParamsValidator func(ctx context.Context, repoPath, commitSHA, schemaPath string, params json.RawMessage) error

// ClaimInput is one Runner's claim request (spec 04 3.3, Q134).
type ClaimInput struct {
	RunnerID      string
	WorkspaceRoot string
	// ValidateParams defaults to registry.ValidateProfileParams. It runs Git
	// inside the claim transaction; registry bounds it with a timeout.
	ValidateParams ParamsValidator
}

// ClaimResponse is the body of POST /api/runners/{id}/claim (spec 04 3.3.1).
// Job is nil, *ExecutionJob or *RouteProposalJob.
type ClaimResponse struct {
	Job any `json:"job"`
}

type ClaimExecution struct {
	ID          string              `json:"id"`
	TaskID      string              `json:"taskId"`
	Seq         int                 `json:"seq"`
	Mode        model.ExecutionMode `json:"mode"`
	ResumeStage *model.Stage        `json:"resumeStage"`
}

type ClaimProfile struct {
	ID         string          `json:"id"`
	HarnessID  string          `json:"harnessId"`
	Version    string          `json:"version"`
	CommitSHA  string          `json:"commitSha"`
	TagName    string          `json:"tagName"`
	RepoPath   string          `json:"repoPath"`
	Definition json.RawMessage `json:"definition"`
}

type ClaimWorkspace struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Created bool   `json:"created"`
}

type ClaimLimits struct {
	TimeoutSec map[model.Stage]int `json:"timeoutSec"`
}

// ClaimResume is filled only for an Execution with resume_stage (spec 04
// 3.3.1).
type ClaimResume struct {
	Stage            model.Stage                `json:"stage"`
	CheckedContentID *string                    `json:"checkedContentId"`
	Approvals        []contract.RequestApproval `json:"approvals"`
	Answer           *contract.Answer           `json:"answer"`
	WorkSessionID    *string                    `json:"workSessionId"`
	CompletedStages  []model.Stage              `json:"completedStages"`
}

// ExecutionJob carries only the materials of the contract 6.1 request JSON.
// Development items (worktree, branch, PR, test commands) are not part of it.
type ExecutionJob struct {
	Kind      string                      `json:"kind"`
	Execution ClaimExecution              `json:"execution"`
	Task      contract.RequestTask        `json:"task"`
	Route     contract.RequestRoute       `json:"route"`
	Params    json.RawMessage             `json:"params"`
	Profile   ClaimProfile                `json:"profile"`
	Workspace ClaimWorkspace              `json:"workspace"`
	Limits    ClaimLimits                 `json:"limits"`
	Previous  *contract.PreviousExecution `json:"previous"`
	Resume    *ClaimResume                `json:"resume"`
}

type RouteProposalJob struct {
	Kind     string        `json:"kind"`
	Proposal ClaimProposal `json:"proposal"`
}

type ClaimProposal struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"taskId"`
	Trigger    string          `json:"trigger"`
	Task       ProposalTask    `json:"task"`
	Candidates json.RawMessage `json:"candidates"`
	Proposer   json.RawMessage `json:"proposer"`
	TimeoutSec int             `json:"timeoutSec"`
}

type ProposalTask struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// ValidWorkspaceRoot reports whether root can prefix task_workspaces.path.
func ValidWorkspaceRoot(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Clean(root) == root && !strings.ContainsRune(root, 0)
}

// Claim runs spec 04 3.3 inside the caller's BEGIN IMMEDIATE transaction:
// capacity, proposals first (Q132), then the oldest eligible pending
// Execution. Candidates with configuration errors become failed (config_error)
// in the same transaction and the next candidate is examined, so the caller
// must commit even when the returned job is nil.
func Claim(ctx context.Context, tx *sql.Tx, input ClaimInput) (ClaimResponse, error) {
	if !ValidWorkspaceRoot(input.WorkspaceRoot) {
		return ClaimResponse{}, fmt.Errorf("store: workspaceRoot must be a clean absolute path")
	}
	if input.ValidateParams == nil {
		input.ValidateParams = registry.ValidateProfileParams
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM executions WHERE status IN `+activeExecutionStatuses+`) +
		(SELECT count(*) FROM route_proposals WHERE status = 'running')`).Scan(&active); err != nil {
		return ClaimResponse{}, err
	}
	if active >= 2 {
		return ClaimResponse{}, nil
	}
	settings, err := ReadSettings(ctx, tx)
	if err != nil {
		return ClaimResponse{}, err
	}
	defaultRunner := settingString(settings, "routing.defaultRunnerId")
	if defaultRunner != "" && defaultRunner == input.RunnerID {
		proposal, err := claimRouteProposal(ctx, tx, input.RunnerID, settings)
		if err != nil {
			return ClaimResponse{}, err
		}
		if proposal != nil {
			return ClaimResponse{Job: proposal}, nil
		}
	}
	for {
		candidate, err := nextPendingExecution(ctx, tx, input.RunnerID, defaultRunner)
		if err != nil || candidate == nil {
			return ClaimResponse{}, err
		}
		var job *ExecutionJob
		if candidate.resumeStage.Valid {
			job, err = claimResumedExecution(ctx, tx, input, candidate, settings)
			var missing *workspaceMissingError
			if errors.As(err, &missing) {
				if err := blockWorkspaceMissing(ctx, tx, candidate, missing); err != nil {
					return ClaimResponse{}, err
				}
				continue
			}
		} else {
			job, err = claimExecution(ctx, tx, input, candidate, settings)
		}
		var configErr *claimConfigError
		if errors.As(err, &configErr) {
			if err := failClaimConfig(ctx, tx, candidate, configErr.message); err != nil {
				return ClaimResponse{}, err
			}
			continue
		}
		if err != nil {
			return ClaimResponse{}, err
		}
		return ClaimResponse{Job: job}, nil
	}
}

func settingString(settings map[string]string, key string) string {
	var value string
	if raw, ok := settings[key]; ok && json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return ""
}

func claimRouteProposal(ctx context.Context, tx *sql.Tx, runnerID string, settings map[string]string) (*RouteProposalJob, error) {
	job := RouteProposalJob{Kind: "route_proposal"}
	p := &job.Proposal
	var candidates string
	err := tx.QueryRowContext(ctx, `SELECT rp.id, rp.task_id, rp.trigger, rp.candidates, t.title, t.body
		FROM route_proposals rp JOIN tasks t ON t.id = rp.task_id
		WHERE rp.status = 'queued' ORDER BY rp.created_at, rp.id LIMIT 1`).
		Scan(&p.ID, &p.TaskID, &p.Trigger, &candidates, &p.Task.Title, &p.Task.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE route_proposals SET status = 'running', runner_id = ?
		WHERE id = ? AND status = 'queued'`, runnerID, p.ID)
	if err := expectOneRow(result, err); err != nil {
		return nil, err
	}
	p.Candidates = json.RawMessage(candidates)
	// routing.proposer default (spec 02 3.8).
	p.Proposer = json.RawMessage(`{"microHarness":"claude_code","model":null}`)
	if raw, ok := settings["routing.proposer"]; ok && json.Valid([]byte(raw)) {
		p.Proposer = json.RawMessage(raw)
	}
	p.TimeoutSec = RouteProposalTimeoutSec
	return &job, nil
}

type pendingCandidate struct {
	executionID, taskID string
	seq                 int
	mode                model.ExecutionMode
	projectID           sql.NullString
	harnessID           sql.NullString
	resumeStage         sql.NullString
}

// nextPendingExecution picks the oldest pending Execution whose route belongs
// to runnerID and whose route (a Project, or a direct routing to a special
// harness) has no active Execution (spec 04 3.3 step 3). Project tasks never
// set tasks.harness_id, so Projects sharing a harness are separate routes.
// Resumed Executions (resume_stage set) compete in the same order and slots.
func nextPendingExecution(ctx context.Context, tx *sql.Tx, runnerID, defaultRunner string) (*pendingCandidate, error) {
	var c pendingCandidate
	err := tx.QueryRowContext(ctx, `SELECT e.id, e.task_id, e.seq, e.mode, t.project_id, t.harness_id, e.resume_stage
		FROM executions e JOIN tasks t ON t.id = e.task_id
		LEFT JOIN projects p ON p.id = t.project_id
		WHERE e.status = 'pending'
		AND ((t.project_id IS NOT NULL AND p.runner_id = ?)
			OR (t.harness_id IS NOT NULL AND ? <> '' AND ? = ?))
		AND NOT EXISTS (SELECT 1 FROM executions a JOIN tasks at ON at.id = a.task_id
			WHERE a.status IN `+activeExecutionStatuses+`
			AND ((t.project_id IS NOT NULL AND at.project_id = t.project_id)
				OR (t.harness_id IS NOT NULL AND at.harness_id = t.harness_id)))
		ORDER BY e.created_at, e.id LIMIT 1`, runnerID, defaultRunner, defaultRunner, runnerID).
		Scan(&c.executionID, &c.taskID, &c.seq, &c.mode, &c.projectID, &c.harnessID, &c.resumeStage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// claimDefinition is the part of a registered (already validated) Profile
// definition the claim checks again against today's task and settings.
type claimDefinition struct {
	AcceptanceCriteria string `json:"acceptanceCriteria"`
	Params             *struct {
		Schema string `json:"schema"`
	} `json:"params"`
	Stages struct {
		Prepare *claimStage `json:"prepare"`
		Work    claimStage  `json:"work"`
		Check   claimStage  `json:"check"`
		Publish claimStage  `json:"publish"`
	} `json:"stages"`
}

type claimStage struct {
	TimeoutSec int `json:"timeoutSec"`
}

type claimConfigError struct{ message string }

func (e *claimConfigError) Error() string { return e.message }

func configError(format string, args ...any) error {
	return &claimConfigError{message: fmt.Sprintf(format, args...)}
}

func claimExecution(ctx context.Context, tx *sql.Tx, input ClaimInput, c *pendingCandidate, settings map[string]string) (*ExecutionJob, error) {
	job := &ExecutionJob{Kind: "execution", Execution: ClaimExecution{
		ID: c.executionID, TaskID: c.taskID, Seq: c.seq, Mode: c.mode,
	}}
	var harness model.Harness
	var profileID string
	var routeProject any
	var err error
	if c.projectID.Valid {
		project, err := GetProject(ctx, tx, c.projectID.String)
		if err != nil {
			return nil, err
		}
		harness, err = GetHarness(ctx, tx, project.HarnessID)
		if err != nil {
			return nil, err
		}
		profileID = project.ProfileID
		job.Params = project.Params
		if len(job.Params) == 0 {
			job.Params = json.RawMessage(`{}`)
		}
		id, name := project.ID, project.Name
		job.Route = contract.RequestRoute{Kind: model.RouteProject, ProjectID: &id, ProjectName: &name}
		routeProject = id
	} else {
		harness, err = GetHarness(ctx, tx, c.harnessID.String)
		if err != nil {
			return nil, err
		}
		if harness.DefaultProfileID == nil || *harness.DefaultProfileID == "" {
			return nil, configError("special harness %s has no default Profile version", harness.ID)
		}
		profileID = *harness.DefaultProfileID
		job.Params = json.RawMessage(`{}`)
		job.Route = contract.RequestRoute{Kind: model.RouteHarness}
	}
	profile, err := GetHarnessProfileByID(ctx, tx, profileID)
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
	if definition.AcceptanceCriteria == "required" && strings.TrimSpace(job.Task.AcceptanceCriteria) == "" {
		return nil, configError("Profile requires acceptance criteria but the task has none")
	}
	if job.Limits.TimeoutSec, err = claimTimeouts(definition, settings, true); err != nil {
		return nil, err
	}
	if definition.Params != nil && definition.Params.Schema != "" {
		if err := input.ValidateParams(ctx, harness.RepoPath, profile.CommitSHA, definition.Params.Schema, job.Params); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, configError("params do not match the Profile params.schema")
		}
	}
	if job.Workspace, err = claimWorkspace(ctx, tx, input, c); err != nil {
		return nil, err
	}
	if job.Previous, err = previousExecution(ctx, tx, c.taskID, c.seq); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE executions SET route_kind = ?, project_id = ?, harness_id = ?,
		profile_id = ?, profile_version = ?, profile_commit_sha = ?, contract_version = ?, params = ?, workspace_id = ?
		WHERE id = ? AND status = 'pending' AND resume_stage IS NULL`,
		job.Route.Kind, routeProject, harness.ID, profile.ID, profile.Version, profile.CommitSHA,
		profile.ContractVersion, string(job.Params), job.Workspace.ID, c.executionID)
	if err := expectOneRow(result, err); err != nil {
		return nil, err
	}
	return job, markClaimed(ctx, tx, input, c, job)
}

// markClaimed moves the chosen Execution to claimed and records the Runner,
// the event and the audit log (spec 04 3.3 step 6).
func markClaimed(ctx context.Context, tx *sql.Tx, input ClaimInput, c *pendingCandidate, job *ExecutionJob) error {
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: c.taskID, ExecutionID: c.executionID,
		FromTask: model.TaskQueued, FromExecution: model.ExecutionPending, ToExecution: model.ExecutionClaimed,
	}); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE executions SET runner_id = ?, claimed_at = ? WHERE id = ? AND status = 'claimed'`,
		input.RunnerID, time.Now().UTC().Format(time.RFC3339), c.executionID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	if _, err := AppendExecutionEvent(ctx, tx, c.executionID, model.EventStatusChanged,
		json.RawMessage(`{"from":"pending","to":"claimed","reason":"runner_claimed"}`)); err != nil {
		return err
	}
	detail := map[string]any{"taskId": c.taskID, "from": model.ExecutionPending, "to": model.ExecutionClaimed,
		"routeKind": job.Route.Kind, "harnessId": job.Profile.HarnessID, "profileId": job.Profile.ID,
		"profileCommitSha": job.Profile.CommitSHA, "workspaceId": job.Workspace.ID}
	if job.Route.ProjectID != nil {
		detail["projectId"] = *job.Route.ProjectID
	}
	if job.Execution.ResumeStage != nil {
		detail["resumeStage"] = *job.Execution.ResumeStage
	}
	_, err = RecordAuditLog(ctx, tx, AuditLog{
		Actor: "runner:" + input.RunnerID, Action: "execution.claim", TargetType: "execution", TargetID: c.executionID,
		Detail: detail,
	})
	return err
}

func readClaimTask(ctx context.Context, tx *sql.Tx, taskID string, out *contract.RequestTask) error {
	var identifier, url sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT t.id, t.title, t.body, t.acceptance_criteria, t.source,
		l.issue_identifier, l.issue_url
		FROM tasks t LEFT JOIN linear_issue_links l ON l.task_id = t.id WHERE t.id = ?`, taskID).
		Scan(&out.ID, &out.Title, &out.Body, &out.AcceptanceCriteria, &out.Source.Kind, &identifier, &url)
	if err != nil {
		return err
	}
	// contract 6.1: manual sources carry null identifier and url.
	if out.Source.Kind == model.SourceLinear && identifier.Valid {
		out.Source.Identifier, out.Source.URL = &identifier.String, &url.String
	}
	return nil
}

// claimTimeouts returns the declared stages' timeoutSec. When enforce is set
// (a claim that pins the version), a value above the current
// limits.stageTimeoutMaxSec is a config_error (Q135).
func claimTimeouts(definition claimDefinition, settings map[string]string, enforce bool) (map[model.Stage]int, error) {
	raw, present := settings["limits.stageTimeoutMaxSec"]
	limits := registry.ParseStageTimeoutMaxSec(raw, present)
	timeouts := map[model.Stage]int{
		model.StageWork: definition.Stages.Work.TimeoutSec, model.StageCheck: definition.Stages.Check.TimeoutSec,
		model.StagePublish: definition.Stages.Publish.TimeoutSec,
	}
	if definition.Stages.Prepare != nil {
		timeouts[model.StagePrepare] = definition.Stages.Prepare.TimeoutSec
	}
	if !enforce {
		return timeouts, nil
	}
	for stage, timeout := range timeouts {
		if max := limits[stage]; max < 1 || timeout > max {
			return nil, configError("stages.%s.timeoutSec exceeds limits.stageTimeoutMaxSec", stage)
		}
	}
	return timeouts, nil
}

// claimWorkspace inherits the task's latest active/retained workspace for
// mode=continue, otherwise creates <workspaceRoot>/<id> (spec 04 3.3 step 4).
func claimWorkspace(ctx context.Context, tx *sql.Tx, input ClaimInput, c *pendingCandidate) (ClaimWorkspace, error) {
	var w ClaimWorkspace
	if c.mode == model.ModeContinue {
		err := tx.QueryRowContext(ctx, `SELECT id, path FROM task_workspaces
			WHERE task_id = ? AND status IN ('active', 'retained')
			ORDER BY created_at DESC, rowid DESC LIMIT 1`, c.taskID).Scan(&w.ID, &w.Path)
		if err == nil {
			return w, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return w, err
		}
	}
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return w, err
	}
	w = ClaimWorkspace{ID: id.String(), Path: filepath.Join(input.WorkspaceRoot, id.String()), Created: true}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_workspaces (id, task_id, runner_id, path, status, created_by_execution_id, created_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?)`, w.ID, c.taskID, input.RunnerID, w.Path, c.executionID,
		time.Now().UTC().Format(time.RFC3339Nano))
	return w, err
}

// previousExecution is the same task's Execution with seq-1 (spec 04 3.3.1).
func previousExecution(ctx context.Context, tx *sql.Tx, taskID string, seq int) (*contract.PreviousExecution, error) {
	var p contract.PreviousExecution
	var content, outputs sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, content_id, outputs FROM executions WHERE task_id = ? AND seq = ?`,
		taskID, seq-1).Scan(&p.ExecutionID, &content, &outputs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if content.Valid {
		p.CheckedContentID = &content.String
	}
	p.Outputs = []contract.Output{}
	if outputs.Valid && outputs.String != "" {
		if err := json.Unmarshal([]byte(outputs.String), &p.Outputs); err != nil {
			return nil, fmt.Errorf("store: Execution %s has invalid outputs", p.ExecutionID)
		}
	}
	return &p, nil
}

// failClaimConfig makes a candidate failed (config_error). The transition
// records the notification request (spec 04 3.3, spec 12).
func failClaimConfig(ctx context.Context, tx *sql.Tx, c *pendingCandidate, message string) error {
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: c.taskID, ExecutionID: c.executionID,
		FromTask: model.TaskQueued, FromExecution: model.ExecutionPending, ToExecution: model.ExecutionFailed,
	}); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ?, error_message = ? WHERE id = ? AND status = 'failed'`,
		model.ErrorConfig, message, c.executionID)
	if err := expectOneRow(result, err); err != nil {
		return err
	}
	_, err = AppendExecutionEvent(ctx, tx, c.executionID, model.EventStatusChanged,
		json.RawMessage(`{"from":"pending","to":"failed","reason":"config_error"}`))
	return err
}
