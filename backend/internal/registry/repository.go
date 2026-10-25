// Package registry validates local repositories before registration or use.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepositoryInput describes a repository on the local Runner's machine.
type RepositoryInput struct {
	Path         string
	URL          string
	Remote       string // Defaults to origin.
	BaseBranch   string // Empty for a Harness, which has no Base Branch.
	WorktreeRoot string
}

// Detail identifies the failed check using the API's field/reason format.
type Detail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// PreconditionError can be used as the error body of a 422 API response.
// No Git output is included: it may contain credentials in remote URLs.
type PreconditionError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []Detail `json:"details"`
}

func (e *PreconditionError) Error() string { return e.Message }

func (*PreconditionError) StatusCode() int { return 422 }

func rejected(field, reason string) error {
	return &PreconditionError{
		Code:    "precondition_failed",
		Message: fmt.Sprintf("repository validation failed: %s (%s)", field, reason),
		Details: []Detail{{Field: field, Reason: reason}},
	}
}

// ValidateRepository returns the absolute, symlink-resolved path to persist.
// It returns an empty path on every failure. Phase 1 runs these checks directly
// in the Control Plane, under the same OS user as the local Runner. A remote
// Runner must execute this function on its own machine under its execution user.
// URL comparison is exact; transport aliases and rewritten URLs are not treated
// as equivalent. Base Branch is checked against the remote, not cached refs.
// The caller must enforce Project path uniqueness transactionally when saving.
func ValidateRepository(ctx context.Context, input RepositoryInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(input.Path) {
		return "", rejected("path", "not_absolute")
	}
	realPath, err := filepath.EvalSymlinks(input.Path)
	if err != nil {
		return "", pathError(err)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", pathError(err)
	}
	if !info.IsDir() {
		return "", rejected("path", "not_directory")
	}
	// Opening and reading the directory tests the process's actual permissions,
	// including ACLs, rather than assuming that Unix mode bits tell the story.
	dir, err := os.Open(realPath)
	if err != nil {
		return "", rejected("path", "not_readable")
	}
	_, readErr := dir.Readdirnames(1)
	closeErr := dir.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil {
		return "", rejected("path", "not_readable")
	}

	if !filepath.IsAbs(input.WorktreeRoot) {
		return "", rejected("worktreeRoot", "not_absolute")
	}
	root, err := resolveWorktreeRoot(input.WorktreeRoot)
	if err != nil {
		return "", rejected("worktreeRoot", "not_resolvable")
	}
	relative, err := filepath.Rel(root, realPath)
	if err != nil {
		return "", rejected("worktreeRoot", "not_resolvable")
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", rejected("path", "within_worktree_root")
	}

	top, err := repositoryGit(ctx, realPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", gitError(ctx, "path", "not_git_repository")
	}
	// Remove only Git's terminating newline; spaces and newlines may be part
	// of the directory name itself.
	topPath, err := filepath.EvalSymlinks(strings.TrimSuffix(top, "\n"))
	if err != nil || topPath != realPath {
		return "", rejected("path", "not_repository_root")
	}
	remote := input.Remote
	if remote == "" {
		remote = "origin"
	}
	url, err := repositoryGit(ctx, realPath, "remote", "get-url", "--", remote)
	if err != nil {
		return "", gitError(ctx, "url", "remote_not_found")
	}
	if input.URL == "" || strings.TrimSuffix(url, "\n") != input.URL {
		return "", rejected("url", "remote_mismatch")
	}
	if input.BaseBranch != "" {
		ref := "refs/heads/" + input.BaseBranch
		if _, err := repositoryGit(ctx, realPath, "check-ref-format", ref); err != nil {
			return "", gitError(ctx, "baseBranch", "invalid_branch")
		}
		refs, err := repositoryGit(ctx, realPath, "ls-remote", "--exit-code", "--heads", "--refs", "--", remote, ref)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
				return "", rejected("baseBranch", "not_found")
			}
			return "", rejected("baseBranch", "remote_unavailable")
		}
		found := false
		for _, line := range strings.Split(refs, "\n") {
			_, name, ok := strings.Cut(line, "\t")
			if ok && name == ref {
				found = true
			}
		}
		if !found {
			return "", rejected("baseBranch", "not_found")
		}
	}
	return realPath, nil
}

func pathError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return rejected("path", "not_found")
	}
	if errors.Is(err, os.ErrPermission) {
		return rejected("path", "not_readable")
	}
	return rejected("path", "not_resolvable")
}

// The Runner need not have created its Worktree Root yet. Resolve the existing
// ancestors without creating anything, and reject dangling links or unreadable
// ancestors rather than guessing their targets.
func resolveWorktreeRoot(name string) (string, error) {
	// Lstat on a trailing slash follows a directory symlink, which would hide
	// a dangling link and incorrectly treat it as a not-yet-created directory.
	name = strings.TrimRight(name, string(filepath.Separator))
	if name == "" {
		name = string(filepath.Separator)
	}
	real, err := filepath.EvalSymlinks(name)
	if err == nil {
		info, err := os.Stat(real)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("worktree root is not a directory")
		}
		return real, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, linkErr := os.Lstat(name); linkErr == nil || !errors.Is(linkErr, os.ErrNotExist) {
		return "", err
	}
	parent, base := filepath.Split(strings.TrimRight(name, string(filepath.Separator)))
	if base == "" || parent == name {
		return "", err
	}
	realParent, err := resolveWorktreeRoot(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, base), nil
}

func gitError(ctx context.Context, field, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return rejected(field, reason)
}

// These checks only read Git state. Ignore inherited repository/config overrides
// so an ambient GIT_DIR or GIT_WORK_TREE cannot validate a different repository.
func repositoryGit(ctx context.Context, repo string, args ...string) (string, error) {
	gitArgs := []string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", repo}
	cmd := exec.CommandContext(ctx, "git", append(gitArgs, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	// Do not mistake a parent repository for the directory being registered.
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CEILING_DIRECTORIES="+filepath.Dir(repo))
	out, err := cmd.Output()
	return string(out), err
}
