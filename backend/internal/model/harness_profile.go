package model

import "encoding/json"

// ProfileDefinition follows data-model 4.3. Model IDs remain opaque strings
// until a Model Registry is introduced.
type ProfileDefinition struct {
	ProfileVersion string `json:"profileVersion"`
	HarnessVersion string `json:"harnessVersion"`
	ModelID        string `json:"modelId"`
	PromptVersion  string `json:"promptVersion"`
	SkillVersions  []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"skillVersions"`
	ToolConfiguration struct {
		AllowedTools []string `json:"allowedTools"`
	} `json:"toolConfiguration"`
	ContextPolicyVersion string `json:"contextPolicyVersion"`
	// ContextFiles explicitly selects repository-relative files at CommitSHA.
	// Omission selects no files; version labels are never interpreted as paths.
	ContextFiles           []string `json:"contextFiles,omitempty"`
	ExecutionConfiguration struct {
		Adapter string   `json:"adapter"`
		Args    []string `json:"args"`
	} `json:"executionConfiguration"`
	CollaborationPolicy     string  `json:"collaborationPolicy"`
	EvaluationPolicyVersion *string `json:"evaluationPolicyVersion"`
}

type HarnessProfile struct {
	ID              string          `json:"id"`
	HarnessID       string          `json:"harnessId"`
	Version         string          `json:"version"`
	CommitSHA       string          `json:"commitSha"`
	TagName         string          `json:"tagName"`
	Definition      json.RawMessage `json:"definition"`
	DefinitionHash  string          `json:"definitionHash"`
	ModelID         string          `json:"modelId"`
	OperationStatus string          `json:"operationStatus"`
	RegisteredAt    string          `json:"registeredAt"`
}
