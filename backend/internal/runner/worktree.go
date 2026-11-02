package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// WorktreeCreator is implemented by worktree.Repositories. It only creates the
// branch/directory; copying and recording the worktrees row follow this step.
type WorktreeCreator interface {
	Create(context.Context, worktree.CreateRequest) (worktree.Created, error)
}

// WorktreePathAuditor is the Control Plane audit boundary for spec 06 section 6.
// The Runner cannot open the management DB. The specs do not yet define the
// wire format for sending this rejection; no new public API is assumed here.
type WorktreePathAuditor interface {
	RecordWorktreePathRejection(context.Context, string) error
}

// CreateWorktree is stage 3's new-creation portion, after RecordRemoteBase.
// freshNumber comes from a task-wide fresh counter, not the Execution sequence.
// The claim contract has no such counter, so its acquisition remains external.
func (e *Execution) CreateWorktree(ctx context.Context, creator WorktreeCreator, freshNumber int, auditor WorktreePathAuditor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || e.control == nil || e.status != model.ExecutionPreparing || creator == nil || auditor == nil {
		return fmt.Errorf("runner: controlled preparing execution, worktree creator and path auditor are required")
	}
	if !e.newWorktreeRequired() || e.BaseSHA == "" || e.WorktreePath != "" || e.WorktreeID != "" || e.BranchName != "" {
		return fmt.Errorf("runner: recorded start SHA and new worktree path are required")
	}
	if e.Claim.Execution.Mode == "fresh" && freshNumber <= 0 || e.Claim.Execution.Mode == "continue" && freshNumber != 0 {
		return fmt.Errorf("runner: fresh ordinal must match execution mode")
	}
	created, err := creator.Create(ctx, worktree.CreateRequest{
		ProjectID: e.Claim.Project.ID, RepositoryURL: e.Claim.Project.GitRemoteURL,
		TaskID: e.Claim.Execution.TaskID, ShortName: e.Claim.Task.ShortName,
		BaseSHA: e.BaseSHA, FreshNumber: freshNumber,
	})
	var auditFailure error
	if errors.Is(err, worktree.ErrWorktreePath) {
		// Use the delivery context so cancellation cannot skip a required audit.
		if auditErr := auditor.RecordWorktreePathRejection(e.control.ReportContext, e.Claim.Execution.ID); auditErr != nil {
			auditFailure = fmt.Errorf("runner: worktree path rejection audit failed")
		}
	}
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), auditFailure)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("runner: create worktree failed"), auditFailure,
			e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "worktreeを新規作成できない"))
	}
	e.WorktreePath, e.BranchName = created.Path, created.BranchName
	return nil
}

func (e *Execution) newWorktreeRequired() bool {
	return e.Claim.Execution.Mode == "fresh" || e.Claim.Execution.Mode == "continue" && e.Claim.Worktree == nil &&
		(e.Claim.Execution.Seq <= 1 || e.Claim.worktreeSnapshotPresent)
}
