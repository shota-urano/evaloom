package model

// Task is the persisted manual task input and its current lifecycle state.
type Task struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	// HarnessID is set for a direct routing to a special harness (spec 02
	// 3.3); "" otherwise. ProjectID and HarnessID are never both set.
	HarnessID          string     `json:"harnessId"`
	Title              string     `json:"title"`
	Body               string     `json:"body"`
	AcceptanceCriteria string     `json:"acceptanceCriteria"`
	ShortName          string     `json:"shortName"`
	TaskType           string     `json:"taskType"`
	CollaborationMode  string     `json:"collaborationMode"`
	Status             TaskStatus `json:"status"`
	IdempotencyKey     string     `json:"-"`
	CreatedAt          string     `json:"createdAt"`
	UpdatedAt          string     `json:"updatedAt"`
}
