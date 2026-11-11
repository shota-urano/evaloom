package runner

import (
	"context"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

// WithStageSecrets is the key preflight boundary for a stage executor. It must
// wrap sandbox creation/start, so no stage begins until all declared keys pass
// validation. ConfigError exposes the failed/config_error result classification.
// Transport to a sandbox and log/artifact exclusion belong to the stage layer;
// start receives only this stage's environment and exact redaction values.
func (runner *Runner) WithStageSecrets(ctx context.Context, harnessID string, stage model.Stage, declarations []contract.Secret, start func(context.Context, secretstore.Resolved) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if start == nil {
		return fmt.Errorf("runner: stage start function is required")
	}
	resolved, err := secretstore.New(runner.dataDir).ForStage(harnessID, stage, declarations)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return start(ctx, resolved)
}
