package model

// WorktreeCleanupTarget is an internal Control Plane/Runner boundary, not a
// wire contract. PRNumber belongs to this worktree's branch, including when a
// later retry has not reported a PR of its own.
type WorktreeCleanupTarget struct {
	ProjectID, RepositoryURL, Path, BranchName, ExecutionID string
	PRNumber                                                int
}
