package model

// WorktreeReuseState is an internal Control Plane read boundary. The claim
// schema does not carry the predecessor's last known SHA or worktree status.
type WorktreeReuseState struct {
	Status, ExpectedHeadSHA string
	PreparationComplete     bool
}
