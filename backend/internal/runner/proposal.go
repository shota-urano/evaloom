package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

// DefaultProposalPaths are contract 6.1 example paths used for Proposal Sandbox mounts.
var DefaultProposalPaths = contract.RequestPaths{
	Profile: "/evaloom/profile", Workspace: "/evaloom/workspace", Out: "/evaloom/out",
	Inputs: "/evaloom/inputs", Records: "/evaloom/records", Scratch: "/evaloom/scratch",
}

type proposerSettings struct {
	MicroHarness string `json:"microHarness"`
	Model        string `json:"model"`
}

// ProposalServices wires Proposal Sandbox execution. Launch, when non-nil, replaces
// the container runtime (tests). Resolve supplies the spec 08 3.10 harness boundary.
type ProposalServices struct {
	Container    sandbox.ContainerConfig
	ResourceCaps ContainerConfig
	Paths        contract.RequestPaths
	Resolve      func(string) (ProposalHarness, error)
	Launch       ProposalLaunch
}

// ProposalServicesFromConfig builds services with no harness implementation until
// spec 08 3.10 wiring registers ProposalHarness factories.
func ProposalServicesFromConfig(config Config) ProposalServices {
	paths := DefaultProposalPaths
	container := sandbox.ContainerConfig{
		RuntimePath: config.Container.RuntimePath, Image: config.Container.Image,
		ProxyImage: config.Container.ProxyImage, ProxyBinary: config.Container.ProxyBinary,
		RuntimeEnv: config.Container.RuntimeEnv, StagePaths: paths,
	}
	return ProposalServices{
		Container: container, ResourceCaps: config.Container, Paths: paths,
		Resolve: func(name string) (ProposalHarness, error) {
			return nil, fmt.Errorf("runner: proposal harness %q is not wired", name)
		},
	}
}

