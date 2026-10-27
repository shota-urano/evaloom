// Package harnesscontext reads selected harness files at the Profile's pinned
// commit. It never resolves tags or reads working-tree file contents.
package harnesscontext

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type File = gitcontext.File

// ConfigError tells the Runner to report failed(config_error), per spec 07 §6.
// The reader does not persist lifecycle changes. Unwrap preserves cancellation
// and deadline errors so the caller can apply its cancellation policy first.
type ConfigError struct {
	Err error
}

func (e *ConfigError) Error() string               { return "harness context: " + e.Err.Error() }
func (e *ConfigError) Unwrap() error               { return e.Err }
func (*ConfigError) Status() model.ExecutionStatus { return model.ExecutionFailed }
func (*ConfigError) ErrorClass() model.ErrorClass  { return model.ErrorConfig }

// Read reads exactly the repository-relative paths selected for the pinned
// Profile. The caller supplies the Execution's saved commit SHA and an explicit
// file list; this reader does not infer paths from version labels. Paths name
// individual files, not directories or glob expressions. Duplicate paths yield
// one snapshot, sorted by path. Empty selection still validates the commit.
// Missing files, invalid pins, non-regular files and Git failures return a
// ConfigError and no partial snapshot. No checkout or file writes occur.
func Read(ctx context.Context, repo, commitSHA string, paths []string) ([]File, error) {
	selected := make(map[string]bool, len(paths))
	for _, name := range paths {
		if name == "" || name == "." || name == ".." || path.IsAbs(name) || path.Clean(name) != name ||
			strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\x00\\") {
			return nil, &ConfigError{Err: fmt.Errorf("invalid repository-relative file path %q", name)}
		}
		selected[name] = true
	}
	files, err := gitcontext.Read(ctx, repo, commitSHA, func(name string) bool { return selected[name] })
	if err != nil {
		return nil, &ConfigError{Err: err}
	}
	found := make(map[string]bool, len(files))
	for _, file := range files {
		found[file.Path] = true
	}
	for _, name := range paths {
		if !found[name] {
			return nil, &ConfigError{Err: fmt.Errorf("file %q is missing from commit %s", name, commitSHA)}
		}
	}
	return files, nil
}
