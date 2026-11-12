package runner

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// ProposalLaunch runs one process inside Proposal Sandbox. Production wiring
// uses Container.RunControlled; tests inject a recorder or fake runtime.
type ProposalLaunch func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error)

// ProposalHarnessInput is the Runner-owned Proposal Sandbox layout (spec 09 3.1).
// The concrete Adapter fills prompts and commands (spec 08 3.10).
type ProposalHarnessInput struct {
	Claim                                        ProposalClaim
	Paths                                        contract.RequestPaths
	InputsHost, OutHost, ScratchHost, ConfigHost string
	Env                                          map[string]string
	Limits                                       sandbox.ResourceLimits
}

// ProposalHarnessOutcome is the harness result before Runner redaction and API delivery.
type ProposalHarnessOutcome struct {
	ProposalJSON []byte
	ErrorClass   *string
}

// ProposalHarness is the Runner boundary for spec 08 3.10. Adapter packages
// implement this in a separate task; Runner must not embed CLI details here.
type ProposalHarness interface {
	ModelAPIDestinations() []string
	Run(context.Context, ProposalHarnessInput, ProposalLaunch) (ProposalHarnessOutcome, error)
}
