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
// file or directory list from stages.work; this reader does not infer paths
// from version labels. Directories include all descendants, while globs are
// literal names. Overlapping selections yield one snapshot per file, sorted by
// path. Empty selection still validates the commit.
// Missing files, invalid pins, non-regular files and Git failures return a
// ConfigError and no partial snapshot. No checkout or file writes occur.
func Read(ctx context.Context, repo, commitSHA string, paths []string) ([]File, error) {
	selected := make(map[string]bool, len(paths))
	for _, name := range paths {
		if name == "" || path.Clean(name) == "." || path.IsAbs(name) ||
			strings.ContainsAny(name, "\x00\\") {
			return nil, &ConfigError{Err: fmt.Errorf("invalid repository-relative file path %q", name)}
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return nil, &ConfigError{Err: fmt.Errorf("invalid repository-relative file path %q", name)}
			}
		}
		selected[path.Clean(name)] = false
	}
	files, err := gitcontext.Read(ctx, repo, commitSHA, func(name string) bool {
		include := false
		for selection := range selected {
			if name == selection || strings.HasPrefix(name, selection+"/") {
				selected[selection], include = true, true
			}
		}
		return include
	})
	if err != nil {
		return nil, &ConfigError{Err: err}
	}
	for name, found := range selected {
		if !found {
			return nil, &ConfigError{Err: fmt.Errorf("file %q is missing from commit %s", name, commitSHA)}
		}
	}
	return files, nil
}
