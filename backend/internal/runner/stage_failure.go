package runner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// A Runner-decided failure is explained by one stream=runner log line that
// starts with "<runnerError>: " (e.g. "sandbox_error: "). The Control Plane
// copies the stage's lines with that prefix into executions.error_message
// (store.FinishStageTx), since stage_finished has no message field.

// MaxStageFailureReason bounds the recorded reason, in characters.
const MaxStageFailureReason = 1000

// stageFailureReason renders the safe operation and the runtime's own reason
// (exit status, stderr, detail). The result is unredacted: record it only
// through Log.Note, which applies secret exclusion before truncation.
func stageFailureReason(err error) string {
	if err == nil {
		return ""
	}
	reason := err.Error()
	var runtime *sandbox.RuntimeError
	if errors.As(err, &runtime) {
		if runtime.ExitCode >= 0 {
			reason += fmt.Sprintf(": runtime exit code %d", runtime.ExitCode)
		}
		if detail := strings.TrimSpace(runtime.Detail); detail != "" {
			reason += ": " + detail
		}
		if stderr := strings.TrimSpace(runtime.Stderr); stderr != "" {
			reason += ": " + stderr
		}
	}
	return reason
}

// noteStageFailure leaves the reason of a Runner-decided failure in the stage
// log before it is finished. Outcomes without a cause keep their logs as is.
func noteStageFailure(log *Log, outcome StageOutcome) {
	if log == nil || outcome.Err == nil || outcome.RunnerError == nil {
		return
	}
	_ = log.Note(*outcome.RunnerError+": "+stageFailureReason(outcome.Err), MaxStageFailureReason)
}
