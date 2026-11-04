package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

type RemoveRequest struct {
	ProjectID, RepositoryURL, Path, BranchName string
}

// Remove deletes only a linked task directory/registration. Branches are never
// deleted. The caller must check lifecycle/PR eligibility and stop Project and
// Worktree Root writers through persistence. No --force, reset, clean or prune.
func (r Repositories) Remove(ctx context.Context, request RemoveRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !projectSlug.MatchString(request.ProjectID) || !networkURL(request.RepositoryURL) ||
		!filepath.IsAbs(r.WorktreeRoot) || !filepath.IsAbs(request.Path) || request.BranchName == "" {
		return fmt.Errorf("worktree: invalid removal configuration")
	}
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil {
		return ErrWorktreePath
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(request.Path))
	if err != nil || parent != root || strings.HasPrefix(filepath.Base(request.Path), ".") {
		return ErrWorktreePath
	}
	path := filepath.Join(root, filepath.Base(request.Path))
	storage := filepath.Join(root, repositoryDirectory)
	if err := containedDirectory(root, storage); err != nil {
		return ErrWorktreePath
	}
	unlock, err := lockRepository(ctx, filepath.Join(storage, "."+request.ProjectID+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	repository := filepath.Join(storage, request.ProjectID)
	if err := validateClone(ctx, root, repository, request.RepositoryURL); err != nil {
		return err
	}
	ref := "refs/heads/" + request.BranchName
	if _, err := safegit.Output(ctx, repository, "check-ref-format", ref); err != nil {
		return ErrGitState
	}
	head, err := safegit.Output(ctx, repository, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return ErrGitState
	}
	_, statErr := os.Lstat(path)
	missing := errors.Is(statErr, os.ErrNotExist)
	if !missing {
		if statErr != nil || validateLinkedWorktree(ctx, root, repository, path) != nil {
			return ErrWorktreePath
		}
		branch, err := safegit.Output(ctx, path, "symbolic-ref", "-q", "HEAD")
		if err != nil || strings.TrimSpace(string(branch)) != ref {
			return ErrGitState
		}
	}
	listed, err := safegit.Output(ctx, repository, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return operationError(ctx, "list cleanup worktrees")
	}
	registered := false
	for _, record := range strings.Split(string(listed), "\x00\x00") {
		fields := strings.Split(record, "\x00")
		if len(fields) == 0 || fields[0] != "worktree "+path {
			continue
		}
		registered = true
		matches := false
		for _, field := range fields[1:] {
			if field == "branch "+ref {
				matches = true
			}
			if field == "locked" || strings.HasPrefix(field, "locked ") {
				return ErrGitState
			}
		}
		if !matches {
			return ErrGitState
		}
	}
	if !registered && !missing {
		return ErrGitState
	}
	if registered {
		if _, err := safegit.Output(ctx, repository, "worktree", "remove", "--", path); err != nil {
			return operationError(ctx, "remove worktree")
		}
	}
	// Also covers a previous deletion whose DB acknowledgement failed. Do not
	// remove a replacement directory or change the branch to repair a mismatch.
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return ErrWorktreePath
	}
	after, err := safegit.Output(ctx, repository, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || string(after) != string(head) {
		return ErrGitState
	}
	return nil
}
