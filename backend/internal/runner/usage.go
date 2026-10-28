package runner

import "github.com/shota-urano/evaloom/backend/internal/adapter"

// ExecutionUsage is sent with the succeeded result. WallTimeMS
// covers this Execute call through completion of step 14, including preparation,
// verification and publishing. HarnessWallTimeMS measures step 8; HarnessUsage
// preserves the adapter's own measurements. Nil means the stage/result was
// unavailable (for example a resume starting at Gate 2), never measured zero.
type ExecutionUsage struct {
	WallTimeMS        int64          `json:"wallTimeMs"`
	HarnessWallTimeMS *int64         `json:"harnessWallTimeMs,omitempty"`
	HarnessUsage      *adapter.Usage `json:"harnessUsage,omitempty"`
}

// completedUsage captures step 14's measurements once, before ReportResult's
// transport retries. Only the Control Plane writes the management database.
func (execution *Execution) completedUsage() *ExecutionUsage {
	usage := &ExecutionUsage{
		WallTimeMS:        execution.now().Sub(execution.startedAt).Milliseconds(),
		HarnessWallTimeMS: execution.harnessWallTimeMS,
	}
	if execution.HarnessResult != nil {
		harness := execution.HarnessResult.Usage
		usage.HarnessUsage = &harness
	}
	return usage
}
