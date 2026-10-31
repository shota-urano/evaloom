package model

import "time"

// TestRun is the measurement record in spec 02. Unknown measurements stay nil.
type TestRun struct {
	ID                string    `json:"id"`
	ExecutionID       string    `json:"executionId"`
	Kind              string    `json:"kind"`
	TargetSHA         string    `json:"targetSha"`
	Command           string    `json:"command"`
	ExitCode          *int      `json:"exitCode"`
	Total             *int      `json:"total"`
	Passed            *int      `json:"passed"`
	Failed            *int      `json:"failed"`
	Skipped           *int      `json:"skipped"`
	TestIDsArtifactID *string   `json:"testIdsArtifactId"`
	TimedOut          bool      `json:"timedOut"`
	StartedAt         time.Time `json:"startedAt"`
	FinishedAt        time.Time `json:"finishedAt"`
}
