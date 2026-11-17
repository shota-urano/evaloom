package codex

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func (*harness) ProposalConfigVariable() string { return "CODEX_HOME" }

// Codex has no per-tool permissions. Proposal Sandbox supplies the filesystem
// and network restrictions, as for work (spec 08 3.10).
func (h *harness) StartProposal(ctx context.Context, req adapter.ProposalRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := req.Validate(env); err != nil {
		return "", err
	}
	return h.start(ctx, adapter.HarnessRequest{Profile: adapter.RequestProfile{ModelID: req.Model}}, env, &req)
}
