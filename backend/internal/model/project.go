package model

// ProjectSettings is the owner-editable Registry configuration (spec 02, 3.2).
// Unspecified timeout keys inherit the global defaults when executing work.
type ProjectSettings struct {
	Name              string         `json:"name"`
	RunnerID          string         `json:"runnerId"`
	DefaultRole       string         `json:"defaultRole"`
	ReviewRole        *string        `json:"reviewRole"`
	AllowedOperations map[string]any `json:"allowedOperations"`
	Timeouts          map[string]int `json:"timeouts"`
	// LinearTeamID and LinearRoutingLabel route Linear Issues to this Project
	// (spec 15 3.6, 3.8). A Project with either one null is never run from
	// Linear.
	LinearTeamID       *string `json:"linearTeamId"`
	LinearRoutingLabel *string `json:"linearRoutingLabel"`
}

type Project struct {
	ID string `json:"id"`
	ProjectSettings
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
