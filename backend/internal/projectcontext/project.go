// Package projectcontext reads project instructions pinned to a worktree's base
// commit. It does not read instruction contents from the working tree.
package projectcontext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"
)

// File is a project-layer snapshot ready for the Runner's context submission.
// ContentHash is the lowercase hexadecimal SHA-256 of the unmodified content.
type File struct {
	Path            string
	Content         string
	ContentHash     string
	SourceCommitSHA string
}

// Read returns all instruction files from baseSHA, sorted by repository-relative
// path. baseSHA must be a full commit SHA (SHA-1 or SHA-256), never a moving ref.
// Targets at every directory level are AGENTS.md, CLAUDE.md, .mcp.json, and all
// tracked files beneath .claude/ or .codex/. Selected symlinks are rejected: their
// target contents cannot be pinned by reading the link's blob. Submodules are
// not traversed. On any failure, no partial snapshot is returned.
func Read(ctx context.Context, worktree, baseSHA string) ([]File, error) {
	if len(baseSHA) != 40 && len(baseSHA) != 64 {
		return nil, fmt.Errorf("project context: base SHA must be a full commit SHA")
	}
	if _, err := hex.DecodeString(baseSHA); err != nil {
		return nil, fmt.Errorf("project context: invalid base SHA: %w", err)
	}
	commit, err := git(ctx, worktree, "rev-parse", "--verify", "--end-of-options", baseSHA+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("project context: resolve base commit: %w", err)
	}
	sourceSHA := strings.TrimSpace(string(commit))
	tree, err := git(ctx, worktree, "ls-tree", "-r", "-z", "--full-tree", sourceSHA)
	if err != nil {
		return nil, fmt.Errorf("project context: list base tree: %w", err)
	}

	files := make([]File, 0)
	for _, entry := range bytes.Split(tree, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("project context: malformed git tree entry %q", entry)
		}
		filePath := string(name)
		if !isInstruction(filePath) {
			continue
		}
		if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("project context: %q is not a regular file in base commit (mode %s)", filePath, fields[0])
		}
		// Use the blob ID from the pinned tree. cat-file reads exact bytes, with
		// no checkout filters, newline conversion, or path quoting involved.
		content, err := git(ctx, worktree, "cat-file", "blob", fields[2])
		if err != nil {
			return nil, fmt.Errorf("project context: read %q: %w", filePath, err)
		}
		hash := sha256.Sum256(content)
		files = append(files, File{
			Path:            filePath,
			Content:         string(content),
			ContentHash:     hex.EncodeToString(hash[:]),
			SourceCommitSHA: sourceSHA,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func isInstruction(filePath string) bool {
	switch path.Base(filePath) {
	case "AGENTS.md", "CLAUDE.md", ".mcp.json":
		return true
	}
	parts := strings.Split(filePath, "/")
	for _, directory := range parts[:len(parts)-1] {
		if directory == ".claude" || directory == ".codex" {
			return true
		}
	}
	return false
}

func git(ctx context.Context, worktree string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", worktree}, args...)...)
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
