package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// WorktreeCopier is implemented by worktree.Repositories after Create.
type WorktreeCopier interface {
	CopyFiles(context.Context, string, string, []string, func(context.Context, string) error) ([]model.CopiedFile, error)
}

// WorktreeCopyRecorder is the Control Plane boundary. Implementations must save
// worktrees.copied_files and associate the row with the Execution before ACK.
// Missing sources require a log warning. No HTTP shape for worktree registration
// exists in the specs; Runner must not open the management DB to implement this.
type WorktreeCopyRecorder interface {
	RecordCopiedWorktree(context.Context, string, model.PreparedWorktree) (string, error)
	WarnMissingCopy(context.Context, string, string) error
}

// CopyWorktreeFiles follows RecordRemoteBase and CreateWorktree in stage 3.
// Resume/reuse and setup are separate stages. The caller emits worktree_ready
// only after this call succeeds; no readiness is announced for partial copies.
func (e *Execution) CopyWorktreeFiles(ctx context.Context, copier WorktreeCopier, recorder WorktreeCopyRecorder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || e.control == nil || e.status != model.ExecutionPreparing || copier == nil || recorder == nil {
		return fmt.Errorf("runner: controlled preparing execution, copier and recorder are required")
	}
	if !e.newWorktreeRequired() || e.WorktreePath == "" || e.BranchName == "" || e.BaseSHA == "" || e.WorktreeID != "" {
		return fmt.Errorf("runner: newly created unrecorded worktree is required")
	}
	copied, err := copier.CopyFiles(ctx, e.Claim.Project.LocalRepoPath, e.WorktreePath, e.Claim.Project.CopyAllowlist,
		func(ctx context.Context, path string) error {
			return recorder.WarnMissingCopy(ctx, e.Claim.Execution.ID, path)
		})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return errors.Join(fmt.Errorf("runner: worktree copy failed"),
			e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "コピー許可リストのファイルを配置できない"))
	}
	id, err := recorder.RecordCopiedWorktree(ctx, e.Claim.Execution.ID, model.PreparedWorktree{
		Path: e.WorktreePath, BranchName: e.BranchName, BaseSHA: e.BaseSHA, CopiedFiles: copied,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("runner: record copied worktree failed")
	}
	if id == "" {
		return fmt.Errorf("runner: missing recorded worktree ID")
	}
	e.WorktreeID, e.CopiedFiles = id, copied
	return nil
}
