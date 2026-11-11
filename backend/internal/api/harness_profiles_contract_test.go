package api

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"gopkg.in/yaml.v3"
)

// contractExampleProfile reads the contract 5.1 example from the source of
// truth. Angle-bracket placeholders stand for real values.
func contractExampleProfile(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../../docs/contract/harness-contract-v1.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(data), "### 5.1 ")
	_, block, found := strings.Cut(section, "```yaml\n")
	if !found {
		t.Fatal("missing contract 5.1 example")
	}
	block, _, _ = strings.Cut(block, "```")
	block = strings.NewReplacer(
		"<ダイジェスト付きのイメージ>", "example/harness@sha256:"+strings.Repeat("a", 64),
		"<正式な Model ID>", "claude-test-model",
	).Replace(block)
	var document map[string]any
	if err := yaml.Unmarshal([]byte(block), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func yamlOf(t *testing.T, document map[string]any) string {
	t.Helper()
	data, err := json.Marshal(document) // JSON is YAML.
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func field(value any, keys ...string) map[string]any {
	object := value.(map[string]any)
	for _, key := range keys {
		object = object[key].(map[string]any)
	}
	return object
}

func profileDetails(t *testing.T, status int, body []byte) []Detail {
	t.Helper()
	requireProjectError(t, status, body, 422, PreconditionFailed, "", "")
	var envelope struct{ Error Error }
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Error.Details
}

func TestHarnessProfilesRegisterContractExample(t *testing.T) {
	f, repo := profileFixture(t)
	sha := commitProfile(t, repo, "1.2.0", yamlOf(t, contractExampleProfile(t)), false)
	status, body := postProfile(t, f, "1.2.0")
	p := requireProfile(t, status, body, 201)
	if p.CommitSHA != sha || p.TagName != "profile/v1.2.0" || p.ContractVersion != "1" || p.OperationStatus != "provisional" || p.ModelID != "claude-test-model" {
		t.Fatalf("registered=%+v", p)
	}
	var stored, want map[string]any
	if err := json.Unmarshal(p.Definition, &stored); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(yamlOf(t, contractExampleProfile(t))), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("definition is not the full text: %s", p.Definition)
	}
	var def contract.ProfileDefinition
	if err := json.Unmarshal(p.Definition, &def); err != nil {
		t.Fatal(err)
	}
}

// Each rule of spec 03 3.4 is broken once; each fails with its own path.
func TestHarnessProfilesRejectEachContractRule(t *testing.T) {
	for _, tc := range []struct {
		name, path, reason string
		edit               func(map[string]any)
	}{
		{"unknown field", "extra", "unknown_field", func(v map[string]any) { v["extra"] = true }},
		{"unknown nested field", "stages.check.env", "unknown_field", func(v map[string]any) { field(v, "stages", "check")["env"] = "x" }},
		{"unsupported contract version", "contractVersion", "unsupported", func(v map[string]any) { v["contractVersion"] = "2" }},
		{"version differs from tag", "profileVersion", "differs_from_tag", func(v map[string]any) { v["profileVersion"] = "1.2.1" }},
		{"missing name", "name", "required", func(v map[string]any) { delete(v, "name") }},
		{"description too long", "description", "too_long", func(v map[string]any) { v["description"] = strings.Repeat("あ", 2001) }},
		{"acceptance criteria", "acceptanceCriteria", "invalid", func(v map[string]any) { v["acceptanceCriteria"] = "maybe" }},
		{"missing publish", "stages.publish", "required", func(v map[string]any) { delete(field(v, "stages"), "publish") }},
		{"work network.allow", "stages.work.network", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "work")["network"] = map[string]any{"allow": []any{"github.com:443"}}
		}},
		{"work secrets", "stages.work.secrets", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "work")["secrets"] = []any{map[string]any{"slot": "key", "access": "read"}}
		}},
		{"work runtime", "stages.work.runtime", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "work")["runtime"] = map[string]any{"kind": "container", "image": "x@sha256:" + strings.Repeat("a", 64)}
		}},
		{"check network.allow", "stages.check.network", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "check")["network"] = map[string]any{"allow": []any{"github.com:443"}}
		}},
		{"check secrets", "stages.check.secrets", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "check")["secrets"] = []any{map[string]any{"slot": "key", "access": "read"}}
		}},
		{"prepare write access", "stages.prepare.secrets[0].access", "not_allowed_in_stage", func(v map[string]any) {
			field(v, "stages", "prepare")["secrets"] = []any{map[string]any{"slot": "github_read", "access": "write"}}
		}},
		{"invalid slot", "stages.publish.secrets[0].slot", "invalid_format", func(v map[string]any) {
			field(v, "stages", "publish")["secrets"] = []any{map[string]any{"slot": "GitHub-Write", "access": "write"}}
		}},
		{"duplicate slot", "stages.publish.secrets[1].slot", "duplicate", func(v map[string]any) {
			field(v, "stages", "publish")["secrets"] = []any{map[string]any{"slot": "key", "access": "read"}, map[string]any{"slot": "key", "access": "write"}}
		}},
		{"IP literal destination", "stages.prepare.network.allow[0]", "ip_literal", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"192.0.2.1:443"}
		}},
		{"IPv6 literal destination", "stages.publish.network.allow[0]", "ip_literal", func(v map[string]any) {
			field(v, "stages", "publish", "network")["allow"] = []any{"[2001:db8::1]:443"}
		}},
		{"wildcard destination", "stages.prepare.network.allow[0]", "wildcard", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"*.github.com:443"}
		}},
		{"local destination", "stages.prepare.network.allow[0]", "local_name", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"build.internal:443"}
		}},
		{"single label destination", "stages.prepare.network.allow[0]", "local_name", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"intranet:443"}
		}},
		{"uppercase destination", "stages.prepare.network.allow[0]", "invalid_format", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"GitHub.com:443"}
		}},
		{"invalid port", "stages.prepare.network.allow[0]", "invalid_port", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"github.com:65536"}
		}},
		{"duplicate destination", "stages.publish.network.allow[1]", "duplicate", func(v map[string]any) {
			field(v, "stages", "publish", "network")["allow"] = []any{"github.com:443", "github.com:443"}
		}},
		{"model API destination in publish", "stages.publish.network.allow[1]", "model_api", func(v map[string]any) {
			field(v, "stages", "publish", "network")["allow"] = []any{"github.com:443", "api.openai.com:443"}
		}},
		{"model API destination in prepare", "stages.prepare.network.allow[0]", "model_api", func(v map[string]any) {
			field(v, "stages", "prepare", "network")["allow"] = []any{"api.anthropic.com:443"}
		}},
		{"timeout over limit", "stages.work.timeoutSec", "exceeds_limit", func(v map[string]any) { field(v, "stages", "work")["timeoutSec"] = 14401 }},
		{"publish timeout over limit", "stages.publish.timeoutSec", "exceeds_limit", func(v map[string]any) { field(v, "stages", "publish")["timeoutSec"] = 1801 }},
		{"zero timeout", "stages.check.timeoutSec", "invalid", func(v map[string]any) { field(v, "stages", "check")["timeoutSec"] = 0 }},
		{"fractional timeout", "stages.check.timeoutSec", "expected_integer", func(v map[string]any) { field(v, "stages", "check")["timeoutSec"] = 1.5 }},
		{"path outside repository", "stages.work.skills[0]", "outside_repository", func(v map[string]any) { field(v, "stages", "work")["skills"] = []any{"evaloom/../../outside"} }},
		{"absolute command", "stages.check.command[0]", "outside_repository", func(v map[string]any) { field(v, "stages", "check")["command"] = []any{"/bin/sh"} }},
		{"missing referenced file", "stages.publish.command[0]", "not_found", func(v map[string]any) { field(v, "stages", "publish")["command"] = []any{"evaloom/publish/missing"} }},
		{"symbolic link reference", "stages.work.settings[0]", "symlink", func(v map[string]any) { field(v, "stages", "work")["settings"] = []any{"evaloom/work/link.json"} }},
		{"directory with symbolic link", "stages.work.skills[0]", "contains_symlink", func(v map[string]any) { field(v, "stages", "work")["skills"] = []any{"evaloom/work"} }},
		{"check command inside work files", "stages.check.command[0]", "inside_work_files", func(v map[string]any) {
			field(v, "stages", "check")["command"] = []any{"evaloom/work/skills/check.sh"}
		}},
		{"empty command", "stages.check.command", "required", func(v map[string]any) { field(v, "stages", "check")["command"] = []any{} }},
		{"unregistered micro-harness", "stages.work.microHarness", "unsupported", func(v map[string]any) { field(v, "stages", "work")["microHarness"] = "gemini" }},
		{"empty model", "stages.work.model", "required", func(v map[string]any) { field(v, "stages", "work")["model"] = "" }},
		{"tool permissions the Adapter cannot convert", "stages.work.toolPermissions", "unconvertible", func(v map[string]any) {
			field(v, "stages", "work")["microHarness"] = "codex"
		}},
		{"tool permissions unknown key", "stages.work.toolPermissions", "unconvertible", func(v map[string]any) {
			field(v, "stages", "work")["toolPermissions"] = map[string]any{"ask": []any{"Read"}}
		}},
		{"resume", "stages.work.resume", "invalid", func(v map[string]any) { field(v, "stages", "work")["resume"] = "never" }},
		{"mutable container image", "stages.prepare.runtime.image", "digest_required", func(v map[string]any) {
			field(v, "stages", "prepare", "runtime")["image"] = "example:latest"
		}},
		{"macOS VM before its implementation", "stages.prepare.runtime.kind", "unsupported", func(v map[string]any) {
			field(v, "stages", "prepare")["runtime"] = map[string]any{"kind": "macos_vm", "image": "macos-xcode"}
		}},
		{"params schema is not JSON Schema", "params.schema", "invalid_schema", func(v map[string]any) { field(v, "params")["schema"] = "evaloom/broken.schema.json" }},
		{"params schema missing", "params.schema", "not_found", func(v map[string]any) { field(v, "params")["schema"] = "evaloom/missing.json" }},
		{"invalid approval operation", "approvals[0].operation", "invalid_format", func(v map[string]any) {
			v["approvals"].([]any)[0].(map[string]any)["operation"] = "Merge-PR"
		}},
		{"duplicate approval operation", "approvals[1].operation", "duplicate", func(v map[string]any) {
			v["approvals"] = append(v["approvals"].([]any), v["approvals"].([]any)[0])
		}},
		{"non-boolean waivable", "approvals[0].waivable", "expected_boolean", func(v map[string]any) {
			v["approvals"].([]any)[0].(map[string]any)["waivable"] = "yes"
		}},
		{"records from work", "records.keep[0]", "invalid_format", func(v map[string]any) { field(v, "records")["keep"] = []any{"work/summary.md"} }},
		{"records pass recent", "records.pass.recent", "out_of_range", func(v map[string]any) { field(v, "records", "pass")["recent"] = 51 }},
		{"case alias", "profileversion", "unknown_field", func(v map[string]any) { v["profileversion"] = "1.2.0" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			document := contractExampleProfile(t)
			tc.edit(document)
			commitProfile(t, repo, "1.2.0", yamlOf(t, document), false)
			status, body := postProfile(t, f, "1.2.0")
			details := profileDetails(t, status, body)
			if !reflect.DeepEqual(details, []Detail{{Field: tc.path, Reason: tc.reason}}) {
				t.Fatalf("details=%+v; want %s %s", details, tc.path, tc.reason)
			}
			if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
				t.Fatal("rejected Profile was persisted")
			}
		})
	}
}

