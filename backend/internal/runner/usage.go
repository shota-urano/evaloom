package runner

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type stageMeasurement struct {
	work *adapter.Usage
}

// Measure only the executor, excluding channel setup, redaction and reporting.
// Executors return after termination and collection, including cancellation.
type usageExecutor struct{ StageExecutor }

func (e usageExecutor) RunStage(ctx context.Context, in StageInvocation) StageOutcome {
	in.measurement = &stageMeasurement{}
	started := time.Now()
	outcome := e.StageExecutor.RunStage(ctx, in)
	outcome.Usage.WallTimeMS = time.Since(started).Milliseconds()
	if in.Stage == model.StageWork && in.measurement.work != nil {
		// WaitWork captures the Adapter's optional counts even on failure. Its
		// duration does not replace Runner's measurement of this invocation.
		outcome.Usage.InputTokens = in.measurement.work.InputTokens
		outcome.Usage.OutputTokens = in.measurement.work.OutputTokens
		// The current Adapter interface does not report cost. Do not estimate it.
		outcome.Usage.CostAmount = nil
	}
	return outcome
}

func programStageUsage(raw json.RawMessage, wallTimeMS int64) StageUsage {
	usage := StageUsage{WallTimeMS: wallTimeMS}
	var result contract.Result
	if json.Unmarshal(raw, &result) == nil && result.Usage != nil {
		usage.InputTokens = result.Usage.InputTokens
		usage.OutputTokens = result.Usage.OutputTokens
		usage.CostAmount = result.Usage.CostAmount
	}
	// Invalid results still go unchanged to the Control Plane for classification.
	// Their usage must not turn that report into a transport validation failure.
	return usage
}
