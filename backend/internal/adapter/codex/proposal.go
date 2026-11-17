package codex

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func (*harness) ProposalConfigVariable() string { return "CODEX_HOME" }

// Codex 0.163.0 has no per-tool permissions (spec 08 3.9). Do not silently
// grant command execution to a proposal, which requires file tools only.
func (*harness) StartProposal(ctx context.Context, req adapter.ProposalRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := req.Validate(env); err != nil {
		return "", err
	}
	return "", adapter.ErrProposalPermissionsUnsupported
}
