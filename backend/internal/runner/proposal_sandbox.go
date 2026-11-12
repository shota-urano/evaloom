package runner

import (
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// BuildProposalSandboxSpec assembles the Proposal Sandbox view (spec 09 3.1).
// Secrets must stay empty; callers attach only the selected model API via launch wiring.
func BuildProposalSandboxSpec(in ProposalHarnessInput) (sandbox.SandboxSpec, error) {
	if in.InputsHost == "" || in.OutHost == "" || in.ScratchHost == "" {
		return sandbox.SandboxSpec{}, fmt.Errorf("runner: proposal mount roots are required")
	}
	return sandbox.SandboxSpec{
		Kind: sandbox.Proposal, Network: sandbox.NetworkHarness, Limits: in.Limits, Env: in.Env,
		Mounts: []sandbox.Mount{
			{HostPath: in.InputsHost, GuestPath: in.Paths.Inputs, Writable: false},
			{HostPath: in.OutHost, GuestPath: in.Paths.Out, Writable: true},
			{HostPath: in.ScratchHost, GuestPath: in.Paths.Scratch, Writable: true},
		},
	}, nil
}
