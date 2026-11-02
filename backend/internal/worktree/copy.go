package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"golang.org/x/sys/unix"
)

// CopyFiles is the new-worktree copy stage (spec 06, 3.5). Missing sources are
// reported synchronously before proceeding; the callback must emit a log warning.
// Sandbox writers must remain stopped until copying and recording have finished.
// Existing destination entries are retained and rejected, never overwritten.
func (r Repositories) CopyFiles(ctx context.Context, source, destination string, allowlist []string, warnMissing func(context.Context, string) error) ([]model.CopiedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) || !filepath.IsAbs(r.WorktreeRoot) || warnMissing == nil {
		return nil, fmt.Errorf("worktree: invalid copy configuration")
	}
	// Validate the complete list before creating any directories or files.
	paths := make([]string, 0, len(allowlist))
	seen := make(map[string]bool)
	for _, name := range allowlist {
		if !copyPath(name) {
			return nil, fmt.Errorf("worktree: invalid copy path")
		}
		name = filepath.Clean(name)
		if !seen[name] {
			paths = append(paths, name)
			seen[name] = true
		}
	}
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil || containedDirectory(root, destination) != nil {
		return nil, ErrWorktreePath
	}
	// Registry paths can have symlinked ancestors (e.g. /var on macOS). Resolve
	// each root once, then use O_NOFOLLOW on EVERY repository-relative component.
	realSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve copy source")
	}
	src, err := openCopyDirectory(realSource)
	if err != nil {
		return nil, fmt.Errorf("worktree: open copy source")
	}
	defer src.Close()
	dst, err := openCopyDirectory(destination)
	if err != nil {
		return nil, ErrWorktreePath
	}
	defer dst.Close()
	copied := make([]model.CopiedFile, 0, len(paths))
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		digest, err := copyFile(ctx, src, dst, name)
		if errors.Is(err, os.ErrNotExist) {
			if err := warnMissing(ctx, filepath.ToSlash(name)); err != nil {
				return nil, fmt.Errorf("worktree: missing copy warning failed")
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("worktree: copy failed")
		}
		if digest != "" {
			copied = append(copied, model.CopiedFile{Path: filepath.ToSlash(name), SHA256: digest})
		}
	}
	return copied, nil
}

func copyPath(name string) bool {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) == "." || strings.ContainsAny(name, "\\\x00\r\n*?[]") {
		return false
	}
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		// Never replace the linked-worktree .git file or write into a Git area.
		if part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func openCopyDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// copyParent pins every directory with its own descriptor. Neither an internal
// nor an external symlink can be followed, even if an entry changes after Lstat.
// ENOTDIR/ELOOP on the source are skips; on the destination they are errors.
func copyParent(root *os.File, name string, create bool) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), ".")
	for _, part := range strings.Split(filepath.Dir(name), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		if create {
			if err := unix.Mkdirat(int(current.Fd()), part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				current.Close()
				return nil, err
			}
		}
		next, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = os.NewFile(uintptr(next), part)
	}
	return current, nil
}

func copyFile(ctx context.Context, src, dst *os.File, name string) (string, error) {
	parent, err := copyParent(src, name, false)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer parent.Close()
	leaf := filepath.Base(name)
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", nil
	}
	fd, err := unix.Openat(int(parent.Fd()), leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	input := os.NewFile(uintptr(fd), leaf)
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	targetParent, err := copyParent(dst, name, true)
	if err != nil {
		// A missing destination must never be mistaken for a missing source.
		return "", fmt.Errorf("worktree: create copy parent")
	}
	defer targetParent.Close()
	fd, err = unix.Openat(int(targetParent.Fd()), leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(info.Mode().Perm()))
	if err != nil {
		return "", fmt.Errorf("worktree: create copy destination")
	}
	output := os.NewFile(uintptr(fd), leaf)
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), copyReader{ctx, input})
	err = errors.Join(copyErr, output.Close())
	if err != nil {
		return "", fmt.Errorf("worktree: write copy destination")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type copyReader struct {
	ctx context.Context
	io.Reader
}

func (r copyReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
