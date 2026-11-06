package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// ReadGitCredentials is supplied by publisher.Reader. PrepareGit issues one
// read-only token per stage and registers it before returning a usable transport.
type ReadGitCredentials interface {
	PrepareGit(context.Context, func(...string)) (worktree.ReadTransport, error)
}

// PrepareReadRepository begins stage 3's new-clone path. Use the returned copy
// for RecordRemoteBase and CreateWorktree within this stage, then discard it.
// The credentials and transport must never be stored on Execution or shared
// across stages. Reuse/restore routing remains the calling stage's responsibility.
func (e *Execution) PrepareReadRepository(ctx context.Context, repositories worktree.Repositories, credentials ReadGitCredentials) (worktree.Repositories, error) {
	if err := ctx.Err(); err != nil {
		return worktree.Repositories{}, err
	}
	if e == nil || e.control == nil || e.status != model.ExecutionPreparing || credentials == nil || !e.newWorktreeRequired() {
		return worktree.Repositories{}, fmt.Errorf("runner: preparing execution, new worktree and read credentials are required")
	}
	transport, err := credentials.PrepareGit(ctx, func(values ...string) { e.logSecrets = append(e.logSecrets, values...) })
	if err == nil && transport != nil {
		repositories.Transport = transport
		_, err = repositories.Prepare(ctx, e.Claim.Project.ID, e.Claim.Project.GitRemoteURL)
	} else if err == nil {
		err = fmt.Errorf("runner: read transport is missing")
	}
	if ctx.Err() != nil {
		return worktree.Repositories{}, ctx.Err()
	}
	if err != nil {
		// Discard authentication/Git diagnostics; neither raw GitHub responses
		// nor credential values may reach result, events or the caller's logs.
		return worktree.Repositories{}, errors.Join(fmt.Errorf("runner: prepare read repository failed"),
			e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "読み取り用の認証情報または作業用クローンを準備できない"))
	}
	return repositories, nil
}

// LogSecrets supplies NewLog's exclusion list for all later Execution streams.
// The returned copy belongs only to Trusted Runner logging, never to a Sandbox.
func (e *Execution) LogSecrets() []string { return append([]string(nil), e.logSecrets...) }
