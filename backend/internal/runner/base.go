package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// BaseFetcher is supplied by worktree.Repositories, after Prepare has returned.
// It deliberately takes no owner Local Repository Path. This is only the new
// worktree path; continue/reuse must retain its previously recorded start SHA.
type BaseFetcher interface {
	FetchBase(context.Context, string, string, string) (string, error)
}

// RecordRemoteBase is the fetch-and-record portion of stage 3. Worktree creation
// remains a separate handler responsibility. The immutable SHA is acknowledged
// by the Control Plane before it is made available to context/worktree stages.
func (e *Execution) RecordRemoteBase(ctx context.Context, fetcher BaseFetcher) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || e.control == nil || e.status != model.ExecutionPreparing || fetcher == nil {
		return fmt.Errorf("runner: controlled preparing execution and Base Branch fetcher are required")
	}
	if !e.newWorktreeRequired() {
		return fmt.Errorf("runner: new worktree path is required for Base Branch fetch")
	}
	if e.BaseSHA != "" {
		return fmt.Errorf("runner: start SHA has already been acquired")
	}
	project := e.Claim.Project
	sha, err := fetcher.FetchBase(ctx, project.ID, project.GitRemoteURL, project.BaseBranch)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		// Transport errors may contain credentials. Only a fixed diagnostic
		// crosses the Runner's reporting/logging boundary.
		return errors.Join(fmt.Errorf("runner: fetch Base Branch failed"),
			e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "リモートのBase Branchを取得できない"))
	}
	if err := e.control.Reporter.ReportResult(e.control.ReportContext, e.Claim.Execution.ID, ExecutionResult{
		FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing, BaseSHA: sha,
	}); err != nil {
		return err
	}
	e.BaseSHA = sha
	return nil
}
