package model

// HarnessSettings is the owner-editable Harness Registry configuration.
// Adapter is resolved through adapter.Registry, including registered extensions.
type HarnessSettings struct {
	Name            string         `json:"name"`
	Specialty       string         `json:"specialty"`
	SupportedTasks  []string       `json:"supportedTasks"`
	Adapter         string         `json:"adapter"`
	RepoPath        string         `json:"repoPath"`
	RepoURL         string         `json:"repoUrl"`
	SupportedModels []string       `json:"supportedModels"`
	AvailableTools  []string       `json:"availableTools"`
	ToolPermissions map[string]any `json:"toolPermissions"`
	ContractVersion string         `json:"contractVersion"`
}

type Harness struct {
	ID string `json:"id"`
	HarnessSettings
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
