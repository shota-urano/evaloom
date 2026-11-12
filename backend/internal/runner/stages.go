package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// StageReporter is the spec 04 3.4.1 reporting boundary. StartStage must
// register request as a request_json artifact before sending stage_started,
// and return the Control Plane's attempt. FinishStage returns its next reply.
// Delivery retries belong here and must never rerun the producing stage.
type StageReporter interface {
	StartStage(context.Context, string, model.Stage, []byte) (int, error)
	FinishStage(context.Context, string, model.Stage, int, StageOutcome) (StageNext, error)
}

type StageNext struct {
	Next             string                     `json:"next"`
	CheckedContentID *string                    `json:"checkedContentId"`
	Approvals        []contract.RequestApproval `json:"approvals"`
}

// StageOutcome contains the collected, redacted stage data. The executor owns
// result-file collection, work result assembly, artifacts and supervision.
// The framework does not validate program results or decide their status.
type StageOutcome struct {
	ExitCode         *int            `json:"exitCode"`
	RunnerError      *string         `json:"runnerError"`
	Result           json.RawMessage `json:"result"`
	ResultArtifactID *string         `json:"resultArtifactId"`
	Work             *StageWork      `json:"work"`
	Usage            StageUsage      `json:"usage"`
}

type StageWork struct {
	SessionID         *string         `json:"sessionId"`
	SummaryArtifactID *string         `json:"summaryArtifactId"`
	Question          json.RawMessage `json:"question"`
}

type StageUsage struct {
	WallTimeMS   int64    `json:"wallTimeMs"`
	InputTokens  *int64   `json:"inputTokens"`
	OutputTokens *int64   `json:"outputTokens"`
	CostAmount   *float64 `json:"costAmount"`
}

// StageInvocation separates host files from sandbox-visible request paths.
// Implementations must mount request and inputs read-only, apply contract 6.2
// visibility, and return only after the whole stage has stopped. WorkSessionID
// is Adapter metadata, never an extra field in the harness request JSON.
type StageInvocation struct {
	Stage         model.Stage
	Attempt       int
	Profile       LoadedProfile
	Workspace     string
	Directory     string
	WorkSessionID *string
	// Host socket source. The executor supplies its own sandbox-visible
	// EVALOOM_CHANNEL path and copies this into SandboxSpec.ChannelPath.
	// The context carries the matching, stage-scoped socket capability.
	ChannelPath string
}

type StageExecutor interface {
	RunStage(context.Context, StageInvocation) StageOutcome
}

type StageServices struct {
	Reporter StageReporter
	Executor StageExecutor
	// Paths are supplied by Sandbox wiring. The contract gives examples,
	// not fixed mount paths; the framework must not choose its own defaults.
	Paths contract.RequestPaths
}

type stageResume struct {
	Stage            model.Stage                `json:"stage"`
	CheckedContentID *string                    `json:"checkedContentId"`
	Approvals        []contract.RequestApproval `json:"approvals"`
	Answer           *contract.Answer           `json:"answer"`
	WorkSessionID    *string                    `json:"workSessionId"`
	CompletedStages  []model.Stage              `json:"completedStages"`
}

var executionStageOrder = []model.Stage{model.StagePrepare, model.StageWork, model.StageCheck, model.StagePublish}

func stageIndex(stage model.Stage) int {
	for i, s := range executionStageOrder {
		if stage == s {
			return i
		}
	}
	return -1
}

// ExecuteStages runs the frame of spec 05 3.3 and prepares and checks the
// workspace (3.5). Secret preflight and Sandbox/Adapter implementation remain
// caller boundaries. Workspace contents are retained on failure or stop.
// Profile preflight errors are returned before any stage starts, as WithProfile
// requires. Job wiring owns reporting those preflight failures.
func (runner *Runner) ExecuteStages(ctx context.Context, claim Claim, services StageServices) error {
	if services.Reporter == nil || services.Executor == nil {
		return fmt.Errorf("runner: stage reporter and executor are required")
	}
	for _, path := range []string{services.Paths.Profile, services.Paths.Workspace, services.Paths.Out,
		services.Paths.Inputs, services.Paths.Records, services.Paths.Scratch} {
		if path == "" {
			return fmt.Errorf("runner: sandbox request paths are required")
		}
	}
	return runner.WithProfile(ctx, claim, func(ctx context.Context, profile LoadedProfile) error {
		return runner.executeStages(ctx, claim, profile, services)
	})
}

