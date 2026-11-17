package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

var _ StageReporter = (*Runner)(nil)

// StartStage registers the request before starting the stage (spec 04 3.4.1).
// Both deliveries retain their identities across transport failures, so losing
// an acknowledgement cannot allocate another attempt or rerun a stage.
func (runner *Runner) StartStage(ctx context.Context, id string, stage model.Stage, request []byte) (int, error) {
	if err := validateStageReportIdentity(id, stage); err != nil {
		return 0, err
	}
	directory := filepath.Join(runner.dataDir, "artifacts")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return 0, fmt.Errorf("runner: create request artifact directory failed")
	}
	artifactID := ulid.Make().String()
	path := filepath.Join(directory, artifactID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, fmt.Errorf("runner: create request artifact failed")
	}
	_, writeErr := file.Write(request)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return 0, fmt.Errorf("runner: save request artifact failed")
	}
	if err := runner.deliverArtifact(ctx, LogArtifact{ID: artifactID, ExecutionID: id, Stage: string(stage),
		Kind: "request_json", Name: "request.json", Path: path}); err != nil {
		return 0, err
	}
	input := struct {
		Type              string      `json:"type"`
		Stage             model.Stage `json:"stage"`
		RequestArtifactID string      `json:"requestArtifactId"`
	}{"stage_started", stage, artifactID}
	var reply struct {
		Attempt int `json:"attempt"`
	}
	if err := runner.postStageReport(ctx, id, input, &reply); err != nil {
		return 0, err
	}
	if reply.Attempt < 1 {
		return 0, fmt.Errorf("runner: invalid stage attempt response")
	}
	return reply.Attempt, nil
}

// FinishStage delivers only the collected outcome. It never decides whether a
// program result passed; that decision and the next stage belong to the API.
func (runner *Runner) FinishStage(ctx context.Context, id string, stage model.Stage, attempt int, outcome StageOutcome) (StageNext, error) {
	if err := validateStageReportIdentity(id, stage); err != nil {
		return StageNext{}, err
	}
	if attempt < 1 {
		return StageNext{}, fmt.Errorf("runner: invalid stage attempt")
	}
	input := struct {
		Type    string      `json:"type"`
		Stage   model.Stage `json:"stage"`
		Attempt int         `json:"attempt"`
		StageOutcome
	}{"stage_finished", stage, attempt, outcome}
	var reply StageNext
	if err := runner.postStageReport(ctx, id, input, &reply); err != nil {
		return StageNext{}, err
	}
	switch reply.Next {
	case "work", "check", "publish", "stop":
		return reply, nil
	default:
		return StageNext{}, fmt.Errorf("runner: invalid next stage response")
	}
}

func validateStageReportIdentity(id string, stage model.Stage) error {
	parsed, err := ulid.ParseStrict(id)
	if err != nil || parsed.String() != id || stage.Validate() != nil {
		return fmt.Errorf("runner: invalid stage report identity")
	}
	return nil
}

func (runner *Runner) postStageReport(ctx context.Context, id string, input, reply any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("runner: encode stage report failed")
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", id, "result")
	if err != nil {
		return fmt.Errorf("runner: invalid stage report URL")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("runner: create stage report request failed")
		}
		request.Header.Set("Authorization", "Bearer "+runner.token)
		request.Header.Set("Content-Type", "application/json")
		response, transportErr := runner.client.Do(request)
		if transportErr == nil {
			if response.StatusCode == http.StatusOK {
				decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
				var raw json.RawMessage
				decodeErr := decoder.Decode(&raw)
				if decodeErr == nil {
					_, decodeErr = decoder.Token()
					if decodeErr == io.EOF {
						decodeErr = nil
					} else if decodeErr == nil {
						decodeErr = fmt.Errorf("trailing JSON")
					}
				}
				response.Body.Close()
				if decodeErr == nil {
					if err := json.Unmarshal(raw, reply); err != nil {
						return fmt.Errorf("runner: invalid stage report response")
					}
					return nil
				}
				// A truncated acknowledgement may follow a committed report.
				// Resend the same body; the API returns the original response.
			} else {
				response.Body.Close()
				if response.StatusCode < 500 {
					return fmt.Errorf("runner: stage report returned HTTP %d", response.StatusCode)
				}
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