func TestHarnessProfilesReportEveryFailedRule(t *testing.T) {
	f, repo := profileFixture(t)
	document := contractExampleProfile(t)
	document["extra"] = 1
	field(document, "stages", "work")["timeoutSec"] = 99999
	field(document, "stages", "check")["secrets"] = []any{map[string]any{"slot": "key", "access": "read"}}
	field(document, "stages", "prepare", "network")["allow"] = []any{"10.0.0.1:443"}
	commitProfile(t, repo, "1.2.0", yamlOf(t, document), false)
	status, body := postProfile(t, f, "1.2.0")
	got := map[string]string{}
	for _, d := range profileDetails(t, status, body) {
		got[d.Field] = d.Reason
	}
	want := map[string]string{
		"extra": "unknown_field", "stages.work.timeoutSec": "exceeds_limit",
		"stages.check.secrets": "not_allowed_in_stage", "stages.prepare.network.allow[0]": "ip_literal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("details=%v; want %v", got, want)
	}
}

func TestHarnessProfilesUseConfiguredStageTimeoutLimit(t *testing.T) {
	f, repo := profileFixture(t)
	stageExec(t, f, `INSERT INTO settings (key, value, updated_at) VALUES ('limits.stageTimeoutMaxSec', ?, '2026-11-11T00:00:00Z')`,
		`{"prepare":600,"work":3600,"check":900,"publish":599}`)
	commitProfile(t, repo, "1.2.0", yamlOf(t, contractExampleProfile(t)), false)
	status, body := postProfile(t, f, "1.2.0")
	details := profileDetails(t, status, body)
	if !reflect.DeepEqual(details, []Detail{{Field: "stages.publish.timeoutSec", Reason: "exceeds_limit"}}) {
		t.Fatalf("details=%+v", details)
	}
}

func TestHarnessProfilesSourceFailuresFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, definition, field, reason string }{
		{"missing tag", "", "tagName", "not_found"},
		{"missing file", "", "definition", "not_found"},
		{"malformed YAML", "profileVersion: [", "definition", "invalid_format"},
		{"not a mapping", "- a\n- b\n", "definition", "invalid_format"},
		{"multiple documents", validProfileYAML + "---\n{}", "definition", "invalid_format"},
		{"duplicate key", validProfileYAML + "name: other\n", "definition", "invalid_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			switch tc.name {
			case "missing tag":
			case "missing file":
				projectGit(t, repo, "tag", "profile/v1.2.0")
			default:
				commitProfile(t, repo, "1.2.0", tc.definition, false)
			}
			status, body := postProfile(t, f, "1.2.0")
			requireProjectError(t, status, body, 422, PreconditionFailed, tc.field, tc.reason)
			if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
				t.Fatal("invalid source was persisted")
			}
		})
	}
}

func TestHarnessProfilesSymlinkedDefinitionIsNotRead(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.0.0", strings.ReplaceAll(validProfileYAML, "1.2.0", "1.0.0"), false)
	if err := os.Rename(repo+"/evaloom/profile.yaml", repo+"/evaloom/real.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", repo+"/evaloom/profile.yaml"); err != nil {
		t.Fatal(err)
	}
	projectGit(t, repo, "add", "-A", "evaloom")
	projectGit(t, repo, "commit", "--quiet", "-m", "symlink")
	projectGit(t, repo, "tag", "profile/v1.2.0")
	status, body := postProfile(t, f, "1.2.0")
	requireProjectError(t, status, body, 422, PreconditionFailed, "definition", "not_found")
}