func (runner *Runner) executeStages(ctx context.Context, claim Claim, profile LoadedProfile, services StageServices) error {
	stage := model.StageWork
	if profile.Definition.Stages.Prepare != nil {
		stage = model.StagePrepare
	}
	var resume stageResume
	if raw := bytes.TrimSpace(claim.Resume); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &resume); err != nil ||
			(resume.Stage != model.StageWork && resume.Stage != model.StagePublish) {
			return fmt.Errorf("runner: invalid stage resume")
		}
		stage = resume.Stage
	}
	if (claim.Execution.ResumeStage == nil) != (resume.Stage == "") ||
		(claim.Execution.ResumeStage != nil && *claim.Execution.ResumeStage != string(resume.Stage)) {
		return fmt.Errorf("runner: inconsistent stage resume")
	}
	completed := append([]model.Stage(nil), resume.CompletedStages...)
	seen := map[model.Stage]bool{}
	for _, done := range completed {
		if stageIndex(done) < 0 || stageIndex(done) >= stageIndex(stage) || seen[done] ||
			(done == model.StagePrepare && profile.Definition.Stages.Prepare == nil) {
			return fmt.Errorf("runner: invalid completed stages")
		}
		seen[done] = true
	}
	workspacePrepared := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := stageRequest(claim, stage, services.Paths, resume)
		if err != nil {
			return err
		}
		attempt, err := services.Reporter.StartStage(ctx, claim.Execution.ID, stage, request)
		if err != nil {
			return err
		}
		if attempt < 1 {
			return fmt.Errorf("runner: invalid stage attempt")
		}
		directory, err := runner.prepareStageFiles(claim.Execution.ID, stage, attempt, request, completed)
		if err == nil {
			if !workspacePrepared {
				err = runner.prepareWorkspace(claim)
				workspacePrepared = err == nil
			} else {
				err = runner.checkWorkspace(claim)
			}
		}
		var outcome StageOutcome
		if err != nil {
			code := "config_error"
			if errors.Is(err, errStageOutMissing) || errors.Is(err, errWorkspaceMissing) {
				code = "workspace_missing"
			}
			outcome.RunnerError = &code
		} else if err := ctx.Err(); err != nil {
			code := "cancelled"
			outcome.RunnerError = &code
		} else {
			invocation := StageInvocation{Stage: stage, Attempt: attempt, Profile: profile,
				Workspace: claim.Workspace.Path, Directory: directory}
			if stage == model.StageWork {
				invocation.WorkSessionID = resume.WorkSessionID
			}
			outcome = runner.runStageWithChannel(ctx, claim.Execution.ID, invocation, services.Executor)
		}
		next, err := services.Reporter.FinishStage(ctx, claim.Execution.ID, stage, attempt, outcome)
		if err != nil {
			return err
		}
		if next.Next == "stop" {
			return nil
		}
		// A malformed response must not skip stages, restart a failed stage,
		// or create an automatic retry. Only the immediate successor is legal.
		i := stageIndex(stage)
		if outcome.RunnerError != nil || i+1 >= len(executionStageOrder) || next.Next != string(executionStageOrder[i+1]) {
			return fmt.Errorf("runner: invalid next stage")
		}
		completed = append(completed, stage)
		stage = model.Stage(next.Next)
		if stage == model.StagePublish {
			resume.CheckedContentID, resume.Approvals = next.CheckedContentID, next.Approvals
		}
	}
}

func stageRequest(claim Claim, stage model.Stage, paths contract.RequestPaths, resume stageResume) ([]byte, error) {
	r := contract.Request{
		ContractVersion: contract.Version, Stage: stage,
		Execution: contract.RequestExecution{ID: claim.Execution.ID, Seq: claim.Execution.Seq, Mode: model.ExecutionMode(claim.Execution.Mode)},
		Task:      contract.RequestTask{ID: claim.Task.ID, Title: claim.Task.Title, Body: claim.Task.Body, AcceptanceCriteria: claim.Task.AcceptanceCriteria},
		Params:    claim.Params, Profile: contract.RequestProfile{HarnessID: claim.Profile.HarnessID, Version: claim.Profile.Version, CommitSHA: claim.Profile.CommitSHA},
		Paths: paths, Limits: contract.RequestLimits{TimeoutSec: claim.Limits.TimeoutSec[string(stage)]},
	}
	if claim.Execution.ResumeStage != nil {
		s := model.Stage(*claim.Execution.ResumeStage)
		r.Execution.ResumeStage = &s
	}
	if json.Unmarshal(claim.Task.Source, &r.Task.Source) != nil || json.Unmarshal(claim.Route, &r.Route) != nil {
		return nil, fmt.Errorf("runner: invalid stage request source or route")
	}
	if len(claim.Previous) > 0 && json.Unmarshal(claim.Previous, &r.Previous) != nil {
		return nil, fmt.Errorf("runner: invalid previous execution")
	}
	if stage == model.StageWork {
		r.Answer = resume.Answer
	}
	if stage == model.StagePublish {
		r.CheckedContentID, r.Approvals = resume.CheckedContentID, resume.Approvals
	}
	data, err := json.Marshal(r)
	// Re-decode through the existing strict contract, including required and
	// stage-specific fields. No Profile definition, secret values or host-only
	// metadata are copied into the request.
	var validated contract.Request
	if err != nil || json.Unmarshal(data, &validated) != nil {
		return nil, fmt.Errorf("runner: invalid stage request")
	}
	return data, nil
}
