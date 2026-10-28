package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/oklog/ulid/v2"
)

var _ ArtifactRegistrar = (*Runner)(nil)

// RegisterLogArtifact uploads the closed local file. Deliver retries failures
// with the same ID and retains the file, including when an acknowledgement is
// lost after the Control Plane has committed the artifact.
func (runner *Runner) RegisterLogArtifact(ctx context.Context, artifact LogArtifact) error {
	for _, value := range []string{artifact.ID, artifact.ExecutionID} {
		if id, err := ulid.ParseStrict(value); err != nil || id.String() != value {
			return fmt.Errorf("runner: invalid artifact identity")
		}
	}
	if artifact.Kind != "harness_log" && artifact.Kind != "setup_log" && artifact.Kind != "test_log" {
		return fmt.Errorf("runner: invalid log artifact kind")
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", artifact.ExecutionID, "artifacts")
	if err != nil {
		return fmt.Errorf("runner: invalid artifact URL")
	}
	redacted := "0"
	if artifact.Redacted {
		redacted = "1"
	}
	query := url.Values{"artifactId": {artifact.ID}, "kind": {artifact.Kind}, "redacted": {redacted}}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return fmt.Errorf("runner: open saved artifact failed")
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?"+query.Encode(), file)
	if err != nil {
		return fmt.Errorf("runner: create artifact request failed")
	}
	request.Header.Set("Authorization", "Bearer "+runner.token)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := runner.client.Do(request)
	if err != nil {
		return fmt.Errorf("runner: artifact transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("runner: artifact returned HTTP %d", response.StatusCode)
	}
	return nil
}
