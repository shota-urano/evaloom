package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type proposalAdapterHarness struct {
	implementation adapter.ProposalAdapter
	environment    *harnessenv.Environment
	spec           sandbox.SandboxSpec
	launch         ProposalLaunch
}

func newProposalAdapterHarness(services ProposalServices, name string) (ProposalHarness, error) {
	if services.Adapters == nil {
		return nil, fmt.Errorf("runner: proposal Adapter registry required")
	}
	h := &proposalAdapterHarness{}
	wiring := WorkAdapterConfig{
		CancelGrace: StageCancelGrace,
		ResolveAuthentication: func(adapter.HarnessRequest) (string, error) {
			path := services.AuthFiles[name]
			if path == "" {
				return "", fmt.Errorf("runner: missing proposal authentication")
			}
			return path, nil
		},
		ResolveConfig: func(env adapter.SandboxEnv) (*harnessenv.Environment, error) {
			if h.environment == nil || h.environment.ConfigDir() != env.ConfigDir {
				return nil, fmt.Errorf("runner: unknown proposal configuration")
			}
			return h.environment, nil
		},
		Launch: func(ctx context.Context, env adapter.SandboxEnv, command []string, stdout, stderr io.Writer) (sandbox.Process, error) {
			if env.Worktree != "" || h.launch == nil {
				return nil, fmt.Errorf("runner: proposal cannot have a workspace")
			}
			spec := h.spec
			spec.ConfigDir, spec.ConfigWritable = env.ConfigDir, true
			spec.Env = maps.Clone(h.spec.Env)
			maps.Copy(spec.Env, env.Env)
			return h.launch(ctx, spec, command, stdout, stderr)
		},
	}
	registry := services.Adapters(wiring)
	if registry == nil {
		return nil, fmt.Errorf("runner: proposal Adapter registry required")
	}
	a, err := registry.Lookup(name)
	if err != nil {
		return nil, err
	}
	var ok bool
	h.implementation, ok = a.(adapter.ProposalAdapter)
	if !ok {
		return nil, fmt.Errorf("runner: Adapter does not support proposals")
	}
	return h, nil
}

func (h *proposalAdapterHarness) ModelAPIDestinations() []string {
	return h.implementation.ModelAPIDestinations()
}

func (h *proposalAdapterHarness) Run(ctx context.Context, in ProposalHarnessInput, launch ProposalLaunch) (outcome ProposalHarnessOutcome, runErr error) {
	spec, err := BuildProposalSandboxSpec(in)
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	env, err := harnessenv.Create(ctx, harnessenv.Config{Root: in.ConfigHost,
		ConfigVariable: h.implementation.ProposalConfigVariable(), ObserveAuthentication: in.invocation.observeAuthentication})
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("config_error")}, nil
	}
	defer env.Close()
	h.environment, h.spec, h.launch = env, spec, launch
	var proposer proposerSettings
	if err := json.Unmarshal(in.Claim.Proposer, &proposer); err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("contract_error")}, nil
	}
	log, err := in.invocation.NewLog(nil)
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	// Finalize every exit, including StartProposal and Logs failures. Delivery
	// uses the job context so retries survive the CLI's execution deadline.
	defer func() {
		_ = log.Finish(false)
		if err := log.Deliver(in.deliveryCtx); err != nil {
			outcome, runErr = ProposalHarnessOutcome{}, err
		}
	}()
	id, err := h.implementation.StartProposal(ctx, adapter.ProposalRequest{Model: proposer.Model,
		InputPath: filepath.Join(in.Paths.Inputs, "proposal-input.json"), OutPath: filepath.Join(in.Paths.Out, "proposal.json")},
		adapter.SandboxEnv{ConfigDir: env.ConfigDir(), Env: env.Env()})
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("config_error")}, err
	}
	lines, err := h.implementation.Logs(id)
	if err != nil {
		_ = h.implementation.Cancel(id)
		_, _ = h.implementation.Result(id)
		return ProposalHarnessOutcome{ErrorClass: strPtr("work_failed")}, nil
	}
	done := make(chan error, 1)
	go func() {
		var readErr error
		for line := range lines {
			if err := log.Read(strings.NewReader(line.Text + "\n")); err != nil {
				readErr = err
			}
		}
		done <- readErr
	}()
	// The Adapter owns timeout/cancellation and returns only after credential
	// writeback, lock release and stopped Sandbox writers, just as for work.
	result, resultErr := h.implementation.Result(id)
	logErr := <-done
	if resultErr != nil || logErr != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	if result.Status == adapter.ResultSucceeded {
		return ProposalHarnessOutcome{}, nil
	}
	if result.ErrorClass != nil {
		switch *result.ErrorClass {
		case adapter.ErrorTimeout:
			return ProposalHarnessOutcome{ErrorClass: strPtr("timeout")}, nil
		case adapter.ErrorCancelled:
			return ProposalHarnessOutcome{}, context.Canceled
		case adapter.ErrorSandbox:
			return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
		case adapter.ErrorConfig:
			return ProposalHarnessOutcome{ErrorClass: strPtr("config_error")}, nil
		}
	}
	return ProposalHarnessOutcome{ErrorClass: strPtr("work_failed")}, nil
}
