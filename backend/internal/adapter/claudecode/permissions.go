package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// CheckToolPermissions is shared by Profile registration and claim-time launch
// (spec 08 3.9, Q145). CLI rule strings are passed through unchanged.
func (*harness) CheckToolPermissions(raw json.RawMessage) error {
	_, _, err := permissionRules(raw)
	return err
}

func permissionRules(raw json.RawMessage) ([]string, []string, error) {
	var permissions map[string]json.RawMessage
	if json.Unmarshal(raw, &permissions) != nil || permissions == nil {
		return nil, nil, fmt.Errorf("toolPermissions: expected object")
	}
	allow, deny := []string{}, []string{}
	for key, value := range permissions {
		if key != "allow" && key != "deny" {
			return nil, nil, fmt.Errorf("toolPermissions: unknown key")
		}
		rules, err := permissionArray(value)
		if err != nil {
			return nil, nil, err
		}
		if key == "allow" {
			allow = rules
		} else {
			deny = rules
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

func (h *harness) configurePermissions(ctx context.Context, req adapter.HarnessRequest) (json.RawMessage, error) {
	raw, err := h.config.ResolveToolPermissions(ctx, req)
	if err != nil {
		return nil, err
	}
	allow, deny, err := permissionRules(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"permissions": map[string][]string{"allow": allow, "deny": deny}})
}
