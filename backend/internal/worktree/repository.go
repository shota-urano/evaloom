// Package worktree prepares Runner-owned repositories for task worktrees.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

const repositoryDirectory = ".evaloom-repositories"

var projectSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ReadTransport is the Trusted Runner's read-only network Git boundary. A nil
// transport uses safegit.Output without additional credentials. Implementations
// must retain safegit's protections and supply credentials outside repositories,
// without enabling credential helpers or persisting secrets in Git config/URLs.
// The specification has not yet selected a credential delivery mechanism.
type ReadTransport interface {
	Output(ctx context.Context, directory string, args ...string) ([]byte, error)
}

type safeTransport struct{}

func (safeTransport) Output(ctx context.Context, directory string, args ...string) ([]byte, error) {
	return safegit.Output(ctx, directory, args...)
}

// Repositories prepares one independent clone per Project below WorktreeRoot.
// Callers must stop sandbox writers for this Project before Prepare and before
// subsequent host Git operations. The filesystem lock serializes preparers; it
// does not stop a harness from writing through an existing linked worktree.
type Repositories struct {
	WorktreeRoot string
	Transport    ReadTransport
}

// Prepare returns an absolute, symlink-resolved clone path for later worktree
// creation. Only the Project ID and Git Repository URL are accepted: the owner's
// Local Repository Path is deliberately not an input. It does not select a Base
// Branch, record a base SHA, create a task worktree or check out project files.
// Existing invalid clones are retained and rejected, never replaced silently.
func (r Repositories) Prepare(ctx context.Context, projectID, repositoryURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !projectSlug.MatchString(projectID) {
		return "", fmt.Errorf("worktree: invalid project ID")
	}
	if !networkURL(repositoryURL) {
		return "", fmt.Errorf("worktree: repository URL must use a network transport without embedded secrets")
	}
	if !filepath.IsAbs(r.WorktreeRoot) {
		return "", fmt.Errorf("worktree: Worktree Root must be absolute")
	}
	if err := os.MkdirAll(r.WorktreeRoot, 0700); err != nil {
		return "", fmt.Errorf("worktree: create Worktree Root")
	}
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil {
		return "", fmt.Errorf("worktree: resolve Worktree Root")
	}
	storage := filepath.Join(root, repositoryDirectory)
	if err := os.Mkdir(storage, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("worktree: create repository storage")
	}
	if err := containedDirectory(root, storage); err != nil {
		return "", err
	}
	unlock, err := lockRepository(ctx, filepath.Join(storage, "."+projectID+".lock"))
	if err != nil {
		return "", err
	}
	defer unlock()

	transport := r.Transport
	if transport == nil {
		transport = safeTransport{}
	}
	repository := filepath.Join(storage, projectID)
	if _, err := os.Lstat(repository); err == nil {
		if err := validateClone(ctx, root, repository, repositoryURL); err != nil {
			return "", err
		}
		// validateClone rejects URL aliases/rewrites before dispatch. A URL
		// argument alone is insufficient: Git can treat it as a remote name.
		// Keep the refspec explicit so config cannot redirect updates to heads.
		_, err = transport.Output(ctx, repository, "fetch", "--no-tags", "--refmap=", "--", repositoryURL,
			"+refs/heads/*:refs/remotes/origin/*")
		if err != nil {
			return "", operationError(ctx, "fetch")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// Clone into a private staging directory. A failed/cancelled clone must
		// not leave a partial repository at the reusable Project path.
		staging, err := os.MkdirTemp(storage, ".clone-"+projectID+"-")
		if err != nil {
			return "", fmt.Errorf("worktree: create clone staging directory")
		}
		defer os.RemoveAll(staging)
		candidate := filepath.Join(staging, "repository")
		_, err = transport.Output(ctx, staging, "clone", "--no-checkout", "--no-local", "--origin", "origin", "--", repositoryURL, candidate)
		if err != nil {
			return "", operationError(ctx, "clone")
		}
		if err := validateClone(ctx, root, candidate, repositoryURL); err != nil {
			return "", err
		}
		if err := os.Rename(candidate, repository); err != nil {
			return "", fmt.Errorf("worktree: publish prepared clone")
		}
	} else {
		return "", fmt.Errorf("worktree: inspect clone path")
	}
	if err := validateClone(ctx, root, repository, repositoryURL); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(repository)
}

// Filesystem paths, file:// and remote helpers must never turn the owner's local
// repository into a clone source. SSH's scp-like syntax is also a network URL.
func networkURL(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") || strings.HasPrefix(value, "-") {
		return false
	}
	if !strings.Contains(value, "://") {
		host, path, ok := strings.Cut(value, ":")
		return ok && host != "" && path != "" && !strings.HasPrefix(path, ":") &&
			!strings.ContainsAny(host, `/\\ `) && host != "file" && host != "ext"
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "http", "https", "git":
		return u.User == nil
	case "ssh":
		if u.User != nil {
			_, password := u.User.Password()
			return !password
		}
		return true
	}
	return false
}

func containedDirectory(root, path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("worktree: repository directories must be real directories")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("worktree: resolve repository path")
	}
	relative, err := filepath.Rel(root, real)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("worktree: repository path escapes Worktree Root")
	}
	return nil
}

func validateClone(ctx context.Context, root, repository, repositoryURL string) error {
	// A .git file/symlink could redirect operations into the owner's Git area.
	for _, directory := range []string{repository, filepath.Join(repository, ".git"),
		filepath.Join(repository, ".git", "objects"), filepath.Join(repository, ".git", "refs")} {
		if err := containedDirectory(root, directory); err != nil {
			return err
		}
	}
	// Nested links (for example refs/remotes/origin or an object shard) can
	// redirect fetch writes even when the clone and .git themselves are inside
	// the root. Git-created standalone clones need no symlinks in their Git area.
	if err := filepath.WalkDir(filepath.Join(repository, ".git"), func(_ string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("worktree: clone Git area must not contain symlinks or unreadable paths")
		}
		return nil
	}); err != nil {
		return err
	}
	for _, name := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(repository, ".git", "objects", "info", name)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("worktree: shared object stores are not allowed")
		}
	}
	// Git also accepts legacy remote definitions outside config, including
	// scp-like URL names. Our standalone clones never need these directories.
	for _, name := range []string{"remotes", "branches"} {
		if _, err := os.Lstat(filepath.Join(repository, ".git", name)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("worktree: legacy remote definitions are not allowed")
		}
	}
	if err := safegit.ValidateFetchURL(ctx, repository, repositoryURL); err != nil {
		return operationError(ctx, "validate fetch destination")
	}
	for _, query := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--absolute-git-dir"}, filepath.Join(repository, ".git")},
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, filepath.Join(repository, ".git")},
		{[]string{"remote", "get-url", "--", "origin"}, repositoryURL},
	} {
		out, err := safegit.Output(ctx, repository, query.args...)
		if err != nil {
			return operationError(ctx, "validate clone")
		}
		if strings.TrimSuffix(string(out), "\n") != query.want {
			return fmt.Errorf("worktree: clone Git directory or origin differs from registered configuration")
		}
	}
	return nil
}

func operationError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Git/transport errors and stderr can contain credentials. Never wrap them.
	return fmt.Errorf("worktree: %s failed", operation)
}

func lockRepository(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("worktree: open repository lock")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("worktree: invalid repository lock")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("worktree: lock repository")
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
