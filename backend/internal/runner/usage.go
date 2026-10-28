package runner

import (
	"context"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// ExecutionUsage is a local handoff, not a new Runner API payload. WallTimeMS
// covers this Execute call through arrival at step 14, including preparation,
// verification and publishing. HarnessWallTimeMS measures step 8; HarnessUsage
// preserves the adapter's own measurements. Nil means the stage/result was
// unavailable (for example a resume starting at Gate 2), never measured zero.
type ExecutionUsage struct {
	WallTimeMS        int64
	HarnessWallTimeMS *int64
	HarnessUsage      *adapter.Usage
}

// UsageRecorder must acknowledge only after usage is recorded. The transport
// to the Control Plane is intentionally unspecified: specs 01/04/05 define no
// usage reporting API. Runner must not open the management database itself.
type UsageRecorder interface {
	RecordUsage(context.Context, string, ExecutionUsage) error
}

// NewUsageStage implements step 14. Install it as the StageComplete handler.
// The controlled executor reports succeeded only after this handler succeeds;
// an unavailable recorder must not be replaced with a successful no-op.
func NewUsageStage(recorder UsageRecorder) (StageFunc, error) {
	if recorder == nil {
		return nil, fmt.Errorf("runner: usage recorder is required")
	}
	return func(ctx context.Context, execution *Execution) error {
		usage := ExecutionUsage{
			WallTimeMS:        execution.now().Sub(execution.startedAt).Milliseconds(),
			HarnessWallTimeMS: execution.harnessWallTimeMS,
		}
		if execution.HarnessResult != nil {
			harness := execution.HarnessResult.Usage
			usage.HarnessUsage = &harness
		}
		return recorder.RecordUsage(ctx, execution.Claim.Execution.ID, usage)
	}, nil
}
