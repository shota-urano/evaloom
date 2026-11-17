package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/contract"
)

// Exercise the registration HTTP handler, committed Profile reader, concrete
// Adapter conversion and persistence together. Neither Adapter is replaced by
// the permissive profileTestAdapter used by general Registry tests.
func TestAdapterProfileToolPermissions(t *testing.T) {
	for _, implementation := range []struct {
		name string
		new  func() adapter.Adapter
	}{
		{"codex", func() adapter.Adapter { return codex.New(codex.Config{}) }},
		{"claude_code", func() adapter.Adapter { return claudecode.New(claudecode.Config{}) }},
	} {
		t.Run(implementation.name, func(t *testing.T) {
			testAdapterProfileToolPermissions(t, implementation.name, implementation.new)
		})
	}
}

func testAdapterProfileToolPermissions(t *testing.T, name string, newAdapter func() adapter.Adapter) {
	t.Helper()
	for _, tc := range []struct {
		name        string
		permissions any  // nil omits the optional field; a typed nil represents JSON null.
		accepted    bool // Nonempty rules are accepted only by Claude Code.
	}{
		{"omitted", nil, true},
		{"empty", map[string]any{}, true},
		{"empty_allow", map[string]any{"allow": []string{}}, true},
		{"empty_deny", map[string]any{"deny": []string{}}, true},
		{"empty_rules", map[string]any{"allow": []string{}, "deny": []string{}}, true},
		{"allow", map[string]any{"allow": []string{"Bash(git commit:*)"}}, name == "claude_code"},
		{"deny", map[string]any{"deny": []string{"WebFetch"}}, name == "claude_code"},
		{"both", map[string]any{"allow": []string{"Read", "Edit"}, "deny": []string{"Bash(git push:*)"}}, name == "claude_code"},
		{"unknown", map[string]any{"unknown": []string{}}, false},
		{"unknown_with_rules", map[string]any{"allow": []string{"Read"}, "extra": false}, false},
		{"case_sensitive_key", map[string]any{"Allow": []string{}}, false},
		{"null", map[string]any(nil), false},
		{"array", []string{}, false},
		{"allow_string", map[string]any{"allow": "Read"}, false},
		{"deny_object", map[string]any{"deny": map[string]any{}}, false},
		{"allow_null", map[string]any{"allow": nil}, false},
		{"deny_null", map[string]any{"deny": nil}, false},
		{"allow_empty_string", map[string]any{"allow": []string{"Read", ""}}, false},
		{"deny_empty_string", map[string]any{"deny": []string{""}}, false},
		{"allow_number", map[string]any{"allow": []any{1}}, false},
		{"deny_null_element", map[string]any{"deny": []any{nil}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := projectFixture(t)
			if err := f.adapters.Register(name, newAdapter()); err != nil {
				t.Fatal(err)
			}
			status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
			h := requireHarness(t, status, body, 201)
			v := contractExampleProfile(t)
			work := field(v, "stages", "work")
			work["microHarness"] = name
			if tc.permissions == nil {
				delete(work, "toolPermissions")
			} else {
				work["toolPermissions"] = tc.permissions
			}
			commitProfile(t, h.RepoPath, v["profileVersion"].(string), yamlOf(t, v), false)
			status, body = postProfile(t, f, v["profileVersion"].(string))
			if tc.accepted {
				profile := requireProfile(t, status, body, 201)
				var definition contract.ProfileDefinition
				if err := json.Unmarshal(profile.Definition, &definition); err != nil {
					t.Fatal(err)
				}
				var saved any
				if len(definition.Stages.Work.ToolPermissions) > 0 {
					if err := json.Unmarshal(definition.Stages.Work.ToolPermissions, &saved); err != nil {
						t.Fatal(err)
					}
				}
				// Compare JSON values so []string and decoded []any are equivalent.
				raw, err := json.Marshal(tc.permissions)
				if err != nil {
					t.Fatal(err)
				}
				var expected any
				if err := json.Unmarshal(raw, &expected); err != nil {
					t.Fatal(err)
				}
				if definition.Stages.Work.MicroHarness != name || !reflect.DeepEqual(saved, expected) {
					t.Fatalf("stored work configuration changed permissions: %+v", definition.Stages.Work)
				}
			} else {
				reason := "unconvertible"
				if tc.name == "null" {
					reason = "must_not_be_null"
				} else if tc.name == "array" {
					reason = "expected_object"
				}
				requireProjectError(t, status, body, 422, PreconditionFailed, "stages.work.toolPermissions", reason)
				if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
					t.Fatal("invalid permissions persisted a Profile or registration audit")
				}
			}
		})
	}
}
