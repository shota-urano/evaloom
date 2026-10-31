package model

import "time"

// TestRun is the measurement record in spec 02. Unknown measurements stay nil.
type TestRun struct {
	ID, ExecutionID, Kind, TargetSHA, Command string
	ExitCode, Total, Passed, Failed, Skipped  *int
	TestIDsArtifactID                         *string
	TimedOut                                  bool
	StartedAt, FinishedAt                     time.Time
}
