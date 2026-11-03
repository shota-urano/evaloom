package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

var (
	taskIDPattern    = regexp.MustCompile(`^task-[0-9]+$`)
	shortNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	commitPattern    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	// ErrWorktreePath identifies a path safety rejection for the Runner's audit
	// boundary. Error text contains neither paths nor repository credentials.
	ErrWorktreePath = errors.New("worktree: unsafe worktree path")
)

// CreateRequest uses the previously fetched and recorded BaseSHA. FreshNumber
// is zero for normal creation and the positive, task-wide fresh ordinal for a
// fresh execution (Q22's proposed naming). It is NOT executions.seq, which also
// counts continue executions. Allocation belongs to the calling stage.
type CreateRequest struct {
	ProjectID, RepositoryURL, TaskID, ShortName, BaseSHA string
	FreshNumber                                          int
}

// Created is the filesystem result for subsequent copy/persistence stages.
// It is not a worktrees row or a worktree_ready event: those follow copying.
type Created struct {
	Path, BranchName, BaseSHA string
}

// Create adds a new branch and linked worktree to an already prepared clone.
// Callers must stop this Project's sandbox writers through the entire call.
// Existing branches and paths are never reused, reset, removed or overwritten.
func (r Repositories) Create(ctx context.Context, request CreateRequest) (Created, error) {
	if err := ctx.Err(); err != nil {
		return Created{}, err
	}
	if !projectSlug.MatchString(request.ProjectID) || !networkURL(request.RepositoryURL) ||
		!filepath.IsAbs(r.WorktreeRoot) || !taskIDPattern.MatchString(request.TaskID) ||
		!shortNamePattern.MatchString(request.ShortName) || !commitPattern.MatchString(request.BaseSHA) || request.FreshNumber < 0 {
		return Created{}, fmt.Errorf("worktree: invalid creation configuration")
	}
	suffix := ""
	if request.FreshNumber > 0 {
		suffix = "-r" + strconv.Itoa(request.FreshNumber)
	}
	branch := "evaloom/" + request.TaskID + "-" + request.ShortName + suffix
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil {
		return Created{}, ErrWorktreePath
	}
	storage := filepath.Join(root, repositoryDirectory)
	if err := containedDirectory(root, storage); err != nil {
		return Created{}, err
	}
	unlock, err := lockRepository(ctx, filepath.Join(storage, "."+request.ProjectID+".lock"))
	if err != nil {
		return Created{}, err
	}
	defer unlock()
	repository := filepath.Join(storage, request.ProjectID)
	if err := validateClone(ctx, root, repository, request.RepositoryURL); err != nil {
		return Created{}, err
	}
	path := filepath.Join(root, request.TaskID+suffix)
	if _, err := os.Lstat(path); err == nil {
		if err := containedDirectory(root, path); err != nil {
			return Created{}, ErrWorktreePath
		}
		return Created{}, fmt.Errorf("worktree: creation path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Created{}, ErrWorktreePath
	}
	ref := "refs/heads/" + branch
	if _, err := safegit.Output(ctx, repository, "check-ref-format", ref); err != nil {
		return Created{}, operationError(ctx, "validate branch name")
	}
	local, err := safegit.Output(ctx, repository, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return Created{}, operationError(ctx, "inspect local branch")
	}
	for _, name := range strings.Split(string(local), "\n") {
		if name == ref {
			return Created{}, fmt.Errorf("worktree: branch already exists")
		}
	}
	transport := r.Transport
	if transport == nil {
		transport = safeTransport{}
	}
	// Inspect the live remote, not stale tracking refs. A branch may have been
	// created after Prepare/FetchBase. No refspec or local ref is updated here.
	remote, err := transport.Output(ctx, repository, "ls-remote", "--heads", "--refs", "--", request.RepositoryURL, ref)
	if err != nil {
		return Created{}, operationError(ctx, "inspect remote branch")
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(remote), "\n"), "\n") {
		if line == "" {
			continue
		}
		sha, name, ok := strings.Cut(line, "\t")
		if !ok || !commitPattern.MatchString(sha) {
			return Created{}, fmt.Errorf("worktree: invalid remote branch response")
		}
		if name == ref {
			return Created{}, fmt.Errorf("worktree: branch already exists")
		}
	}
	kind, err := safegit.Output(ctx, repository, "cat-file", "-t", request.BaseSHA)
	if err != nil || string(kind) != "commit\n" {
		return Created{}, operationError(ctx, "validate start commit")
	}
	// Reserve our own empty directory exclusively, including across Projects.
	// Never allow Git to populate an existing empty directory or follow a link.
	if err := os.Mkdir(path, 0700); err != nil {
		return Created{}, ErrWorktreePath
	}
	if err := containedDirectory(root, path); err != nil {
		return Created{}, ErrWorktreePath
	}
	if _, err := safegit.Output(ctx, repository, "worktree", "add", "--no-track", "-b", branch, "--", path, request.BaseSHA); err != nil {
		return Created{}, operationError(ctx, "create worktree")
	}
	if err := containedDirectory(root, path); err != nil {
		return Created{}, ErrWorktreePath
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Created{}, ErrWorktreePath
	}
	return Created{Path: real, BranchName: branch, BaseSHA: request.BaseSHA}, nil
}
