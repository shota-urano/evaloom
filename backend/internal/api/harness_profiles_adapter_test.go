package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/contract"
)

func TestProfileRegistrationUsesAdapterRegistry(t *testing.T) {
	for _, name := range []string{"codex", "claude_code", "extension_test"} {
		t.Run(name, func(t *testing.T) {
			f := projectFixture(t)
			status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
			h := requireHarness(t, status, body, 201)
			commitProfile(t, h.RepoPath, "1.2.0", strings.ReplaceAll(validProfileYAML, `microHarness: "codex"`, `microHarness: "`+name+`"`), false)
			for _, unrelated := range []bool{false, true} {
				if unrelated {
					if err := f.adapters.Register("unrelated", dummy.New()); err != nil {
						t.Fatal(err)
					}
				}
				status, body = postProfile(t, f, "1.2.0")
				requireProjectError(t, status, body, 422, PreconditionFailed, "stages.work.microHarness", "unsupported")
				if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
					t.Fatal("rejected Profile changed stored profiles or audit logs")
				}
			}
			// Adding the implementation is the only change needed for the same
			// pinned Profile to become registrable, including an extension name.
			if err := f.adapters.Register(name, dummy.New()); err != nil {
				t.Fatal(err)
			}
			status, body = postProfile(t, f, "1.2.0")
			profile := requireProfile(t, status, body, 201)
			var def contract.ProfileDefinition
			if err := json.Unmarshal(profile.Definition, &def); err != nil || def.Stages.Work.MicroHarness != name {
				t.Fatalf("registered Profile = %s, error = %v", profile.Definition, err)
			}
			if projectCount(t, f, "harness_profiles") != 1 {
				t.Fatal("registered Profile was not persisted")
			}
		})
	}
}
