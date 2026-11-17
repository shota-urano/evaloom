package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
	"golang.org/x/sys/unix"
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
	Adapters     func(WorkAdapterConfig) *adapter.Registry
	AuthFiles    map[string]string
	Resolve      func(string) (ProposalHarness, error)
	Launch       ProposalLaunch
}

// ProposalServicesFromConfig builds proposal services. The command supplies
// the same Adapter registry factory as work execution.
func ProposalServicesFromConfig(config Config) ProposalServices {
	paths := DefaultProposalPaths
	container := sandbox.ContainerConfig{
		RuntimePath: config.Container.RuntimePath, Image: config.Container.Image,
		ProxyImage: config.Container.ProxyImage, ProxyBinary: config.Container.ProxyBinary,
		RuntimeEnv: config.Container.RuntimeEnv, StagePaths: paths,
	}
	return ProposalServices{
		Container: container, ResourceCaps: config.Container, Paths: paths, AuthFiles: config.MicroHarnessAuthFiles,
	}
}

// ExecuteProposal runs one route proposal (spec 05 3.13). It never passes
// Evaloom secrets into the sandbox and only allows the selected harness model API destinations.
func (runner *Runner) ExecuteProposal(ctx context.Context, claim ProposalClaim, services ProposalServices) error {
	// Reporting survives the proposal's work timeout, but must stop on job
	// cancellation or Runner shutdown, including while retrying artifacts.
	reportCtx := ctx
	if services.Paths == (contract.RequestPaths{}) {
		services.Paths = DefaultProposalPaths
	}
	if claim.ID == "" || claim.TimeoutSec <= 0 || claim.TimeoutSec > int(time.Duration(1<<63-1)/time.Second) {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("contract_error"))
	}
	var proposer proposerSettings
	if err := json.Unmarshal(claim.Proposer, &proposer); err != nil || proposer.MicroHarness == "" || proposer.Model == "" {
		return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("contract_error"))
	}
	resolve := services.Resolve
	if resolve == nil {
		resolve = func(name string) (ProposalHarness, error) { return newProposalAdapterHarness(services, name) }
	}
	harness, err := resolve(proposer.MicroHarness)
	if err != nil {
		if services.Resolve == nil {
			return reportProposalFailure(reportCtx, runner, claim.ID, strPtr("config_error"))
		}
		class := "sandbox_error"
		return reportProposalFailure(reportCtx, runner, claim.ID, &class)
	}
	stage := model.Stage("proposal")
	var outcome ProposalHarnessOutcome
	var label, reason string
	var readOK bool
	runErr := runner.WithStageSecrets(ctx, "", stage, nil, func(ctx context.Context, keys secretstore.Resolved) error {
		security := &stageSecurity{runner: runner, id: claim.ID, resolved: keys, active: true}
		defer func() { security.active = false; clear(security.credentials) }()
		invocation := StageInvocation{Stage: stage, security: security}
		directory, input, roots, err := prepareProposalFiles(runner.dataDir, claim, services.Paths)
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
			Claim: claim, Paths: services.Paths, Limits: limits, invocation: invocation,
			InputsHost: input, OutHost: filepath.Join(directory, "out"),
			ScratchHost: filepath.Join(directory, "scratch"), ConfigHost: filepath.Join(directory, "config"),
			Env: map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8"},
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
			raw, readErr := readProposalFile(filepath.Join(directory, "out", "proposal.json"))
			if readErr == nil {
				artifact, collectErr := invocation.CollectArtifact("proposal_result", "proposal.json", bytes.NewReader(raw))
				if collectErr != nil {
					return collectErr
				}
				if deliverErr := runner.deliverArtifact(reportCtx, artifact); deliverErr != nil {
					return deliverErr
				}
				// Assemble only from the sanitized copy of the actual out file.
				sanitized, readErr := os.ReadFile(artifact.Path)
				if readErr != nil {
					return readErr
				}
				_, _, rawErr := readProposalOutcomeBytes(raw)
				label, reason, err = readProposalOutcomeBytes(sanitized)
				readOK = rawErr == nil && err == nil
			}
		}
		return nil
	})
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
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
	container.ConfigDirRoot = filepath.Join(filepath.Dir(roots[services.Paths.Out]), "config")
	_, err = sandbox.NewContainer(container)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, spec sandbox.SandboxSpec, command []string, stdout, stderr io.Writer) (sandbox.Process, error) {
		local := container
		local.Stdout, local.Stderr = stdout, stderr
		box, err := sandbox.NewContainer(local)
		if err != nil {
			return nil, err
		}
		return box.RunControlled(ctx, spec, command)
	}, nil
}

type proposalDestinations struct{ hosts []string }

func (d proposalDestinations) ModelAPIDestinations() []string {
	return append([]string(nil), d.hosts...)
}

func prepareProposalFiles(dataDir string, claim ProposalClaim, paths contract.RequestPaths) (directory, inputs string, roots map[string]string, err error) {
	if _, err := ulid.ParseStrict(claim.ID); err != nil {
		return "", "", nil, err
	}
	directory = filepath.Join(dataDir, "proposals", claim.ID)
	for _, name := range []string{"out", "inputs", "scratch", "config"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			return "", "", nil, err
		}
	}
	roots = map[string]string{
		paths.Out:     filepath.Join(directory, "out"),
		paths.Inputs:  filepath.Join(directory, "inputs"),
		paths.Scratch: filepath.Join(directory, "scratch"),
	}
	payload := struct {
		TaskTitle  string          `json:"taskTitle"`
		TaskBody   string          `json:"taskBody"`
		Candidates json.RawMessage `json:"candidates"`
	}{
		TaskTitle: claim.Task.Title, TaskBody: claim.Task.Body,
		Candidates: claim.Candidates,
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

func readProposalFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("runner: proposal output must be a regular file up to 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return nil, err
	}
	return data, nil
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
	if json.Unmarshal(data, &parsed) != nil || parsed.Label == "" || parsed.Reason == "" || utf8.RuneCountInString(parsed.Reason) > 2000 {
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
