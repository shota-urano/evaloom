package adapter_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func TestContractSpecExamples(t *testing.T) {
	var request adapter.HarnessRequest
	assertRoundTrip(t, fixture(t, "request"), &request)
	if request.ExecutionID != "01J..." || request.Task.ID != "task-001" ||
		request.Profile.Version != "1.2.0" || request.Limits.TimeoutSec != 0 {
		t.Fatalf("request fields not decoded: %+v", request)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	var result adapter.HarnessResult
	assertRoundTrip(t, fixture(t, "result"), &result)
	if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil ||
		result.Usage.InputTokens != nil || result.Usage.OutputTokens != nil || result.PRURL != nil {
		t.Fatalf("result fields not decoded: %+v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryContractFieldIsRequired(t *testing.T) {
	for _, kind := range []string{"request", "result"} {
		original := fixture(t, kind)
		var object map[string]any
		if err := json.Unmarshal(original, &object); err != nil {
			t.Fatal(err)
		}
		// Enumerate the specification fixtures, including every nested field.
		// Profile definition is an opaque object, so its internal keys are not
		// part of the HarnessRequest schema.
		walkFields(object, nil, func(path []string, value any) {
			name := kind + "/" + strings.Join(path, ".")
			t.Run(name+"/missing", func(t *testing.T) {
				err := decodeContract(kind, changed(t, original, path, nil, true))
				if err == nil || !strings.Contains(err.Error(), strings.Join(path, ".")) {
					t.Fatalf("missing field accepted or not identified: %v", err)
				}
			})
			t.Run(name+"/null", func(t *testing.T) {
				err := decodeContract(kind, changed(t, original, path, nil, false))
				if (err == nil) != (value == nil) {
					t.Fatalf("null field: error = %v, nullable = %v", err, value == nil)
				}
			})
		})
	}
}

func TestInvalidContractValues(t *testing.T) {
	for _, tc := range []struct {
		kind, field string
		value       any
	}{
		{"request", "contractVersion", "2"},
		{"request", "contractVersion", 1},
		{"request", "executionId", 1},
		{"request", "task", []any{}},
		{"request", "task.title", false},
		{"request", "task.acceptanceCriteria", ""},
		{"request", "profile.definition", []any{}},
		{"request", "profile.definition", "not an object"},
		{"request", "context.revisionIds", "not an array"},
		{"request", "context.revisionIds", []any{nil}},
		{"request", "context.revisionIds", []any{12}},
		{"request", "limits.timeoutSec", -1},
		{"request", "limits.timeoutSec", 1.5},
		{"result", "contractVersion", ""},
		{"result", "status", "running"},
		{"result", "status", "Succeeded"},
		{"result", "errorClass", "unknown_error"},
		{"result", "errorClass", ""},
		{"result", "errorClass", 1},
		{"result", "prUrl", 42},
		{"result", "commits", []any{nil}},
		{"result", "logArtifactIds", []any{false}},
		{"result", "artifactIds", []any{nil}},
		{"result", "usage.inputTokens", "10"},
		{"result", "usage.inputTokens", -1},
		{"result", "usage.outputTokens", -1},
		{"result", "usage.wallTimeMs", -1},
		{"result", "usage.wallTimeMs", 0.5},
		{"request", "ExecutionId", "case alias"},
		{"result", "Status", "failed"},
	} {
		t.Run(tc.kind+"/"+tc.field+"/"+string(mustJSON(t, tc.value)), func(t *testing.T) {
			data := changed(t, fixture(t, tc.kind), strings.Split(tc.field, "."), tc.value, false)
			if err := decodeContract(tc.kind, data); err == nil {
				t.Fatalf("invalid JSON accepted: %s", data)
			}
		})
	}
	for _, kind := range []string{"request", "result"} {
		for _, input := range []string{"null", "[]", "{}", `"text"`, `{`, string(fixture(t, kind)) + " {}"} {
			if err := decodeContract(kind, []byte(input)); err == nil {
				t.Errorf("%s accepted invalid document: %s", kind, input)
			}
		}
	}
}

func TestFailedResultAcceptsAllSpecifiedErrorClasses(t *testing.T) {
	for _, classification := range []string{
		"config_error", "execution_failed", "test_failed", "timeout", "cancelled",
		"interrupted", "publish_failed", "sandbox_error",
	} {
		t.Run(classification, func(t *testing.T) {
			data := changed(t, fixture(t, "result"), []string{"status"}, "failed", false)
			data = changed(t, data, []string{"errorClass"}, classification, false)
			var result adapter.HarnessResult
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != adapter.ResultFailed || result.ErrorClass == nil || string(*result.ErrorClass) != classification {
				t.Fatalf("failed result decoded incorrectly: %+v", result)
			}
		})
	}
}

func TestContractAllowsEmptyArraysAndAvailableUsage(t *testing.T) {
	request := changed(t, fixture(t, "request"), []string{"context", "revisionIds"}, []any{}, false)
	if err := decodeContract("request", request); err != nil {
		t.Fatal(err)
	}
	result := fixture(t, "result")
	for _, field := range []string{"commits", "logArtifactIds", "artifactIds"} {
		result = changed(t, result, []string{field}, []any{}, false)
	}
	result = changed(t, result, []string{"summary"}, "", false)
	result = changed(t, result, []string{"usage"}, map[string]any{
		"inputTokens": 0, "outputTokens": int64(9007199254740993), "wallTimeMs": 123,
	}, false)
	result = changed(t, result, []string{"prUrl"}, "https://example.com/pull/1", false)
	var decoded adapter.HarnessResult
	assertRoundTrip(t, result, &decoded)
	if decoded.Usage.InputTokens == nil || *decoded.Usage.InputTokens != 0 ||
		decoded.Usage.OutputTokens == nil || *decoded.Usage.OutputTokens != 9007199254740993 {
		t.Fatalf("token counts lost: %+v", decoded.Usage)
	}
}

func TestInvalidJSONDoesNotReplaceExistingContract(t *testing.T) {
	var request adapter.HarnessRequest
	if err := json.Unmarshal(fixture(t, "request"), &request); err != nil {
		t.Fatal(err)
	}
	before := mustJSON(t, request)
	bad := changed(t, fixture(t, "request"), []string{"limits", "timeoutSec"}, -1, false)
	if err := json.Unmarshal(bad, &request); err == nil {
		t.Fatal("invalid request accepted")
	}
	if string(mustJSON(t, request)) != string(before) {
		t.Fatal("invalid request replaced existing value")
	}
	var result adapter.HarnessResult
	if err := json.Unmarshal(fixture(t, "result"), &result); err != nil {
		t.Fatal(err)
	}
	before = mustJSON(t, result)
	bad = changed(t, fixture(t, "result"), []string{"errorClass"}, "unknown", false)
	if err := json.Unmarshal(bad, &result); err == nil {
		t.Fatal("invalid result accepted")
	}
	if string(mustJSON(t, result)) != string(before) {
		t.Fatal("invalid result replaced existing value")
	}
}

func fixture(t *testing.T, kind string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/harness_" + kind + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeContract(kind string, data []byte) error {
	if kind == "request" {
		return json.Unmarshal(data, new(adapter.HarnessRequest))
	}
	return json.Unmarshal(data, new(adapter.HarnessResult))
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func changed(t *testing.T, original []byte, path []string, value any, remove bool) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(original, &object); err != nil {
		t.Fatal(err)
	}
	if len(path) > 1 {
		object[path[0]] = changed(t, object[path[0]], path[1:], value, remove)
	} else if remove {
		delete(object, path[0])
	} else {
		object[path[0]] = mustJSON(t, value)
	}
	return mustJSON(t, object)
}

func walkFields(object map[string]any, parent []string, visit func([]string, any)) {
	for key, value := range object {
		path := append(append([]string(nil), parent...), key)
		visit(path, value)
		if child, ok := value.(map[string]any); ok && strings.Join(path, ".") != "profile.definition" {
			walkFields(child, path, visit)
		}
	}
}

func assertRoundTrip(t *testing.T, original []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(original, target); err != nil {
		t.Fatal(err)
	}
	var before, after any
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mustJSON(t, target), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("round trip changed JSON: before=%v after=%v", before, after)
	}
}
