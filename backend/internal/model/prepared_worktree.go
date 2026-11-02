package model

// PreparedWorktree is the internal copy/persistence boundary for spec 06, 3.5.
// This is not a new HTTP payload. Wire delivery is not specified yet.
type PreparedWorktree struct {
	Path, BranchName, BaseSHA string
	CopiedFiles               []CopiedFile
}
