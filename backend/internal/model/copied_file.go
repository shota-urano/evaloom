package model

// CopiedFile is an entry in worktrees.copied_files (spec 06, 3.5).
type CopiedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
