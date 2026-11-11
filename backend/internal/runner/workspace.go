package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errWorkspaceMissing = errors.New("runner: inherited workspace is missing")

// validateWorkspacePath also resolves existing ancestors of a missing workspace.
// Never create a directory until symlinks and the storage boundaries are checked.
func (runner *Runner) validateWorkspacePath(claim Claim) error {
	id, path := claim.Workspace.ID, claim.Workspace.Path
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") ||
		!filepath.IsAbs(runner.workspaceRoot) || !filepath.IsAbs(path) ||
		filepath.Clean(path) != filepath.Join(runner.workspaceRoot, id) ||
		!configPathWithin(runner.workspaceRoot, path) {
		return fmt.Errorf("runner: workspace must resolve inside workspaceRoot")
	}
	root, err := resolveConfigPath(runner.workspaceRoot)
	data, dataErr := resolveConfigPath(runner.dataDir)
	repo, repoErr := resolveConfigPath(claim.Profile.RepoPath)
	if err != nil || dataErr != nil || repoErr != nil || configPathsOverlap(root, data) ||
		root == repo || configPathWithin(repo, root) {
		return fmt.Errorf("runner: workspaceRoot overlaps Runner data or is inside the Profile repository")
	}
	return nil
}

func (runner *Runner) prepareWorkspace(claim Claim) error {
	if err := runner.validateWorkspacePath(claim); err != nil {
		return err
	}
	if claim.Workspace.Created {
		if err := os.MkdirAll(runner.workspaceRoot, 0700); err != nil {
			return err
		}
		root, err := os.OpenRoot(runner.workspaceRoot)
		if err != nil {
			return err
		}
		defer root.Close()
		// Mkdir, rather than MkdirAll, refuses any existing workspace. A fresh
		// claim must start empty and must never overwrite retained contents.
		if err := root.Mkdir(claim.Workspace.ID, 0700); err != nil {
			return err
		}
		if err := root.Chmod(claim.Workspace.ID, 0700); err != nil {
			return err
		}
	}
	return runner.checkWorkspace(claim)
}

// Check before every stage, including the first stage after creation. Earlier
// stages may have removed the directory or replaced it with an escaping symlink.
func (runner *Runner) checkWorkspace(claim Claim) error {
	if err := runner.validateWorkspacePath(claim); err != nil {
		return err
	}
	info, err := os.Stat(claim.Workspace.Path)
	if errors.Is(err, os.ErrNotExist) {
		return errWorkspaceMissing
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("runner: workspace must be a directory")
	}
	return nil
}
