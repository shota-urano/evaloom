package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

// FetchBase fetches exactly the registered Base Branch in an already prepared
// Runner-owned clone. The returned commit is independent of local HEAD and
// FETCH_HEAD. Callers must stop sandbox writers, as with Prepare.
func (r Repositories) FetchBase(ctx context.Context, projectID, repositoryURL, baseBranch string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !projectSlug.MatchString(projectID) || !networkURL(repositoryURL) || !filepath.IsAbs(r.WorktreeRoot) {
		return "", fmt.Errorf("worktree: invalid repository configuration")
	}
	if baseBranch == "" || strings.ContainsAny(baseBranch, "\x00\r\n") {
		return "", fmt.Errorf("worktree: invalid Base Branch")
	}
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil {
		return "", fmt.Errorf("worktree: resolve Worktree Root")
	}
	storage := filepath.Join(root, repositoryDirectory)
	if err := containedDirectory(root, storage); err != nil {
		return "", err
	}
	unlock, err := lockRepository(ctx, filepath.Join(storage, "."+projectID+".lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	repository := filepath.Join(storage, projectID)
	if err := validateClone(ctx, root, repository, repositoryURL); err != nil {
		return "", err
	}
	source := "refs/heads/" + baseBranch
	if _, err := safegit.Output(ctx, repository, "check-ref-format", source); err != nil {
		return "", operationError(ctx, "validate Base Branch")
	}
	transport := r.Transport
	if transport == nil {
		transport = safeTransport{}
	}
	// Use the registered URL and an explicit refspec, rather than origin's
	// sandbox-writable fetch mapping. '+' also handles a remote force-push.
	// A missing remote branch MUST fail, even if its old tracking ref remains.
	target := "refs/remotes/origin/" + baseBranch
	if _, err := transport.Output(ctx, repository, "fetch", "--no-tags", "--refmap=", "--", repositoryURL, "+"+source+":"+target); err != nil {
		return "", operationError(ctx, "fetch Base Branch")
	}
	out, err := safegit.Output(ctx, repository, "rev-parse", "--verify", "--end-of-options", target+"^{commit}")
	if err != nil {
		return "", operationError(ctx, "resolve Base Branch commit")
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}
