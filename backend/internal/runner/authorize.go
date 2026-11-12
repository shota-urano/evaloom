package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
)

// Authorizer is the publish channel's Control Plane boundary. The stage relay
// supplies its own Execution ID; it must never accept one from the sandbox.
type Authorizer interface {
	Authorize(context.Context, string, contract.AuthorizeRequest) contract.AuthorizeResponse
}

var _ Authorizer = (*Runner)(nil)

// Authorize makes exactly one attempt. A lost response may have consumed the
// approval, so neither transport failures nor malformed replies are retried.
func (runner *Runner) Authorize(ctx context.Context, executionID string, input contract.AuthorizeRequest) contract.AuthorizeResponse {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // spec 05, 3.7
	defer cancel()
	unavailable := contract.AuthorizeResponse{Reason: "unavailable"}
	id, err := ulid.ParseStrict(executionID)
	if err != nil || id.String() != executionID || input.Validate() != nil {
		return unavailable
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", executionID, "authorize")
	if err != nil {
		return unavailable
	}
	body, err := json.Marshal(input)
	if err != nil {
		return unavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return unavailable
	}
	request.Header.Set("Authorization", "Bearer "+runner.token)
	request.Header.Set("Content-Type", "application/json")
	client := *runner.client
	client.Timeout = 10 * time.Second // heartbeat's 5s timeout is independent
	response, err := client.Do(request)
	if err != nil {
		return unavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return unavailable
	}
	var reply contract.AuthorizeResponse
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&reply); err != nil {
		return unavailable
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return unavailable
	}
	return reply
}
