package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

// ErrGitState stops continue without changing the branch or working files.
var ErrGitState = errors.New("worktree: Git state requires review")

type ReuseRequest struct {
	ProjectID, RepositoryURL, Path, BranchName, BaseSHA, ExpectedHeadSHA string
	CopiedFiles                                                          []model.CopiedFile
	// BeforeRecreate must durably record restore start, mark the missing
	// directory cleaned and invalidate its old setup/copy snapshot BEFORE Git
	// creates a new one. An unfinished generation must block the next continue.
	BeforeRecreate func(context.Context) error
}

type Reused struct {
	HeadSHA   string
	Recreated bool
}

// Reuse checks the saved branch and last known commit without fetching or
// merging. A missing directory is recreated from the existing local branch.
// Stop sandbox and Worktree Root writers until persistence/copying finishes.
func (r Repositories) Reuse(ctx context.Context, request ReuseRequest) (Reused, error) {
	if err := ctx.Err(); err != nil {
		return Reused{}, err
	}
	if !projectSlug.MatchString(request.ProjectID) || !networkURL(request.RepositoryURL) ||
		!filepath.IsAbs(r.WorktreeRoot) || !filepath.IsAbs(request.Path) ||
		!commitPattern.MatchString(request.BaseSHA) || !commitPattern.MatchString(request.ExpectedHeadSHA) || request.BranchName == "" {
		return Reused{}, fmt.Errorf("worktree: invalid reuse configuration")
	}
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil {
		return Reused{}, ErrWorktreePath
	}
	// Only the direct task directories created by Create are valid. In
	// particular, never check out into the clone/storage or through an ancestor.
	parent, err := filepath.EvalSymlinks(filepath.Dir(request.Path))
	if err != nil || parent != root {
		return Reused{}, ErrWorktreePath
	}
	path := filepath.Join(root, filepath.Base(request.Path))
	if path == filepath.Join(root, repositoryDirectory) || strings.HasPrefix(filepath.Base(path), ".") {
		return Reused{}, ErrWorktreePath
	}
	storage := filepath.Join(root, repositoryDirectory)
	if err := containedDirectory(root, storage); err != nil {
		return Reused{}, err
	}
	unlock, err := lockRepository(ctx, filepath.Join(storage, "."+request.ProjectID+".lock"))
	if err != nil {
		return Reused{}, err
	}
	defer unlock()
	repository := filepath.Join(storage, request.ProjectID)
	if err := validateClone(ctx, root, repository, request.RepositoryURL); err != nil {
		return Reused{}, err
	}
	ref := "refs/heads/" + request.BranchName
	if _, err := safegit.Output(ctx, repository, "check-ref-format", ref); err != nil {
		return Reused{}, operationError(ctx, "validate reuse branch")
	}
	_, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		out, err := safegit.Output(ctx, repository, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil {
			return Reused{}, ErrGitState
		}
		result := Reused{HeadSHA: strings.TrimSpace(string(out))}
		if result.HeadSHA != request.ExpectedHeadSHA {
			return result, ErrGitState
		}
		if request.BeforeRecreate == nil {
			return result, fmt.Errorf("worktree: restore checkpoint is required")
		}
		if err := request.BeforeRecreate(ctx); err != nil {
			return result, err
		}
		// Clear only this absent directory's stale registration. A global prune
		// could change other tasks. Locked registrations are not forced away.
		listed, err := safegit.Output(ctx, repository, "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return result, operationError(ctx, "list worktrees")
		}
		for _, field := range strings.Split(string(listed), "\x00") {
			if field == "worktree "+path {
				if _, err := safegit.Output(ctx, repository, "worktree", "remove", "--", path); err != nil {
					return result, operationError(ctx, "clear missing worktree registration")
				}
			}
		}
		// No --force: a branch still checked out elsewhere must be preserved.
		if err := os.Mkdir(path, 0700); err != nil {
			return result, ErrWorktreePath
		}
		if _, err := safegit.Output(ctx, repository, "worktree", "add", "--", path, request.BranchName); err != nil {
			return result, operationError(ctx, "recreate worktree")
		}
		if err := validateLinkedWorktree(ctx, root, repository, path); err != nil {
			return result, err
		}
		// Verify the actual checkout too: a tag with the same short name or
		// sandbox-written Git settings must not select a different commit.
		head, clean, checkErr := gate.CheckWorktree(ctx, path, nil)
		branch, branchErr := safegit.Output(ctx, path, "symbolic-ref", "-q", "HEAD")
		result.HeadSHA = head
		if checkErr != nil || branchErr != nil || !clean || head != request.ExpectedHeadSHA || strings.TrimSpace(string(branch)) != ref {
			return result, ErrGitState
		}
		result.Recreated = true
		return result, nil
	}
	if err != nil {
		return Reused{}, ErrWorktreePath
	}
	if err := validateLinkedWorktree(ctx, root, repository, path); err != nil {
		return Reused{}, err
	}
	head, clean, err := gate.CheckWorktree(ctx, path, request.CopiedFiles)
	result := Reused{HeadSHA: head}
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrGitState
	}
	branch, err := safegit.Output(ctx, path, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != ref || !clean || head != request.ExpectedHeadSHA {
		return result, ErrGitState
	}
	return result, nil
}

func validateLinkedWorktree(ctx context.Context, root, repository, path string) error {
	if err := containedDirectory(root, path); err != nil {
		return ErrWorktreePath
	}
	// Inspect the pointer before invoking Git: an arbitrary .git must never
	// cause host operations to open the owner's Git area or another clone.
	pointer := filepath.Join(path, ".git")
	info, err := os.Lstat(pointer)
	if err != nil || !info.Mode().IsRegular() {
		return ErrWorktreePath
	}
	body, err := os.ReadFile(pointer)
	if err != nil {
		return ErrWorktreePath
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSuffix(string(body), "\n"), "gitdir: ")
	if !ok {
		return ErrWorktreePath
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if filepath.Dir(gitdir) != filepath.Join(repository, ".git", "worktrees") || containedDirectory(root, gitdir) != nil {
		return ErrWorktreePath
	}
	for _, query := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, filepath.Join(repository, ".git")},
		{[]string{"rev-parse", "--show-toplevel"}, path},
	} {
		out, err := safegit.Output(ctx, path, query.args...)
		if err != nil || strings.TrimSpace(string(out)) != query.want {
			return ErrWorktreePath
		}
	}
	return nil
}
