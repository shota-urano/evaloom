// Package projectcontext reads project instructions pinned to a worktree's base
// commit. It does not read instruction contents from the working tree.
package projectcontext

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
)

// File is a project-layer snapshot ready for the Runner's context submission.
// ContentHash is the lowercase hexadecimal SHA-256 of the unmodified content.
type File = gitcontext.File

// Read returns all instruction files from baseSHA, sorted by repository-relative
// path. baseSHA must be a full commit SHA (SHA-1 or SHA-256), never a moving ref.
// Targets at every directory level are AGENTS.md, CLAUDE.md, .mcp.json, and all
// tracked files beneath .claude/ or .codex/. Selected symlinks are rejected: their
// target contents cannot be pinned by reading the link's blob. Submodules are
// not traversed. On any failure, no partial snapshot is returned.
func Read(ctx context.Context, worktree, baseSHA string) ([]File, error) {
	files, err := gitcontext.Read(ctx, worktree, baseSHA, isInstruction)
	if err != nil {
		return nil, fmt.Errorf("project context: %w", err)
	}
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
