package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

// CheckWorktree reads Git only after sandbox writers have stopped. A copy is
// exempt only while it is a regular file with its originally recorded bytes;
// staging, deletion and renaming are never exempted. Reports have no exemption.
func CheckWorktree(ctx context.Context, path string, copied []model.CopiedFile) (head string, clean bool, err error) {
	body, err := safegit.Output(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", false, err
	}
	head = strings.TrimSpace(string(body))
	body, err = safegit.Output(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return head, false, err
	}
	if len(body) == 0 {
		return head, true, nil
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return head, false, err
	}
	defer root.Close()
	allowed := make(map[string]string, len(copied))
	for _, file := range copied {
		// Invalid or ambiguous trusted metadata must never grant an exception.
		if _, exists := allowed[file.Path]; exists || !filepath.IsLocal(file.Path) || filepath.Clean(file.Path) != file.Path {
			return head, false, nil
		}
		allowed[file.Path] = file.SHA256
	}
	for _, entry := range bytes.Split(bytes.TrimSuffix(body, []byte{0}), []byte{0}) {
		if err := ctx.Err(); err != nil {
			return head, false, err
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return head, false, nil
		}
		status, name := string(entry[:2]), string(entry[3:])
		if status != "??" && status != " M" || allowed[name] == "" {
			return head, false, nil
		}
		if !unchangedCopy(ctx, root, name, allowed[name]) {
			return head, false, ctx.Err()
		}
	}
	return head, true, nil
}

func unchangedCopy(ctx context.Context, root *os.Root, path, digest string) bool {
	if !filepath.IsLocal(path) || filepath.Clean(path) != path {
		return false
	}
	parts := strings.Split(path, string(filepath.Separator))
	for i := range parts {
		if strings.EqualFold(parts[i], ".git") {
			return false
		}
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil || i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return false
		}
	}
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, file}); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == digest
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// CheckCommit binds Gate 3 to an actual, nonempty Gate 2 measurement. Skipped
// tests do not count as executed tests (spec 10, 3.4.3).
func CheckCommit(executionID, head, verified string, run *model.TestRun) bool {
	return verified != "" && head == verified && run != nil && run.ExecutionID == executionID &&
		run.Kind == "verify" && run.TargetSHA == verified && run.Total != nil && run.Skipped != nil &&
		*run.Skipped >= 0 && *run.Total > *run.Skipped
}