// ExecuteProposal runs one route proposal (spec 05 3.13). It never passes
// Evaloom secrets into the sandbox and only allows the selected harness model API destinations.
func (runner *Runner) ExecuteProposal(ctx context.Context, claim ProposalClaim, services ProposalServices) error {
	reportCtx := context.WithoutCancel(ctx)
	if services.Paths == (contract.RequestPaths{}) {
		services.Paths = DefaultProposalPaths
	}
	if claim.ID == "" || claim.TimeoutSec <= 0 {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("contract_error"))
	}
	var proposer proposerSettings
	if err := json.Unmarshal(claim.Proposer, &proposer); err != nil || proposer.MicroHarness == "" || proposer.Model == "" {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("contract_error"))
	}
	if services.Resolve == nil {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("sandbox_error"))
	}
	harness, err := services.Resolve(proposer.MicroHarness)
	if err != nil {
		class := "sandbox_error"
		return reportProposalFailure(reportCtx, runner, claim.ID, &class)
	}
	stage := model.Stage("proposal")
	var outcome ProposalHarnessOutcome
	var label, reason string
	var readOK bool
	runErr := runner.WithStageSecrets(ctx, "", stage, nil, func(ctx context.Context, _ secretstore.Resolved) error {
		directory, input, roots, err := prepareProposalWorkspace(runner.dataDir, claim, services.Paths, proposer.Model)
		if err != nil {
			outcome.ErrorClass = strPtr("sandbox_error")
			return nil
		}
		defer os.RemoveAll(directory)
		limits := services.ResourceCaps.ResourceLimits(time.Duration(claim.TimeoutSec) * time.Second)
		launch, err := runner.proposalLaunch(services, harness, limits.Time, roots)
		if err != nil {
			outcome.ErrorClass = strPtr("sandbox_error")
			return nil
		}
		runCtx, cancel := context.WithTimeout(ctx, limits.Time)
		defer cancel()
		outcome, err = harness.Run(runCtx, ProposalHarnessInput{
			Claim: claim, Paths: services.Paths, Limits: limits,
			InputsHost: input, OutHost: filepath.Join(directory, "out"),
			ScratchHost: filepath.Join(directory, "scratch"), ConfigHost: filepath.Join(directory, "config"),
			Env: map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"},
		}, launch)
		if err != nil {
			if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				outcome.ErrorClass = strPtr("timeout")
			} else if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			} else if outcome.ErrorClass == nil {
				outcome.ErrorClass = strPtr("sandbox_error")
			}
		}
		if outcome.ErrorClass == nil {
			label, reason, err = readProposalOutcome(filepath.Join(directory, "out", "proposal.json"))
			readOK = err == nil
		}
		return nil
	})
	if errors.Is(runErr, context.Canceled) {
		return nil
	}
	if outcome.ErrorClass != nil {
		return reportProposalFailure(reportCtx, runner, claim.ID, outcome.ErrorClass)
	}
	if !readOK {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("contract_error"))
	}
	payload, _ := json.Marshal(struct {
		Label  string `json:"label"`
		Reason string `json:"reason"`
	}{label, reason})
	redacted := redactStageBytes(payload, []string{runner.token})
	var parsed struct {
		Label  string `json:"label"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(redacted, &parsed) != nil {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("contract_error"))
	}
	return runner.ReportProposalResult(reportCtx, claim.ID, ProposalResult{
		Label: &parsed.Label, Reason: &parsed.Reason, ErrorClass: nil,
	})
}

func (runner *Runner) proposalLaunch(services ProposalServices, harness ProposalHarness, timeout time.Duration, roots map[string]string) (ProposalLaunch, error) {
	if services.Launch != nil {
		return services.Launch, nil
	}
	container := services.Container
	container.ControlTimeout = timeout
	if container.ControlTimeout < time.Second {
		container.ControlTimeout = time.Second
	}
	container.ModelAPI = proposalDestinations{hosts: harness.ModelAPIDestinations()}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	container.HostHome = home
	container.DataDir = runner.dataDir
	container.RunnerTokenFile = runner.tokenFile
	container.StageMountRoots = roots
	box, err := sandbox.NewContainer(container)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, spec sandbox.SandboxSpec, command []string) (sandbox.Process, error) {
		return box.RunControlled(ctx, spec, command)
	}, nil
}

type proposalDestinations struct{ hosts []string }

func (d proposalDestinations) ModelAPIDestinations() []string {
	return append([]string(nil), d.hosts...)
}

func prepareProposalWorkspace(dataDir string, claim ProposalClaim, paths contract.RequestPaths, model string) (directory, inputs string, roots map[string]string, err error) {
	if _, err := ulid.ParseStrict(claim.ID); err != nil {
		return "", "", nil, err
	}
	directory = filepath.Join(dataDir, "proposals", claim.ID)
	for _, name := range []string{"profile", "workspace", "out", "inputs", "records", "scratch", "config"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			return "", "", nil, err
		}
	}
	roots = map[string]string{
		paths.Profile:   filepath.Join(directory, "profile"),
		paths.Workspace: filepath.Join(directory, "workspace"),
		paths.Out:       filepath.Join(directory, "out"),
		paths.Inputs:    filepath.Join(directory, "inputs"),
		paths.Records:   filepath.Join(directory, "records"),
		paths.Scratch:   filepath.Join(directory, "scratch"),
	}
	payload := struct {
		TaskTitle  string          `json:"taskTitle"`
		TaskBody   string          `json:"taskBody"`
		Candidates json.RawMessage `json:"candidates"`
		Trigger    string          `json:"trigger"`
		Model      string          `json:"model"`
	}{
		TaskTitle: claim.Task.Title, TaskBody: claim.Task.Body,
		Candidates: claim.Candidates, Trigger: claim.Trigger, Model: model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", nil, err
	}
	inputs = filepath.Join(directory, "inputs")
	if err := os.WriteFile(filepath.Join(inputs, "proposal-input.json"), body, 0600); err != nil {
		return "", "", nil, err
	}
	return directory, inputs, roots, nil
}

func readProposalOutcome(path string) (label, reason string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return "", "", err
	}
	return readProposalOutcomeBytes(data)
}

func readProposalOutcomeBytes(data []byte) (label, reason string, err error) {
	if len(data) > 64<<10 {
		return "", "", fmt.Errorf("runner: proposal output too large")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return "", "", fmt.Errorf("runner: invalid proposal json")
	}
	if len(fields) != 2 {
		return "", "", fmt.Errorf("runner: proposal json has extra fields")
	}
	var parsed struct {
		Label  string `json:"label"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(data, &parsed) != nil || parsed.Label == "" || parsed.Reason == "" {
		return "", "", fmt.Errorf("runner: invalid proposal json")
	}
	return parsed.Label, parsed.Reason, nil
}

func reportProposalFailure(ctx context.Context, runner *Runner, id string, class *string) error {
	if id == "" {
		return fmt.Errorf("runner: route proposal ID is required")
	}
	return runner.ReportProposalResult(ctx, id, ProposalResult{Label: nil, Reason: nil, ErrorClass: class})
}

func strPtr(value string) *string { return &value }
