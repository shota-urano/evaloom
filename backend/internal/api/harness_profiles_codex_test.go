package api

import (
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
)

func TestCodexProfileToolPermissions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions map[string]any
		accepted    bool
	}{
		{"empty", map[string]any{}, true},
		{"empty_rules", map[string]any{"allow": []string{}, "deny": []string{}}, true},
		{"allow", map[string]any{"allow": []string{"Bash"}}, false},
		{"deny", map[string]any{"deny": []string{"Bash"}}, false},
		{"unknown", map[string]any{"unknown": []string{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := projectFixture(t)
			if err := f.adapters.Register("codex", codex.New(codex.Config{})); err != nil {
				t.Fatal(err)
			}
			status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
			h := requireHarness(t, status, body, 201)
			v := contractExampleProfile(t)
			work := field(v, "stages", "work")
			work["microHarness"], work["toolPermissions"] = "codex", tc.permissions
			commitProfile(t, h.RepoPath, v["profileVersion"].(string), yamlOf(t, v), false)
			status, body = postProfile(t, f, v["profileVersion"].(string))
			if tc.accepted {
				requireProfile(t, status, body, 201)
			} else {
				requireProjectError(t, status, body, 422, PreconditionFailed, "stages.work.toolPermissions", "unconvertible")
				if projectCount(t, f, "harness_profiles") != 0 {
					t.Fatal("invalid Codex permissions persisted")
				}
			}
		})
	}
}
