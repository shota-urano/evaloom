package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// BaselineConfig is trusted runtime wiring, not a new public API or Registry
// setting. Limits and the shell executable must be supplied explicitly. The
// shell exists only inside the Test Sandbox; no command runs on the host.
// Resolver is shared with Gate 2; nil deliberately selects count-only parsing.
type BaselineConfig struct {
	Sandbox sandbox.Sandbox
	Limits  sandbox.ResourceLimits
	Shell   string
	// Registry spelling for the supported report format is explicit because
	// spec 03 does not define a report-format enum.
	JUnitFormat string
	Resolver    gate.JUnitIDResolver
}

// BaselineHandler constructs the complete stage-7 path: isolated execution,
// fresh JUnit report, test_ids upload, then durable result acknowledgement.
// Install this handler at StageTestBaseline in the existing stage table.
func (runner *Runner) BaselineHandler(config BaselineConfig) (StageFunc, error) {
	if config.Sandbox == nil || config.Shell == "" || config.Shell[0] != '/' || config.JUnitFormat == "" ||
		config.Limits.CPU <= 0 || config.Limits.MemoryBytes <= 0 || config.Limits.Time <= 0 {
		return nil, fmt.Errorf("runner: explicit baseline runtime, shell and resource limits are required")
	}
	return func(ctx context.Context, execution *Execution) error {
		if execution == nil || execution.control == nil || execution.status != model.ExecutionPreparing {
			return fmt.Errorf("runner: controlled preparing execution is required for baseline")
		}
		if execution.Claim.Worktree != nil {
			w := execution.Claim.Worktree
			if w.ID != execution.WorktreeID || w.BaseSHA != execution.BaseSHA || w.Path != execution.WorktreePath {
				return fmt.Errorf("runner: prepared worktree differs from claim")
			}
		}
		stage := BaselineStage{
			Repository: &remoteBaselineRepository{runner: runner, execution: execution},
			Sandbox:    newPreparedBaselineSandbox(config),
			Parse:      func(reader io.Reader) (gate.TestReport, error) { return gate.ParseJUnitXML(reader, config.Resolver) },
			RecordIDs: func(_ context.Context, id string, ids []string) (string, error) {
				return runner.recordBaselineIDs(execution.control.ReportContext, id, ids)
			},
		}
		return stage.Run(ctx, execution)
	}, nil
}

type remoteBaselineRepository struct {
	runner    *Runner
	execution *Execution
}

var _ BaselineRepository = (*remoteBaselineRepository)(nil)

func (r *remoteBaselineRepository) FindBaseline(ctx context.Context, executionID, worktreeID string) (*model.TestRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if executionID != r.execution.Claim.Execution.ID || worktreeID != r.execution.WorktreeID {
		return nil, fmt.Errorf("runner: invalid baseline lookup scope")
	}
	if r.execution.Baseline != nil {
		return r.execution.Baseline, nil
	}
	if w := r.execution.Claim.Worktree; w != nil {
		if w.ID != worktreeID {
			return nil, fmt.Errorf("runner: baseline claim worktree mismatch")
		}
		return w.Baseline, nil
	}
	// Explicit null means the predecessor failed before recording a worktree;
	// preparation can create one and acquire its baseline. Missing information
	// in an old/partial claim must still never trigger another test run.
	if r.execution.Claim.Execution.Mode == "continue" && r.execution.Claim.Execution.Seq > 1 && !r.execution.Claim.worktreeSnapshotPresent {
		return nil, fmt.Errorf("runner: continue claim lacks saved worktree baseline information")
	}
	return nil, nil
}

func (r *remoteBaselineRepository) SaveBaseline(ctx context.Context, worktreeID string, run model.TestRun) error {
	if worktreeID != r.execution.WorktreeID || run.ExecutionID != r.execution.Claim.Execution.ID {
		return fmt.Errorf("runner: invalid baseline save scope")
	}
	return r.runner.ReportResult(r.execution.control.ReportContext, run.ExecutionID, ExecutionResult{
		FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		WorktreeID: worktreeID, BaseSHA: r.execution.BaseSHA, Baseline: &run,
	})
}

// Use the existing artifact endpoint with the same ID and bytes for retries.
// JSON arrays preserve ID strings without inventing a test-runner ID rule.
func (runner *Runner) recordBaselineIDs(ctx context.Context, executionID string, ids []string) (string, error) {
	if parsed, err := ulid.ParseStrict(executionID); err != nil || parsed.String() != executionID {
		return "", fmt.Errorf("runner: invalid baseline artifact execution ID")
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	id := ulid.Make().String()
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", executionID, "artifacts")
	if err != nil {
		return "", fmt.Errorf("runner: invalid baseline artifact URL")
	}
	query := url.Values{"artifactId": {id}, "kind": {"test_ids"}, "redacted": {"0"}}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?"+query.Encode(), bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("runner: create baseline artifact request failed")
		}
		request.Header.Set("Authorization", "Bearer "+runner.token)
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := runner.client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return id, nil
			}
			if response.StatusCode < 500 {
				return "", fmt.Errorf("runner: baseline artifact returned HTTP %d", response.StatusCode)
			}
		}
		timer := time.NewTimer(runner.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}
