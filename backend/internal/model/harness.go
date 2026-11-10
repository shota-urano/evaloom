package model

// HarnessSettings stores repository identity and the direct-routing defaults.
// Adapter, tools and models belong to the immutable Profile definition.
type HarnessSettings struct {
	Name             string   `json:"name"`
	RepoPath         string   `json:"repoPath"`
	RepoURL          string   `json:"repoUrl"`
	DefaultProfileID *string  `json:"defaultProfileId"`
	ApprovalWaivers  []string `json:"approvalWaivers"`
}

type Harness struct {
	ID string `json:"id"`
	HarnessSettings
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
