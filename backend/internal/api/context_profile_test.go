package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestHarnessProfilesContextFileSelection(t *testing.T) {
	for _, test := range []struct {
		name, selection string
		valid           bool
	}{
		{"explicit", `contextFiles: ["prompts/developer.md", "skills/check/SKILL.md"]`, true},
		{"empty", `contextFiles: []`, true},
		{"omitted", "", true},
		{"null", `contextFiles: null`, false},
		{"scalar", `contextFiles: "prompts/developer.md"`, false},
		{"wrong element type", `contextFiles: [42]`, false},
		{"duplicate", `contextFiles: ["prompt.md", "prompt.md"]`, false},
		{"absolute", `contextFiles: ["/prompt.md"]`, false},
		{"traversal", `contextFiles: ["../prompt.md"]`, false},
		{"unnormalized", `contextFiles: ["sub/../prompt.md"]`, false},
		{"empty path", `contextFiles: [""]`, false},
		{"directory dot", `contextFiles: ["."]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			commitProfile(t, repo, "1.2.0", validProfileYAML+test.selection+"\n", false)
			status, body := postProfile(t, f, "1.2.0")
			if !test.valid {
				assertAuthError(t, status, body, 422, PreconditionFailed)
				if projectCount(t, f, "harness_profiles") != 0 {
					t.Fatal("invalid selection saved")
				}
				return
			}
			profile := requireProfile(t, status, body, 201)
			var definition model.ProfileDefinition
			if err := json.Unmarshal(profile.Definition, &definition); err != nil {
				t.Fatal(err)
			}
			if test.name == "explicit" && !reflect.DeepEqual(definition.ContextFiles, []string{"prompts/developer.md", "skills/check/SKILL.md"}) {
				t.Fatalf("selection changed during registration: %s", profile.Definition)
			}
			if profile.DefinitionHash != contextHash(string(profile.Definition)) {
				t.Fatal("selection was not covered by definition hash")
			}
		})
	}
}
