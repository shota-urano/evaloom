package model

// ProjectSettings is the owner-editable Registry configuration (spec 02, 3.2).
// Unspecified timeout keys inherit the global defaults when executing work.
type ProjectSettings struct {
	Name              string         `json:"name"`
	LocalRepoPath     string         `json:"localRepoPath"`
	GitRemoteURL      string         `json:"gitRemoteUrl"`
	RunnerID          string         `json:"runnerId"`
	BaseBranch        string         `json:"baseBranch"`
	DefaultRole       string         `json:"defaultRole"`
	ReviewRole        *string        `json:"reviewRole"`
	ContextLocation   string         `json:"contextLocation"`
	TestCommand       *string        `json:"testCommand"`
	TestReportFormat  *string        `json:"testReportFormat"`
	TestReportPath    *string        `json:"testReportPath"`
	SetupCommand      *string        `json:"setupCommand"`
	CopyAllowlist     []string       `json:"copyAllowlist"`
	TestFilePatterns  []string       `json:"testFilePatterns"`
	AllowedOperations map[string]any `json:"allowedOperations"`
	ApprovalPolicy    map[string]any `json:"approvalPolicy"`
	Timeouts          map[string]int `json:"timeouts"`
}

type Project struct {
	ID string `json:"id"`
	ProjectSettings
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
