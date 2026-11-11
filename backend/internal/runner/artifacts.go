package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
)

var _ ArtifactRegistrar = (*Runner)(nil)

// RegisterLogArtifact uploads the closed local file. Deliver retries failures
// with the same ID and retains the file, including when an acknowledgement is
// lost after the Control Plane has committed the artifact.
func (runner *Runner) RegisterLogArtifact(ctx context.Context, artifact LogArtifact) error {
	if artifact.Kind != "log" {
		return fmt.Errorf("runner: invalid log artifact kind")
	}
	return runner.registerArtifact(ctx, artifact)
}

// deliverArtifact retains one closed file and identity until registration is
// acknowledged or the execution is cancelled. Never rerun the producing stage.
func (runner *Runner) deliverArtifact(ctx context.Context, artifact LogArtifact) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := runner.registerArtifact(ctx, artifact); err == nil {
			return nil
		}
		// Spec 05, section 6: preserve the local artifact for later resend.
		// Reuse the Runner's existing retry interval; no new timeout default.
		timer := time.NewTimer(runner.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// registerArtifact uploads the supplied stage and display name unchanged.
func (runner *Runner) registerArtifact(ctx context.Context, artifact LogArtifact) error {
	for _, value := range []string{artifact.ID, artifact.ExecutionID} {
		if id, err := ulid.ParseStrict(value); err != nil || id.String() != value {
			return fmt.Errorf("runner: invalid artifact identity")
		}
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", artifact.ExecutionID, "artifacts")
	if err != nil {
		return fmt.Errorf("runner: invalid artifact URL")
	}
	redacted := "0"
	if artifact.Redacted {
		redacted = "1"
	}
	query := url.Values{"artifactId": {artifact.ID}, "stage": {artifact.Stage}, "kind": {artifact.Kind}, "name": {artifact.Name}, "redacted": {redacted}}
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
