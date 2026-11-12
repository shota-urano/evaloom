package api

import (
	"fmt"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
)

// Q146 must use every registered Adapter, including one not selected by the
// Profile. Exercise registration with the real Adapter declarations.
func TestProfilesRejectRegisteredAdapterModelAPIs(t *testing.T) {
	f := projectFixture(t)
	if err := f.adapters.Register("codex", codex.New(codex.Config{})); err != nil {
		t.Fatal(err)
	}
	if err := f.adapters.Register("claude_code", claudecode.New(claudecode.Config{})); err != nil {
		t.Fatal(err)
	}
	status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
	repo := requireHarness(t, status, body, 201).RepoPath
	version := 0
	for _, stage := range []string{"prepare", "publish"} {
		for _, destination := range []string{"chatgpt.com:443", "auth.openai.com:443", "api.openai.com:443", "api.anthropic.com:443", "console.anthropic.com:443", "claude.ai:443"} {
			t.Run(stage+"/"+destination, func(t *testing.T) {
				v := contractExampleProfile(t)
				// The example selects Claude Code, while the denylist includes
				// Codex's model API and authentication-refresh destinations too.
				field(v, "stages", "work")["toolPermissions"] = map[string]any{}
				field(v, "stages", stage)["network"] = map[string]any{"allow": []string{destination}}
				v["profileVersion"] = fmt.Sprintf("1.2.%d", version)
				commitProfile(t, repo, v["profileVersion"].(string), yamlOf(t, v), false)
				status, body := postProfile(t, f, v["profileVersion"].(string))
				details := profileDetails(t, status, body)
				want := Detail{Field: "stages." + stage + ".network.allow[0]", Reason: "model_api"}
				found := false
				for _, detail := range details {
					if detail == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("model destination accepted or misclassified: %s", body)
				}
			})
			version++
		}
	}
}
