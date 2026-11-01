package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func baselineReportPath(request BaselineTestRequest, format string) (string, error) {
	if request.ReportFormat == nil || *request.ReportFormat != format || request.ReportPath == nil {
		return "", fmt.Errorf("%w: configured JUnit report format and path are required", ErrBaselineUnverifiable)
	}
	path := *request.ReportPath
	if !filepath.IsLocal(path) || filepath.Clean(path) == "." || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: report must be inside the worktree", ErrBaselineUnverifiable)
	}
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("%w: report must not reference Git metadata", ErrBaselineUnverifiable)
		}
	}
	return path, nil
}

// Do not follow a report directory symlink even within the worktree: it could
// redirect cleanup into .git or turn an unrelated file into a fresh report.
func baselineReportParents(root *os.Root, path string) error {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := 1; i < len(parts); i++ {
		info, err := root.Lstat(filepath.Join(parts[:i]...))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !info.IsDir() {
			return fmt.Errorf("runner: baseline report directories must not be symlinks")
		}
	}
	return nil
}

func newPreparedBaselineSandbox(config BaselineConfig) SandboxBaseline {
	return SandboxBaseline{
		Sandbox: config.Sandbox,
		Prepare: func(ctx context.Context, request BaselineTestRequest) (sandbox.SandboxSpec, []string, error) {
			if err := ctx.Err(); err != nil {
				return sandbox.SandboxSpec{}, nil, err
			}
			path, err := baselineReportPath(request, config.JUnitFormat)
			if err != nil {
				return sandbox.SandboxSpec{}, nil, err
			}
			// Q39 also applies when a fresh report cannot be prepared, before
			// any test process starts. Keep these failures identifiable so the
			// controlled handler persists config_error and requests notification.
			root, err := os.OpenRoot(request.WorktreePath)
			if err != nil {
				return sandbox.SandboxSpec{}, nil, fmt.Errorf("%w: cannot open baseline report worktree: %w", ErrBaselineUnverifiable, err)
			}
			defer root.Close()
			if err := baselineReportParents(root, path); err != nil {
				return sandbox.SandboxSpec{}, nil, fmt.Errorf("%w: %w", ErrBaselineUnverifiable, err)
			}
			info, err := root.Lstat(path)
			if err != nil && !os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				return sandbox.SandboxSpec{}, nil, fmt.Errorf("%w: previous baseline report is not a file", ErrBaselineUnverifiable)
			}
			// Remove only the configured previous report, never its target if it
			// is a symlink. Root rejects parent paths escaping the worktree.
			if err := root.Remove(path); err != nil && !os.IsNotExist(err) {
				return sandbox.SandboxSpec{}, nil, fmt.Errorf("%w: cannot clear previous baseline report", ErrBaselineUnverifiable)
			}
			spec := sandbox.SandboxSpec{Kind: sandbox.TestRun, Worktree: request.WorktreePath,
				Network: sandbox.NetworkNone, Limits: config.Limits}
			// The exact Registry string is one shell argument. There is no host
			// shell, interpolation, environment inheritance or Git mount here.
			return spec, []string{config.Shell, "-c", request.Command}, nil
		},
		OpenReport: func(ctx context.Context, request BaselineTestRequest) (io.ReadCloser, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path, err := baselineReportPath(request, config.JUnitFormat)
			if err != nil {
				return nil, err
			}
			root, err := os.OpenRoot(request.WorktreePath)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			if err := baselineReportParents(root, path); err != nil {
				return nil, err
			}
			info, err := root.Lstat(path)
			if os.IsNotExist(err) {
				return nil, nil
			}
			if err != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("runner: baseline report is not a regular file")
			}
			// Nonblocking open plus a regular-file check rejects FIFOs/devices
			// without letting a report keep the trusted Runner blocked forever.
			file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if os.IsNotExist(err) {
				return nil, nil
			}
			if err != nil {
				return nil, fmt.Errorf("runner: cannot open fresh baseline report")
			}
			info, err = file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				file.Close()
				return nil, fmt.Errorf("runner: baseline report is not a regular file")
			}
			return file, nil
		},
	}
}
