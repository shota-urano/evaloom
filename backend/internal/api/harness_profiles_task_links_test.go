package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
)

// contract 5.2 stages.prepare.network.taskLinks (spec 03 3.4, Q233): a boolean
// that only prepare can declare.
func TestHarnessProfilesAcceptTaskLinksInPrepare(t *testing.T) {
	for _, tc := range []struct {
		name    string
		network map[string]any
		want    bool
	}{
		{"with allow", map[string]any{"allow": []any{"github.com:443"}, "taskLinks": true}, true},
		{"without allow", map[string]any{"taskLinks": true}, true},
		{"false", map[string]any{"allow": []any{"github.com:443"}, "taskLinks": false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			document := contractExampleProfile(t)
			field(document, "stages", "prepare")["network"] = tc.network
			commitProfile(t, repo, "1.2.0", yamlOf(t, document), false)
			status, body := postProfile(t, f, "1.2.0")
			p := requireProfile(t, status, body, 201)
			var def contract.ProfileDefinition
			if err := json.Unmarshal(p.Definition, &def); err != nil {
				t.Fatal(err)
			}
			got := def.Stages.Prepare.Network.TaskLinks
			if got == nil || *got != tc.want {
				t.Fatalf("stored taskLinks=%v; want %v", got, tc.want)
			}
		})
	}
}

func TestHarnessProfilesRejectTaskLinksOutsidePrepare(t *testing.T) {
	for _, tc := range []struct {
		name, path, reason string
		edit               func(map[string]any)
	}{
		{"work", "stages.work.network", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "work")["network"] = map[string]any{"taskLinks": true}
		}},
		{"check", "stages.check.network", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "check")["network"] = map[string]any{"taskLinks": true}
		}},
		{"publish", "stages.publish.network.taskLinks", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "publish", "network")["taskLinks"] = true
		}},
		{"publish false", "stages.publish.network.taskLinks", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "publish", "network")["taskLinks"] = false
		}},
		{"not a boolean", "stages.prepare.network.taskLinks", "expected_boolean", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["taskLinks"] = "yes"
		}},
		{"empty network", "stages.prepare.network.allow", "required", func(v map[string]any) {
			field(v, "stages", "prepare")["network"] = map[string]any{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			document := contractExampleProfile(t)
			tc.edit(document)
			commitProfile(t, repo, "1.2.0", yamlOf(t, document), false)
			status, body := postProfile(t, f, "1.2.0")
			details := profileDetails(t, status, body) // requires 422
			if !reflect.DeepEqual(details, []Detail{{Field: tc.path, Reason: tc.reason}}) {
				t.Fatalf("details=%+v; want %s %s", details, tc.path, tc.reason)
			}
			if projectCount(t, f, "harness_profiles") != 0 {
				t.Fatal("rejected Profile was persisted")
			}
		})
	}
}
