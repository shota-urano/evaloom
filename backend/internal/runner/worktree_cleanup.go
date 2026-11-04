package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// WorktreeCleanupRepository excludes lifecycle changes from eligibility checks
// through removal and persistence. Its delivery/trigger is unspecified; this is
// an internal boundary, not permission to add an HTTP API or access DB on Runner.
type WorktreeCleanupRepository interface {
	WithWorktreeCleanup(context.Context, string, string, func(context.Context, model.WorktreeCleanupTarget) error) error
}

type WorktreeRemover interface {
	Remove(context.Context, worktree.RemoveRequest) error
}

// CleanupWorktree is explicitly invoked by a future cleanup trigger, never on
// Execution completion. Stop Project sandbox/Worktree Root writers until this
// returns. Open/unknown PRs, missing dependencies and failed checks retain data.
func CleanupWorktree(ctx context.Context, runnerID, worktreeID string, records WorktreeCleanupRepository,
	remover WorktreeRemover, prs publisher.PreviousPullRequest, auditor WorktreePathAuditor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runnerID == "" || worktreeID == "" || records == nil || remover == nil || prs == nil || auditor == nil {
		return fmt.Errorf("runner: cleanup identity and dependencies are required")
	}
	var rejectedExecution string
	err := records.WithWorktreeCleanup(ctx, runnerID, worktreeID, func(ctx context.Context, target model.WorktreeCleanupTarget) error {
		if target.PRNumber <= 0 {
			return fmt.Errorf("runner: cleanup requires a recorded pull request")
		}
		state, err := prs.State(ctx, target.RepositoryURL, target.PRNumber)
		if err != nil || state != publisher.PRClosed && state != publisher.PRMerged {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("runner: cleanup requires a closed or merged pull request")
		}
		err = remover.Remove(ctx, worktree.RemoveRequest{ProjectID: target.ProjectID, RepositoryURL: target.RepositoryURL,
			Path: target.Path, BranchName: target.BranchName})
		if errors.Is(err, worktree.ErrWorktreePath) {
			rejectedExecution = target.ExecutionID
		}
		return err
	})
	// Audit only after the repository releases its transaction; audit delivery
	// can use the same Control Plane writer. Never log raw DB/transport errors.
	if rejectedExecution != "" {
		if auditErr := auditor.RecordWorktreePathRejection(ctx, rejectedExecution); auditErr != nil {
			return fmt.Errorf("runner: cleanup path rejection audit failed")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, worktree.ErrWorktreePath) || errors.Is(err, worktree.ErrGitState) {
		return err
	}
	if err != nil {
		return fmt.Errorf("runner: worktree cleanup failed")
	}
	return nil
}
