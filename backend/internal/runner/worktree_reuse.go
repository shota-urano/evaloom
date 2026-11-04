package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

type WorktreeReuser interface {
	Reuse(context.Context, worktree.ReuseRequest) (worktree.Reused, error)
	WorktreeCopier
}

// WorktreeReuseRepository is a Control Plane delivery boundary. The current
// claim omits the predecessor's pushed/harness SHA and worktree status; copy
// registration likewise has no defined wire shape. Do not invent an endpoint
// or open the DB in Runner to obtain these. Missing delivery fails closed.
type WorktreeReuseRepository interface {
	LoadWorktreeReuse(context.Context, string, string) (model.WorktreeReuseState, error)
	BeginWorktreeRestore(context.Context, string, string) error
	RecordRestoredCopies(context.Context, string, string, model.PreparedWorktree) error
	WarnMissingCopy(context.Context, string, string) error
}

// ReuseWorktree is stage 3 for continue with a saved worktree. Call it instead
// of RecordRemoteBase/CreateWorktree/CopyWorktreeFiles. It reports git_state
// through the existing result API and requires ACK before later stages run.
func (e *Execution) ReuseWorktree(ctx context.Context, reuser WorktreeReuser, history WorktreeReuseRepository, auditor WorktreePathAuditor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || e.control == nil || e.status != model.ExecutionPreparing || reuser == nil || history == nil || auditor == nil {
		return fmt.Errorf("runner: controlled preparing execution and reuse dependencies are required")
	}
	w := e.Claim.Worktree
	if e.Claim.Execution.Mode != "continue" || e.Claim.Execution.ResumePoint != nil || w == nil ||
		w.ID == "" || w.Path == "" || w.BaseSHA == "" || w.BranchName == "" || e.WorktreeID != "" || e.WorktreePath != "" {
		return fmt.Errorf("runner: continue with a saved worktree is required")
	}
	state, err := history.LoadWorktreeReuse(ctx, e.Claim.Execution.ID, w.ID)
	if err != nil || state.ExpectedHeadSHA == "" || state.Status != "active" && state.Status != "retained" && state.Status != "cleaned" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("runner: previous worktree state unavailable")
	}
	// Keep the predecessor checkpoint for EVERY outcome, including Git-state
	// blocks, operational failures and cancellation. The observed HEAD of an
	// unexpected checkout is only a Block measurement, never the checkpoint.
	// Leave WorktreePath unset until preparation is ACKed.
	e.WorktreeID, e.BaseSHA, e.BranchName = w.ID, w.BaseSHA, w.BranchName
	e.HarnessHeadSHA = state.ExpectedHeadSHA
	if !state.PreparationComplete {
		return errors.Join(worktree.ErrGitState, e.reportResult(model.ExecutionBlocked, "",
			&ResultBlock{Cause: model.BlockGitState, HeadSHA: state.ExpectedHeadSHA},
			"前回のworktreeの準備完了を確認できない。記録したSHAは前回の既知のSHAであり、現在のHEADは未確認"))
	}
	result, err := reuser.Reuse(ctx, worktree.ReuseRequest{
		ProjectID: e.Claim.Project.ID, RepositoryURL: e.Claim.Project.GitRemoteURL,
		Path: w.Path, BranchName: w.BranchName, BaseSHA: w.BaseSHA,
		ExpectedHeadSHA: state.ExpectedHeadSHA, CopiedFiles: w.CopiedFiles,
		BeforeRecreate: func(ctx context.Context) error {
			return history.BeginWorktreeRestore(ctx, e.Claim.Execution.ID, w.ID)
		},
	})
	var auditFailure error
	if errors.Is(err, worktree.ErrWorktreePath) {
		if auditErr := auditor.RecordWorktreePathRejection(e.control.ReportContext, e.Claim.Execution.ID); auditErr != nil {
			auditFailure = fmt.Errorf("runner: worktree path rejection audit failed")
		}
	}
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), auditFailure)
	}
	if err != nil {
		if errors.Is(err, worktree.ErrGitState) {
			if result.HeadSHA == "" {
				// A missing branch has no current HEAD. Label the reference SHA
				// explicitly, as Gate 3 does for an unreadable HEAD.
				return errors.Join(worktree.ErrGitState, e.reportResult(model.ExecutionBlocked, "",
					&ResultBlock{Cause: model.BlockGitState, HeadSHA: state.ExpectedHeadSHA},
					"現在のHEADを取得できない。記録したSHAは前回の既知のSHAであり、現在のHEADは未確認"))
			}
			return errors.Join(worktree.ErrGitState, e.reportBlock(model.ExecutionBlocked, "",
				&ResultBlock{Cause: model.BlockGitState, HeadSHA: result.HeadSHA}))
		}
		return errors.Join(fmt.Errorf("runner: reuse worktree failed"), auditFailure,
			e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "worktreeを再利用できない"))
	}
	copied := w.CopiedFiles
	if state.Status == "cleaned" && !result.Recreated {
		return errors.Join(worktree.ErrGitState, e.reportResult(model.ExecutionBlocked, "",
			&ResultBlock{Cause: model.BlockGitState, HeadSHA: result.HeadSHA},
			"削除済みの記録とworktreeの実ディレクトリが一致しない"))
	}
	if result.Recreated {
		copied, err = reuser.CopyFiles(ctx, e.Claim.Project.LocalRepoPath, w.Path, e.Claim.Project.CopyAllowlist,
			func(ctx context.Context, path string) error {
				return history.WarnMissingCopy(ctx, e.Claim.Execution.ID, path)
			})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errors.Join(fmt.Errorf("runner: restored worktree copy failed"),
				e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "コピー許可リストのファイルを配置できない"))
		}
		// Only a complete copy pass may produce a readiness record. An
		// interrupted pass keeps the restore-start checkpoint and blocks retry.
		if saveErr := history.RecordRestoredCopies(e.control.ReportContext, e.Claim.Execution.ID, w.ID, model.PreparedWorktree{
			Path: w.Path, BranchName: w.BranchName, BaseSHA: w.BaseSHA, CopiedFiles: copied,
		}); saveErr != nil {
			return errors.Join(ctx.Err(), fmt.Errorf("runner: record restored copies failed"))
		}
	}
	if err := e.control.Reporter.ReportResult(e.control.ReportContext, e.Claim.Execution.ID, ExecutionResult{
		FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		WorktreeID: w.ID, BaseSHA: w.BaseSHA, BranchName: w.BranchName,
	}); err != nil {
		return fmt.Errorf("runner: record reused worktree failed")
	}
	e.WorktreeID, e.WorktreePath, e.BranchName, e.BaseSHA = w.ID, w.Path, w.BranchName, w.BaseSHA
	e.CopiedFiles = copied
	// SetupStage consumes this snapshot and must rerun setup for a new
	// directory even when setup had succeeded in the removed one.
	if result.Recreated {
		updated := *w
		updated.CopiedFiles, updated.SetupStatus = copied, "pending"
		e.Claim.Worktree = &updated
	}
	return nil
}
