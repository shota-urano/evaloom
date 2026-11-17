package claudecode

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func (*harness) ProposalConfigVariable() string { return "CLAUDE_CONFIG_DIR" }

func (h *harness) StartProposal(ctx context.Context, req adapter.ProposalRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := req.Validate(env); err != nil {
		return "", err
	}
	// Only the model is shared with work's internal command supervisor. No
	// Execution, workspace, Profile, or work instructions are manufactured.
	return h.start(ctx, adapter.HarnessRequest{Profile: adapter.RequestProfile{ModelID: req.Model}}, env, &req)
}

func proposalPermissions(req adapter.ProposalRequest) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"permissions": map[string][]string{
		"allow": {"Read", "Edit(//" + strings.TrimPrefix(filepath.ToSlash(req.OutPath), "/") + ")"},
	}})
}
