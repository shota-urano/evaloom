package contract_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"gopkg.in/yaml.v3"
)

// Read the examples from the source of truth so schema drift fails this test.
// Angle-bracket explanations of identifiers/images stand for real wire values.
func example(t *testing.T, heading, language string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../docs/contract/harness-contract-v1.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), heading)
	if !found {
		t.Fatalf("missing contract heading %s", heading)
	}
	_, block, found := strings.Cut(section, "```"+language+"\n")
	if !found {
		t.Fatal("missing example")
	}
	block, _, _ = strings.Cut(block, "```")
	block = strings.NewReplacer(
		"<ダイジェスト付きのイメージ>", "example/harness@sha256:"+strings.Repeat("a", 64),
		"<40桁>", strings.Repeat("a", 40),
		"<40桁の commit SHA など>", strings.Repeat("b", 40),
		"<前回の確かめる段が報告した識別子>", "previous-content",
		"<この Execution の確かめる段が報告した識別子>", "checked-content",
		"<識別子>", "approved-content",
	).Replace(block)
	if language == "yaml" {
		var document map[string]any
		if err := yaml.Unmarshal([]byte(block), &document); err != nil {
			t.Fatal(err)
		}
		return encode(t, document)
	}
	return []byte(block)
}

func encode(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func document(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func object(value map[string]any, key string) map[string]any {
	return value[key].(map[string]any)
}

func requestExample(t *testing.T) []byte { return example(t, "### 6.1 ", "json") }
func resultExample(t *testing.T) []byte  { return example(t, "### 6.5 ", "json") }
func profileExample(t *testing.T) []byte { return example(t, "### 5.1 ", "yaml") }
func findingExample(t *testing.T) []byte { return example(t, "## 14. ", "json") }

func TestContractExamples(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		into any
	}{
		{"5.1 profile", profileExample(t), &contract.ProfileDefinition{}},
		{"6.1 request", requestExample(t), &contract.Request{}},
		{"6.5 result", resultExample(t), &contract.Result{}},
		{"14 finding", findingExample(t), &contract.Finding{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal(tc.data, tc.into); err != nil {
				t.Fatal(err)
			}
			// Ensure marshalled Go values also produce valid contract JSON.
			if err := json.Unmarshal(encode(t, tc.into), tc.into); err != nil {
				t.Fatalf("round trip: %v", err)
			}
		})
	}
}

func TestClosedWireObjects(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		into any
	}{
		{"request", requestExample(t), &contract.Request{}},
		{"result", resultExample(t), &contract.Result{}},
		{"profile", profileExample(t), &contract.ProfileDefinition{}},
		{"finding", findingExample(t), &contract.Finding{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Mutate every closed object, including array elements. Opaque
			// params, target and Adapter permissions deliberately stay open.
			rootDocument := document(t, tc.data)
			var walk func(map[string]any, string)
			walk = func(value map[string]any, path string) {
				value["unknown"] = true
				if err := json.Unmarshal(encode(t, rootDocument), tc.into); err == nil {
					t.Errorf("unknown field accepted at %s", path)
				}
				delete(value, "unknown")
				for key, child := range value {
					if key == "params" && tc.name == "request" || key == "target" || key == "toolPermissions" {
						continue
					}
					switch child := child.(type) {
					case map[string]any:
						walk(child, path+"."+key)
					case []any:
						for _, item := range child {
							if object, ok := item.(map[string]any); ok {
								walk(object, path+"."+key+"[]")
							}
						}
					}
				}
			}
			walk(rootDocument, "$")
			for _, field := range []string{"contractVersion", "stage", "rule"} {
				value := document(t, tc.data)
				original, exists := value[field]
				if !exists {
					continue
				}
				delete(value, field)
				if err := json.Unmarshal(encode(t, value), tc.into); err == nil {
					t.Errorf("missing %s accepted", field)
				}
				value[strings.ToUpper(field)] = original
				if err := json.Unmarshal(encode(t, value), tc.into); err == nil {
					t.Errorf("case alias for %s accepted", field)
				}
			}
			duplicate := append([]byte(`{"contractVersion":"0",`), tc.data[strings.IndexByte(string(tc.data), '{')+1:]...)
			if err := json.Unmarshal(duplicate, tc.into); err == nil {
				t.Error("duplicate field accepted")
			}
			for _, bad := range []string{"null", "[]", string(tc.data) + `{}`} {
				if err := json.Unmarshal([]byte(bad), tc.into); err == nil {
					t.Errorf("invalid root accepted: %s", bad)
				}
			}
			original := encode(t, tc.into)
			if err := json.Unmarshal([]byte(`{"unknown":true}`), tc.into); err == nil {
				t.Fatal("bad decode succeeded")
			}
			if string(original) != string(encode(t, tc.into)) {
				t.Error("failed decode mutated receiver")
			}
		})
	}
}

