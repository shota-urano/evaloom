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
// 3.3). Blocked, waiting_approval, merging (spec 17 3.9, Q59) and terminal
// Executions do not occupy a slot.
const activeExecutionStatuses = `('claimed', 'preparing', 'running', 'verifying', 'publishing')`

var ErrClaimAssignmentMissing = errors.New("Project has no Default Harness assignment")

type ClaimExecution struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	Seq    int    `json:"seq"`
	Mode   string `json:"mode"`
}

// ClaimProject is the Project snapshot sent to the Runner.
type ClaimProject struct {
	model.Project
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

// Claim contains only execution input, never Runner credentials. Assignment is
// internal metadata from the saved pin, or the current assignment on first claim.
type Claim struct {
	Execution     ClaimExecution       `json:"execution"`
	Task          ClaimTask            `json:"task"`
	Project       ClaimProject         `json:"project"`
	Profile       ClaimProfile         `json:"profile"`
	Assignment    model.ProjectHarness `json:"-"`
	profilePinned bool
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
	err := tx.QueryRowContext(ctx, `SELECT e.id, e.task_id, e.seq, e.mode,
		t.project_id, t.title, t.body, t.acceptance_criteria, t.short_name,
		e.role, e.harness_id, e.profile_id, e.profile_version, e.profile_commit_sha
		FROM executions e JOIN tasks t ON t.id = e.task_id
		JOIN projects p ON p.id = t.project_id
		WHERE e.status = 'pending' AND p.runner_id = ?
		AND NOT EXISTS (SELECT 1 FROM executions a JOIN tasks at ON at.id = a.task_id
			WHERE at.project_id = p.id AND a.status IN `+activeExecutionStatuses+`)
		ORDER BY e.created_at, e.id LIMIT 1`, runnerID).
		Scan(&c.Execution.ID, &c.Execution.TaskID, &c.Execution.Seq, &c.Execution.Mode,
			&projectID, &c.Task.Title, &c.Task.Body,
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
	c.Profile = ClaimProfile{HarnessID: harness.ID, Adapter: harness.Adapter,
		Version: profile.Version, CommitSHA: profile.CommitSHA, Definition: profile.Definition,
		HarnessRepoPath: harness.RepoPath, ToolPermissions: harness.ToolPermissions}
	if c.profilePinned {
		c.Profile.Version = version.String
		c.Profile.CommitSHA = commitSHA.String
	}
	return &c, nil
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
