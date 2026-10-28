package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ExecutionResult uses the existing result endpoint's transition contract.
// Stage-specific metadata reporting remains the responsibility of stage code.
type ExecutionResult struct {
	FromStatus model.ExecutionStatus `json:"fromStatus"`
	Status     model.ExecutionStatus `json:"status"`
	ErrorClass model.ErrorClass      `json:"errorClass,omitempty"`
	WorktreeID string                `json:"worktreeId,omitempty"`
}

type ResultReporter interface {
	ReportResult(context.Context, string, ExecutionResult) error
}

var _ ResultReporter = (*Runner)(nil)

// ReportResult keeps a failed report pending on the calling execution and
// retries transport/5xx failures without rerunning its stage. Use a delivery
// context independent of the cancelled Execution. 4xx responses are returned:
// the current API rejects duplicate transitions with 409, so a lost success
// acknowledgement cannot be reconciled through the existing Runner API.
func (runner *Runner) ReportResult(ctx context.Context, id string, result ExecutionResult) error {
	if parsed, err := ulid.ParseStrict(id); err != nil || parsed.String() != id {
		return fmt.Errorf("runner: invalid result execution ID")
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", id, "result")
	if err != nil {
		return fmt.Errorf("runner: invalid result URL")
	}
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("runner: encode result failed")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("runner: create result request failed")
		}
		request.Header.Set("Authorization", "Bearer "+runner.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := runner.client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				return nil
			}
			if response.StatusCode < 500 {
				return fmt.Errorf("runner: result returned HTTP %d", response.StatusCode)
			}
		}
		// Reuse the Runner's existing poll interval; Q15/grace have no defaults.
		timer := time.NewTimer(runner.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
