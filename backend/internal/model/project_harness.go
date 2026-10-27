package model

// ProjectHarness pins one registered Profile to a Project role.
type ProjectHarness struct {
	ProjectID  string `json:"projectId"`
	Role       string `json:"role"`
	HarnessID  string `json:"harnessId"`
	ProfileID  string `json:"profileId"`
	AssignedAt string `json:"assignedAt"`
}

// ProjectHarnessView is a role's pin as shown to the owner: the assignment with
// the pinned Profile's version and commit SHA.
type ProjectHarnessView struct {
	ProjectHarness
	Version   string `json:"version"`
	CommitSHA string `json:"commitSha"`
}
