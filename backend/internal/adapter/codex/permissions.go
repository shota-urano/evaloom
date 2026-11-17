package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// CheckToolPermissions is shared by Profile registration and claim-time launch
// (spec 08 3.9, Q145). Codex 0.163.0 cannot represent per-tool rules.
func (*harness) CheckToolPermissions(raw json.RawMessage) error {
	var permissions map[string]json.RawMessage
	if json.Unmarshal(raw, &permissions) != nil || permissions == nil {
		return fmt.Errorf("toolPermissions: expected object")
	}
	for key, value := range permissions {
		if key != "allow" && key != "deny" {
			return fmt.Errorf("toolPermissions: unknown key")
		}
		rules, err := permissionArray(value)
		if err != nil {
			return err
		}
		if len(rules) != 0 {
			return fmt.Errorf("codex: per-tool permissions are unsupported")
		}
	}
	return nil
}

func permissionArray(raw json.RawMessage) ([]string, error) {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, fmt.Errorf("permissions: expected string array")
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value == "" {
			return nil, fmt.Errorf("permissions: expected nonempty string")
		}
		result = append(result, value)
	}
	return result, nil
}

func (h *harness) configurePermissions(ctx context.Context, req adapter.HarnessRequest) error {
	raw, err := h.config.ResolveToolPermissions(ctx, req)
	if err != nil {
		return err
	}
	return h.CheckToolPermissions(raw)
}
