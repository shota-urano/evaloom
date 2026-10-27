// Package gitcontext reads exact file bytes from an immutable Git commit.
package gitcontext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// File is a context snapshot shared by the project and harness readers.
// ContentHash is the lowercase SHA-256 of the unmodified Git blob bytes.
type File struct {
	Path            string `json:"path"`
	Content         string `json:"content"`
	ContentHash     string `json:"contentHash"`
	SourceCommitSHA string `json:"sourceCommitSha"`
}

// Read returns the regular files selected by include, sorted by relative path.
// commitSHA must be a full SHA-1 or SHA-256 commit ID, never a moving ref.
// Submodules are not traversed. Any failure discards the entire snapshot.
func Read(ctx context.Context, repo, commitSHA string, include func(string) bool) ([]File, error) {
	if len(commitSHA) != 40 && len(commitSHA) != 64 {
		return nil, fmt.Errorf("SHA must be a full commit SHA")
	}
	if _, err := hex.DecodeString(commitSHA); err != nil {
		return nil, fmt.Errorf("invalid commit SHA: %w", err)
	}
	commit, err := git(ctx, repo, "rev-parse", "--verify", "--end-of-options", commitSHA+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("resolve commit: %w", err)
	}
	sourceSHA := strings.TrimSpace(string(commit))
	// A full object ID can name an annotated tag object too. Only a commit ID
	// is a valid pin; do not silently peel another object into a different ID.
	if !strings.EqualFold(sourceSHA, commitSHA) {
		return nil, fmt.Errorf("SHA does not identify a commit")
	}
	tree, err := git(ctx, repo, "ls-tree", "-r", "-z", "--full-tree", sourceSHA)
	if err != nil {
		return nil, fmt.Errorf("list commit tree: %w", err)
	}

	files := make([]File, 0)
	for _, entry := range bytes.Split(tree, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("malformed git tree entry %q", entry)
		}
		filePath := string(name)
		if !include(filePath) {
			continue
		}
		if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("%q is not a regular file in commit (mode %s)", filePath, fields[0])
		}
		// Reading the pinned blob avoids checkout filters, newline conversion,
		// symlink traversal and path quoting or pathspec interpretation.
		content, err := git(ctx, repo, "cat-file", "blob", fields[2])
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", filePath, err)
		}
		hash := sha256.Sum256(content)
		files = append(files, File{
			Path: filePath, Content: string(content),
			ContentHash: hex.EncodeToString(hash[:]), SourceCommitSHA: sourceSHA,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	gitArgs := []string{"--no-replace-objects", "--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", repo}
	cmd := exec.CommandContext(ctx, "git", append(gitArgs, args...)...)
	// Follow Registry's practice of ignoring ambient repository overrides.
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
