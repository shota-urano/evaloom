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
)

// ProposalResult is POST /api/route-proposals/{id}/result (spec 01 5.2, 15 3.6.4).
// Runner does not validate labels against candidates; the Control Plane does.
type ProposalResult struct {
	Label      *string `json:"label"`
	Reason     *string `json:"reason"`
	ErrorClass *string `json:"errorClass"`
}

// ReportProposalResult retries transport and 5xx failures without re-running the proposal.
func (runner *Runner) ReportProposalResult(ctx context.Context, id string, result ProposalResult) error {
	if parsed, err := ulid.ParseStrict(id); err != nil || parsed.String() != id {
		return fmt.Errorf("runner: invalid route proposal ID")
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "route-proposals", id, "result")
	if err != nil {
		return fmt.Errorf("runner: invalid route proposal result URL")
	}
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("runner: encode route proposal result failed")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("runner: create route proposal result request failed")
		}
		request.Header.Set("Authorization", "Bearer "+runner.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := runner.client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			if response.StatusCode < 500 {
				return fmt.Errorf("runner: route proposal result returned HTTP %d", response.StatusCode)
			}
		}
		timer := time.NewTimer(runner.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
