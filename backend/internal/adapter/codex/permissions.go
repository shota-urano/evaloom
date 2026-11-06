package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// permissionRules validates spec 08, section 3.8 without inventing tool names.
func (h *harness) permissionRules(ctx context.Context, req adapter.HarnessRequest) ([]string, []string, error) {
	raw, err := h.config.ResolveToolPermissions(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var permissions map[string]json.RawMessage
	if json.Unmarshal(raw, &permissions) != nil || permissions == nil {
		return nil, nil, fmt.Errorf("toolPermissions: expected object")
	}
	deny := []string{}
	for key, value := range permissions {
		if key != "deny" {
			return nil, nil, fmt.Errorf("toolPermissions: unknown key")
		}
		deny, err = permissionArray(value)
		if err != nil {
			return nil, nil, err
		}
	}
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(req.Profile.Definition, &definition); err != nil {
		return nil, nil, err
	}
	allow := []string{}
	if raw, ok := definition["toolConfiguration"]; ok {
		var tools map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) != nil || tools == nil {
			return nil, nil, fmt.Errorf("toolConfiguration: expected object")
		}
		if raw, ok := tools["allowedTools"]; ok {
			allow, err = permissionArray(raw)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return allow, deny, nil
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
	allow, deny, err := h.permissionRules(ctx, req)
	if err != nil {
		return err
	}
	if len(allow) != 0 || len(deny) != 0 {
		return fmt.Errorf("codex: per-tool permissions are unsupported")
	}
	return nil
}