func TestSpecialHarnessErrorClasses(t *testing.T) {
	for _, class := range []string{
		"config_error", "prepare_failed", "check_failed", "publish_failed", "publish_unknown", "cancelled",
		"work_failed", "approval_rejected", "contract_error", "sandbox_error", "timeout", "interrupted",
		"execution_failed", "test_failed", "workspace_missing", "unknown", "",
	} {
		t.Run(class, func(t *testing.T) {
			value := document(t, resultExample(t))
			value["status"], value["errorClass"] = "failed", class
			var result contract.Result
			err := json.Unmarshal(encode(t, value), &result)
			allowed := class == "config_error" || class == "prepare_failed" || class == "check_failed" || class == "publish_failed" || class == "publish_unknown" || class == "cancelled"
			if (err == nil) != allowed {
				t.Errorf("allowed=%v, error=%v", allowed, err)
			}
			goValue := contract.Result{ContractVersion: "1", Stage: model.StageCheck, Status: model.StageFailed, ErrorClass: pointer(model.ErrorClass(class))}
			if (goValue.Validate() == nil) != allowed {
				t.Error("Go validation differs from JSON validation")
			}
		})
	}
}

func TestResultStageStatusMatrix(t *testing.T) {
	for _, stage := range []model.Stage{model.StagePrepare, model.StageWork, model.StageCheck, model.StagePublish} {
		for _, status := range []model.StageStatus{model.StageSucceeded, model.StageFailed, model.StageBlocked, model.StageNeedsApproval, model.StageUnknown, model.StageRunning, model.StageQuestion, model.StageCancelled, model.StageTimedOut, "unexpected"} {
			t.Run(string(stage)+"/"+string(status), func(t *testing.T) {
				value := contract.Result{ContractVersion: "1", Stage: stage, Status: status}
				if stage == model.StageCheck {
					value.ContentID = pointer("opaque-content")
				}
				if status == model.StageFailed {
					value.ErrorClass = pointer(model.ErrorConfig)
				}
				if status == model.StageUnknown {
					value.ErrorClass = pointer(model.ErrorPublishUnknown)
				}
				allowed := stage != model.StageWork && (status == model.StageSucceeded || status == model.StageFailed ||
					status == model.StageBlocked && stage == model.StageCheck ||
					status == model.StageNeedsApproval && stage != model.StagePrepare ||
					status == model.StageUnknown && stage == model.StagePublish)
				if err := json.Unmarshal(encode(t, value), &contract.Result{}); (err == nil) != allowed {
					t.Errorf("allowed=%v, error=%v", allowed, err)
				}
			})
		}
	}
}

func TestResultConstraints(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"failed needs error", func(v map[string]any) { v["status"] = "failed" }},
		{"unknown needs publish_unknown", func(v map[string]any) {
			v["stage"], v["status"], v["errorClass"] = "publish", "unknown", "publish_failed"
		}},
		{"check needs content", func(v map[string]any) { delete(v, "contentId") }},
		{"content empty", func(v map[string]any) { v["contentId"] = "" }},
		{"content long", func(v map[string]any) { v["contentId"] = strings.Repeat("a", 257) }},
		{"content non ASCII", func(v map[string]any) { v["contentId"] = "中身" }},
		{"content control", func(v map[string]any) { v["contentId"] = "abc\n" }},
		{"message long", func(v map[string]any) { v["message"] = strings.Repeat("あ", 1001) }},
		{"summary long", func(v map[string]any) { v["summary"] = strings.Repeat("あ", 4001) }},
		{"findings null element", func(v map[string]any) { v["findings"] = []any{nil} }},
		{"too many findings", func(v map[string]any) {
			items := make([]any, 501)
			for i := range items {
				items[i] = document(t, findingExample(t))
			}
			v["findings"] = items
		}},
		{"prepare check data", func(v map[string]any) { v["stage"] = "prepare" }},
		{"outputs in check", func(v map[string]any) {
			v["outputs"] = []any{map[string]any{"kind": "draft", "label": "x", "url": "opaque"}}
		}},
		{"reported context in check", func(v map[string]any) {
			v["reportedContext"] = []any{map[string]any{"source": "x", "path": "y", "revision": "z", "sha256": "hash"}}
		}},
		{"negative tokens", func(v map[string]any) {
			v["usage"] = map[string]any{"inputTokens": -1, "outputTokens": nil, "costAmount": nil}
		}},
		{"negative cost", func(v map[string]any) {
			v["usage"] = map[string]any{"inputTokens": nil, "outputTokens": nil, "costAmount": -0.1}
		}},
		{"target not object", func(v map[string]any) { v["approvalRequests"].([]any)[0].(map[string]any)["target"] = "url" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := document(t, resultExample(t))
			tc.edit(value)
			if err := json.Unmarshal(encode(t, value), &contract.Result{}); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
	var result contract.Result
	if err := json.Unmarshal(resultExample(t), &result); err != nil {
		t.Fatal(err)
	}
	result.Message = strings.Repeat("あ", 1000)
	result.Summary = pointer(strings.Repeat("あ", 4000))
	result.ContentID = pointer(strings.Repeat("x", 256))
	for range 500 {
		result.Findings = append(result.Findings, contract.Finding{Rule: "check", Severity: "info", Message: strings.Repeat("あ", 1000)})
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("inclusive limits rejected: %v", err)
	}
}

func TestFindingConstraints(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"rule", "Upper"}, {"rule", strings.Repeat("a", 65)}, {"rule", ""},
		{"severity", "critical"}, {"message", strings.Repeat("あ", 1001)},
		{"location", map[string]any{"path": "../outside", "line": 0}},
		{"location", map[string]any{"path": "/absolute", "line": 0}},
		{"location", map[string]any{"path": "relative", "line": -1}},
		{"location", map[string]any{"path": "relative", "line": nil}},
	} {
		value := document(t, findingExample(t))
		value[tc.key] = tc.value
		if err := json.Unmarshal(encode(t, value), &contract.Finding{}); err == nil {
			t.Errorf("invalid %s accepted: %v", tc.key, tc.value)
		}
	}
	value := document(t, findingExample(t))
	delete(value, "location")
	value["rule"] = strings.Repeat("a", 64)
	if err := json.Unmarshal(encode(t, value), &contract.Finding{}); err != nil {
		t.Fatal(err)
	}
}

