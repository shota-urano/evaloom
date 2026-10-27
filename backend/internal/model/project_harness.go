package model

// ProjectHarness pins one registered Profile to a Project role.
type ProjectHarness struct {
	ProjectID  string `json:"projectId"`
	Role       string `json:"role"`
	HarnessID  string `json:"harnessId"`
	ProfileID  string `json:"profileId"`
	AssignedAt string `json:"assignedAt"`
}
