package model

import "encoding/json"

// ProjectSettings contains only the six owner-managed items in spec 02, 3.2.
// Routing labels live in routes, separately from this configuration.
type ProjectSettings struct {
	Name            string          `json:"name"`
	HarnessID       string          `json:"harnessId"`
	ProfileID       string          `json:"profileId"`
	Params          json.RawMessage `json:"params"`
	ApprovalWaivers []string        `json:"approvalWaivers"`
	RunnerID        string          `json:"runnerId"`
}

type Project struct {
	ID string `json:"id"`
	ProjectSettings
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
