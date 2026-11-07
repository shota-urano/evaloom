package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Keep the active set identical for the global and per-Project limits (spec 04,
// 3.3). Blocked and terminal Executions do not occupy a slot.
const activeExecutionStatuses = `('claimed', 'preparing', 'running', 'verifying', 'publishing')`

var ErrClaimAssignmentMissing = errors.New("Project has no Default Harness assignment")

type ClaimExecution struct {
	ID                   string            `json:"id"`
	TaskID               string            `json:"taskId"`
	Seq                  int               `json:"seq"`
	Mode                 string            `json:"mode"`
	ResumePoint          *string           `json:"resumePoint"`
	FreshNumber          *int              `json:"freshNumber"`
	ResumeHeadSHA        *string           `json:"resumeHeadSha"`
	HarnessHeadSHA       *string           `json:"harnessHeadSha"`
	VerifiedSHA          *string           `json:"verifiedSha"`
	VerifyRun            *model.TestRun    `json:"verifyRun"`
	ResumeBlock          *ClaimResumeBlock `json:"resumeBlock"`
	ResumeGateResults    []ClaimGateResult `json:"resumeGateResults"`
	ResumeHarnessSummary *string           `json:"resumeHarnessSummary"`
}

type ClaimResumeBlock struct {
	ID           string          `json:"id"`
	Cause        string          `json:"cause"`
	HeadSHA      string          `json:"headSha"`
	GateResultID *string         `json:"gateResultId"`
	Detail       json.RawMessage `json:"detail"`
	DecidedAt    *string         `json:"decidedAt"`
}

type ClaimGateResult struct {
	ID        string          `json:"id"`
	Gate      int             `json:"gate"`
	Result    string          `json:"result"`
	BaseSHA   string          `json:"baseSha"`
	TargetSHA string          `json:"targetSha"`
	Findings  []model.Finding `json:"findings"`
}

type ClaimPullRequest struct {
	Number     int     `json:"number"`
	BranchName *string `json:"branchName"`
	Seq        int     `json:"seq"`
}

// Setup host registration is a separate spec 03 unit. Keep its claim boundary
// closed until that Registry implementation supplies persisted destinations.
type ClaimProject struct {
	model.Project
	SetupAllowedHosts []string `json:"setupAllowedHosts"`
}

type ClaimTask struct {
	Title              string `json:"title"`
	Body               string `json:"body"`
	AcceptanceCriteria string `json:"acceptanceCriteria"`
	ShortName          string `json:"shortName"`
}

type ClaimProfile struct {
	HarnessID       string          `json:"harnessId"`
	Adapter         string          `json:"adapter"`
	Version         string          `json:"version"`
	CommitSHA       string          `json:"commitSha"`
	Definition      json.RawMessage `json:"definition"`
	HarnessRepoPath string          `json:"harnessRepoPath"`
	ToolPermissions map[string]any  `json:"toolPermissions"`
}

type ClaimPrevious struct {
	WorktreePath *string `json:"worktreePath"`
	BranchName   *string `json:"branchName"`
	PRNumber     *int    `json:"prNumber"`
}

// ClaimWorktree carries the original baseline with the worktree it measures.
// A continue claim gets this from its predecessor; a resumed claim gets its
// own saved association. A fresh worktree has no previous baseline.
type ClaimWorktree struct {
	ID                  string             `json:"id"`
	Path                string             `json:"path"`
	BaseSHA             string             `json:"baseSha"`
	BranchName          string             `json:"branchName"`
	SetupStatus         string             `json:"setupStatus"`
	Baseline            *ClaimBaseline     `json:"baseline"`
	CopiedFiles         []model.CopiedFile `json:"copiedFiles"`
	Status              string             `json:"status"`
	ExpectedHeadSHA     *string            `json:"expectedHeadSha"`
	PreparationComplete bool               `json:"preparationComplete"`
}

// ClaimBaseline adds artifact contents only to the claim snapshot, not to the
// persisted TestRun or result reporting schema. Unavailable IDs remain null.
type ClaimBaseline struct {
	model.TestRun
	TestIDs []string `json:"testIds"`
}

// Claim contains only execution input, never Runner credentials. Assignment is
// internal metadata from the saved pin, or the current assignment on first claim.
type Claim struct {
	Execution    ClaimExecution     `json:"execution"`
	Task         ClaimTask          `json:"task"`
	Project      ClaimProject       `json:"project"`
	Profile      ClaimProfile       `json:"profile"`
	Previous     *ClaimPrevious     `json:"previous"`
	Worktree     *ClaimWorktree     `json:"worktree"`
	PullRequests []ClaimPullRequest `json:"pullRequests"`
	// Artifact selection is captured in the transaction; bytes are read after commit.
	HarnessOutputArtifactID *string              `json:"-"`
	Assignment              model.ProjectHarness `json:"-"`
	profilePinned           bool
}

// NextClaim reads capacity, the oldest eligible Execution and its Registry
// inputs under the caller's BEGIN IMMEDIATE transaction. Nil means no work.
// The caller must transition, pin and commit before returning this input.
func NextClaim(ctx context.Context, tx *sql.Tx, runnerID string) (*Claim, error) {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM executions WHERE status IN `+activeExecutionStatuses).Scan(&active); err != nil {
		return nil, err
	}
	if active >= 2 {
		return nil, nil
	}
	var c Claim
	var projectID string
	var role, harnessID, profileID, version, commitSHA sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT e.id, e.task_id, e.seq, e.mode, e.resume_point,
		t.project_id, t.title, t.body, t.acceptance_criteria, t.short_name,
		e.role, e.harness_id, e.profile_id, e.profile_version, e.profile_commit_sha
		FROM executions e JOIN tasks t ON t.id = e.task_id
		JOIN projects p ON p.id = t.project_id
		WHERE e.status = 'pending' AND p.runner_id = ?
		AND NOT EXISTS (SELECT 1 FROM executions a JOIN tasks at ON at.id = a.task_id
			WHERE at.project_id = p.id AND a.status IN `+activeExecutionStatuses+`)
		ORDER BY e.created_at, e.id LIMIT 1`, runnerID).
		Scan(&c.Execution.ID, &c.Execution.TaskID, &c.Execution.Seq, &c.Execution.Mode,
			&c.Execution.ResumePoint, &projectID, &c.Task.Title, &c.Task.Body,
			&c.Task.AcceptanceCriteria, &c.Task.ShortName,
			&role, &harnessID, &profileID, &version, &commitSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Project.Project, err = GetProject(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	c.profilePinned = profileID.Valid
	if c.profilePinned {
		// Blocked continuation keeps the same Execution and Profile (spec 04,
		// 3.8 and 4). Its input must not depend on today's Default Role or pin.
		c.Assignment = model.ProjectHarness{
			ProjectID: projectID, Role: role.String, HarnessID: harnessID.String, ProfileID: profileID.String,
		}
	} else {
		c.Assignment, err = GetProjectHarness(ctx, tx, projectID, c.Project.DefaultRole)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrClaimAssignmentMissing
		}
		if err != nil {
			return nil, err
		}
	}
	profile, err := GetHarnessProfileByID(ctx, tx, c.Assignment.ProfileID)
	if err != nil {
		return nil, err
	}
	harness, err := GetHarness(ctx, tx, c.Assignment.HarnessID)
	if err != nil {
		return nil, err
	}
	c.Project.SetupAllowedHosts = []string{}
	c.Profile = ClaimProfile{HarnessID: harness.ID, Adapter: harness.Adapter,
		Version: profile.Version, CommitSHA: profile.CommitSHA, Definition: profile.Definition,
		HarnessRepoPath: harness.RepoPath, ToolPermissions: harness.ToolPermissions}
	if c.profilePinned {
		c.Profile.Version = version.String
		c.Profile.CommitSHA = commitSHA.String
	}
	if c.Execution.Seq > 1 && c.Execution.Mode == "continue" {
		var previous ClaimPrevious
		err := tx.QueryRowContext(ctx, `SELECT w.path, e.branch_name, e.pr_number
			FROM executions e LEFT JOIN worktrees w ON w.id = e.worktree_id
			WHERE e.task_id = ? AND e.seq = ?`, c.Execution.TaskID, c.Execution.Seq-1).
			Scan(&previous.WorktreePath, &previous.BranchName, &previous.PRNumber)
		if err != nil {
			return nil, err
		}
		c.Previous = &previous
	}
	if err := loadClaimWorktree(ctx, tx, &c, runnerID); err != nil {
		return nil, err
	}
	if err := loadClaimAdditions(ctx, tx, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func loadClaimWorktree(ctx context.Context, tx *sql.Tx, c *Claim, runnerID string) error {
	var worktreeID, base sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT worktree_id, base_sha FROM executions WHERE id = ?`, c.Execution.ID).Scan(&worktreeID, &base)
	if err != nil {
		return err
	}
	if !worktreeID.Valid && c.Execution.Mode == "continue" && c.Execution.Seq > 1 {
		err = tx.QueryRowContext(ctx, `SELECT worktree_id, base_sha FROM executions WHERE task_id = ? AND seq = ?`, c.Execution.TaskID, c.Execution.Seq-1).Scan(&worktreeID, &base)
		if err != nil {
			return err
		}
	}
	if !worktreeID.Valid {
		return nil
	}
	w := &ClaimWorktree{ID: worktreeID.String}
	var copied string
	// A cleaned worktree retains its branch and start SHA (spec 06, 3.3.6).
	// Preparation needs this snapshot to restore it and reuse its baseline.
	err = tx.QueryRowContext(ctx, `SELECT path, base_sha, branch_name, copied_files, setup_status, status FROM worktrees WHERE id = ? AND task_id = ? AND project_id = ? AND runner_id = ?`, w.ID, c.Execution.TaskID, c.Project.ID, runnerID).Scan(&w.Path, &w.BaseSHA, &w.BranchName, &copied, &w.SetupStatus, &w.Status)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(copied), &w.CopiedFiles); err != nil {
		return err
	}
	if base.Valid && base.String != w.BaseSHA {
		return ErrBaselineScope
	}
	baseline, err := findBaseline(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	if baseline != nil {
		w.Baseline = &ClaimBaseline{TestRun: *baseline}
	}
	if w.Baseline != nil && w.Baseline.TargetSHA != w.BaseSHA {
		return ErrBaselineScope
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT coalesce(nullif(pushed_sha,''),nullif(harness_head_sha,''),base_sha)
		FROM executions WHERE task_id=? AND seq=?)`, c.Execution.TaskID, c.Execution.Seq-1).Scan(&w.ExpectedHeadSHA); err != nil {
		return err
	}
	ready, err := WorktreeFilesReady(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	setupReady := w.SetupStatus == "succeeded"
	if ready && w.SetupStatus == "pending" {
		// Require the saved skip record in the current preparation generation;
		// today's setup command alone cannot prove preparation was completed.
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worktrees w
			JOIN executions h ON h.task_id=w.task_id AND h.runner_id=w.runner_id
			JOIN execution_events skip ON skip.execution_id=h.id AND skip.type='log'
			JOIN execution_events ready ON ready.execution_id=h.id AND ready.type='worktree_ready' AND ready.id<skip.id
			WHERE w.id=? AND ((h.worktree_id=w.id AND h.base_sha=w.base_sha AND h.branch_name=w.branch_name)
				OR EXISTS(SELECT 1 FROM audit_logs receipt WHERE receipt.target_type='execution' AND receipt.target_id=h.id
					AND receipt.action='execution.result' AND json_extract(receipt.detail,'$.step')='worktree.restored'
					AND json_extract(receipt.detail,'$.report.id')=w.id))
			AND json_extract(ready.payload,'$.path')=w.path
			AND json_extract(ready.payload,'$.branch')=w.branch_name
			AND json_extract(ready.payload,'$.baseSha')=w.base_sha
			AND json_extract(skip.payload,'$.sourceId')=?
			AND json_extract(skip.payload,'$.stream')='runner'
			AND NOT EXISTS(SELECT 1 FROM executions r JOIN execution_events start ON start.execution_id=r.id
				WHERE r.task_id=h.task_id AND start.type='log' AND json_extract(start.payload,'$.sourceId')=?
				AND (r.seq>h.seq OR (r.seq=h.seq AND start.id>ready.id))))`,
			w.ID, setupSkippedPrefix+w.ID, restoreSourcePrefix+w.ID).Scan(&setupReady)
		if err != nil {
			return err
		}
	}
	w.PreparationComplete = ready && setupReady
	c.Worktree = w
	return nil
}

// PinClaim saves Registry values only on first claim, after the guarded pending
// -> claimed transition in the same transaction. Reclaim only renews acquisition
// metadata, preserving the Execution's fixed role, Harness and Profile.
func PinClaim(ctx context.Context, tx *sql.Tx, c *Claim, runnerID, claimedAt string) error {
	var result sql.Result
	var err error
	if c.profilePinned {
		result, err = tx.ExecContext(ctx, `UPDATE executions SET runner_id = ?, claimed_at = ?
			WHERE id = ? AND status = 'claimed'`, runnerID, claimedAt, c.Execution.ID)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE executions SET runner_id = ?, role = ?,
			harness_id = ?, profile_id = ?, profile_version = ?, profile_commit_sha = ?, claimed_at = ?
			WHERE id = ? AND status = 'claimed' AND profile_id IS NULL`, runnerID, c.Assignment.Role,
			c.Assignment.HarnessID, c.Assignment.ProfileID, c.Profile.Version, c.Profile.CommitSHA,
			claimedAt, c.Execution.ID)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("store: claim metadata was not saved to exactly one Execution")
	}
	return err
}