func TestResultPinnedProfile(t *testing.T) {
	var profile contract.ProfileDefinition
	var result contract.Result
	if err := json.Unmarshal(profileExample(t), &profile); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resultExample(t), &result); err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateForProfile(model.StageCheck, profile); err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateForProfile(model.StagePublish, profile); err == nil {
		t.Error("stage mismatch accepted")
	}
	result.ApprovalRequests[0].Operation = "not_declared"
	if err := result.ValidateForProfile(model.StageCheck, profile); err == nil {
		t.Error("undeclared operation accepted")
	}
}

func TestProfileConstraints(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"contract version", func(v map[string]any) { v["contractVersion"] = "2" }},
		{"version not semver", func(v map[string]any) { v["profileVersion"] = "1.2" }},
		{"leading zero prerelease", func(v map[string]any) { v["profileVersion"] = "1.2.0-01" }},
		{"description long", func(v map[string]any) { v["description"] = strings.Repeat("あ", 2001) }},
		{"acceptance invalid", func(v map[string]any) { v["acceptanceCriteria"] = "maybe" }},
		{"schema outside", func(v map[string]any) { object(v, "params")["schema"] = "../schema" }},
		{"missing work", func(v map[string]any) { delete(object(v, "stages"), "work") }},
		{"work runtime", func(v map[string]any) {
			object(object(v, "stages"), "work")["runtime"] = map[string]any{"kind": "container", "image": "test"}
		}},
		{"work secrets", func(v map[string]any) { object(object(v, "stages"), "work")["secrets"] = []any{} }},
		{"work network", func(v map[string]any) {
			object(object(v, "stages"), "work")["network"] = map[string]any{"allow": []any{}}
		}},
		{"work resume invalid", func(v map[string]any) { object(object(v, "stages"), "work")["resume"] = "never" }},
		{"work null instruction", func(v map[string]any) { object(object(v, "stages"), "work")["instructions"] = []any{nil} }},
		{"work path outside", func(v map[string]any) { object(object(v, "stages"), "work")["skills"] = []any{"../outside"} }},
		{"empty adapter", func(v map[string]any) { object(object(v, "stages"), "work")["microHarness"] = "" }},
		{"zero timeout", func(v map[string]any) { object(object(v, "stages"), "work")["timeoutSec"] = 0 }},
		{"fractional timeout", func(v map[string]any) { object(object(v, "stages"), "check")["timeoutSec"] = 1.5 }},
		{"absolute command", func(v map[string]any) { object(object(v, "stages"), "check")["command"] = []any{"/bin/sh"} }},
		{"check network", func(v map[string]any) {
			object(object(v, "stages"), "check")["network"] = map[string]any{"allow": []any{}}
		}},
		{"check secrets", func(v map[string]any) {
			object(object(v, "stages"), "check")["secrets"] = []any{map[string]any{"slot": "key", "access": "read"}}
		}},
		{"prepare write secret", func(v map[string]any) {
			object(object(v, "stages"), "prepare")["secrets"] = []any{map[string]any{"slot": "key", "access": "write"}}
		}},
		{"invalid slot", func(v map[string]any) {
			object(object(v, "stages"), "prepare")["secrets"] = []any{map[string]any{"slot": "Bad", "access": "read"}}
		}},
		{"duplicate slot", func(v map[string]any) {
			object(object(v, "stages"), "prepare")["secrets"] = []any{map[string]any{"slot": "key", "access": "read"}, map[string]any{"slot": "key", "access": "read"}}
		}},
		{"mutable container image", func(v map[string]any) {
			object(object(object(v, "stages"), "prepare"), "runtime")["image"] = "example:latest"
		}},
		{"duplicate operation", func(v map[string]any) { v["approvals"] = append(v["approvals"].([]any), v["approvals"].([]any)[0]) }},
		{"record from work", func(v map[string]any) { object(v, "records")["keep"] = []any{"work/report.md"} }},
		{"record outside", func(v map[string]any) { object(v, "records")["keep"] = []any{"check/../outside"} }},
		{"too many records", func(v map[string]any) { object(object(v, "records"), "pass")["recent"] = 51 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := document(t, profileExample(t))
			tc.edit(value)
			if err := json.Unmarshal(encode(t, value), &contract.ProfileDefinition{}); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
	for _, dest := range []string{"*.github.com:443", "127.0.0.1:443", "[::1]:443", "localhost:443", "host.local:443", "host:443", "github.com", "github.com:0", "github.com:65536", "-host.com:443", "github.com:443/path"} {
		value := document(t, profileExample(t))
		object(object(object(value, "stages"), "prepare"), "network")["allow"] = []any{dest}
		if err := json.Unmarshal(encode(t, value), &contract.ProfileDefinition{}); err == nil {
			t.Errorf("invalid network destination accepted: %s", dest)
		}
	}
}

type environment struct{ reject string }

func (e environment) ValidateAdapter(name string) error {
	if e.reject == "adapter" || (name != "codex" && name != "claude_code") {
		return errors.New("unregistered Adapter")
	}
	return nil
}

func (e environment) StageTimeoutMaxSec(model.Stage) int {
	if e.reject == "timeout" {
		return 1
	}
	return 14400
}
func (e environment) ValidateToolPermissions(model.AdapterName, json.RawMessage) error {
	if e.reject == "permissions" {
		return errors.New("unsupported Adapter permission")
	}
	return nil
}
func (e environment) ValidateRuntime(contract.Runtime) error {
	if e.reject == "runtime" {
		return errors.New("unsupported runtime")
	}
	return nil
}
func (e environment) ValidateNetworkDestination(string) error {
	if e.reject == "model endpoint" {
		return errors.New("known model endpoint")
	}
	return nil
}

func TestProfileEnvironmentBoundary(t *testing.T) {
	var profile contract.ProfileDefinition
	if err := json.Unmarshal(profileExample(t), &profile); err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateForRegistration("1.2.0", environment{}); err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateForRegistration("1.2.1", environment{}); err == nil {
		t.Error("tag mismatch accepted")
	}
	if err := profile.ValidateForRegistration("1.2.0", nil); err == nil {
		t.Error("missing environment accepted")
	}
	for _, reject := range []string{"adapter", "timeout", "permissions", "runtime", "model endpoint"} {
		if err := profile.ValidateForRegistration("1.2.0", environment{reject}); err == nil {
			t.Errorf("ignored environment rejection: %s", reject)
		}
	}
}

func TestProfileOptionalDeclarations(t *testing.T) {
	value := document(t, profileExample(t))
	for _, key := range []string{"params", "approvals", "records"} {
		delete(value, key)
	}
	stages := object(value, "stages")
	delete(stages, "prepare")
	for _, key := range []string{"instructions", "skills", "settings", "toolPermissions", "resume"} {
		delete(object(stages, "work"), key)
	}
	object(stages, "work")["instructions"] = []any{"./evaloom/work/AGENTS.md"}
	delete(object(stages, "publish"), "network")
	delete(object(stages, "publish"), "secrets")
	var profile contract.ProfileDefinition
	if err := json.Unmarshal(encode(t, value), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Stages.Prepare != nil || len(profile.Approvals) != 0 {
		t.Error("optional declarations were invented")
	}
	// No digest algorithm is fixed by this contract.
	object(stages, "check")["runtime"] = map[string]any{"kind": "container", "image": "example/image@sha512:" + strings.Repeat("a", 128)}
	if err := json.Unmarshal(encode(t, value), &profile); err != nil {
		t.Fatal(err)
	}
}

func pointer[T any](v T) *T { return &v }
