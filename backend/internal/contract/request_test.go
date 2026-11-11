package contract_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
)

func TestRequestConstraints(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"contract version", func(v map[string]any) { v["contractVersion"] = "2" }},
		{"unknown stage", func(v map[string]any) { v["stage"] = "proposal" }},
		{"unknown mode", func(v map[string]any) { object(v, "execution")["mode"] = "retry" }},
		{"zero seq", func(v map[string]any) { object(v, "execution")["seq"] = 0 }},
		{"bad resume", func(v map[string]any) { object(v, "execution")["resumeStage"] = "prepare" }},
		{"unknown source", func(v map[string]any) { object(object(v, "task"), "source")["kind"] = "other" }},
		{"manual with identifier", func(v map[string]any) { object(object(v, "task"), "source")["kind"] = "manual" }},
		{"linear without identifier", func(v map[string]any) { object(object(v, "task"), "source")["identifier"] = nil }},
		{"unknown route", func(v map[string]any) { object(v, "route")["kind"] = "other" }},
		{"harness with project", func(v map[string]any) { object(v, "route")["kind"] = "harness" }},
		{"project without id", func(v map[string]any) { object(v, "route")["projectId"] = nil }},
		{"params array", func(v map[string]any) { v["params"] = []any{} }},
		{"params null", func(v map[string]any) { v["params"] = nil }},
		{"null string", func(v map[string]any) { object(v, "task")["body"] = nil }},
		{"missing title", func(v map[string]any) { delete(object(v, "task"), "title") }},
		{"bad timeout", func(v map[string]any) { object(v, "limits")["timeoutSec"] = 0 }},
		{"publish without checked content", func(v map[string]any) { delete(v, "checkedContentId") }},
		{"checked content in work", func(v map[string]any) { v["stage"] = "work" }},
		{"answer in publish", func(v map[string]any) { v["answer"] = map[string]any{"decisionId": "x", "text": "yes", "option": nil} }},
		{"rejected approval", func(v map[string]any) { v["approvals"].([]any)[0].(map[string]any)["status"] = "rejected" }},
		{"waived with id", func(v map[string]any) { v["approvals"].([]any)[1].(map[string]any)["id"] = "x" }},
		{"approved without id", func(v map[string]any) { v["approvals"].([]any)[0].(map[string]any)["id"] = nil }},
		{"approved without content", func(v map[string]any) { v["approvals"].([]any)[0].(map[string]any)["contentId"] = nil }},
		{"invalid previous output", func(v map[string]any) { object(v, "previous")["outputs"].([]any)[0].(map[string]any)["kind"] = "PR-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := document(t, requestExample(t))
			tc.edit(value)
			if err := json.Unmarshal(encode(t, value), &contract.Request{}); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestRequestManualHarnessAndWorkAnswer(t *testing.T) {
	value := document(t, requestExample(t))
	value["stage"] = "work"
	value["checkedContentId"] = nil
	delete(value, "approvals")
	value["previous"] = nil
	value["params"] = map[string]any{}
	value["route"] = map[string]any{"kind": "harness", "projectId": nil, "projectName": nil}
	value["answer"] = map[string]any{"decisionId": "answer", "text": "yes", "option": nil}
	object(value, "execution")["resumeStage"] = "work"
	object(value, "task")["source"] = map[string]any{"kind": "manual", "identifier": nil, "url": nil}
	object(value, "task")["acceptanceCriteria"] = ""
	var request contract.Request
	if err := json.Unmarshal(encode(t, value), &request); err != nil {
		t.Fatal(err)
	}
	value["params"] = map[string]any{"extra": true}
	if err := json.Unmarshal(encode(t, value), &contract.Request{}); err == nil {
		t.Error("harness route with params accepted")
	}
}

type paramsValidator struct {
	t      *testing.T
	expect json.RawMessage
	reject bool
}

func (v paramsValidator) ValidateParams(schema string, params json.RawMessage) error {
	v.t.Helper()
	if schema != "evaloom/params.schema.json" || !bytes.Equal(params, v.expect) {
		v.t.Error("schema or opaque params changed")
	}
	if v.reject {
		return errors.New("schema mismatch")
	}
	return nil
}

func TestRequestProfileBoundary(t *testing.T) {
	var profile contract.ProfileDefinition
	var request contract.Request
	if err := json.Unmarshal(profileExample(t), &profile); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(requestExample(t), &request); err != nil {
		t.Fatal(err)
	}
	validator := paramsValidator{t: t, expect: request.Params}
	if err := request.ValidateForProfile(profile, validator); err != nil {
		t.Fatal(err)
	}
	validator.reject = true
	if err := request.ValidateForProfile(profile, validator); err == nil {
		t.Error("ignored schema rejection")
	}
	if err := request.ValidateForProfile(profile, nil); err == nil {
		t.Error("missing schema validator accepted")
	}
	profile.Params = nil
	if err := request.ValidateForProfile(profile, nil); err == nil {
		t.Error("nonempty params without schema accepted")
	}
	request.Params = json.RawMessage(`{}`)
	if err := request.ValidateForProfile(profile, nil); err != nil {
		t.Fatal(err)
	}
	request.Task.AcceptanceCriteria = ""
	if err := request.ValidateForProfile(profile, nil); err == nil {
		t.Error("missing required acceptance criteria accepted")
	}
	profile.AcceptanceCriteria = "optional"
	if err := request.ValidateForProfile(profile, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOpaqueDataPreserved(t *testing.T) {
	value := document(t, requestExample(t))
	value["params"] = map[string]any{"domainSpecific": map[string]any{"unknown": []any{1.0, nil, true}}}
	var request contract.Request
	if err := json.Unmarshal(encode(t, value), &request); err != nil {
		t.Fatal(err)
	}
	var roundTrip contract.Request
	if err := json.Unmarshal(encode(t, request), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(request.Params, roundTrip.Params) {
		t.Error("opaque params changed")
	}
	var result contract.Result
	value = document(t, resultExample(t))
	value["approvalRequests"].([]any)[0].(map[string]any)["target"] = map[string]any{"domainSpecific": []any{nil, 1.0, false}}
	if err := json.Unmarshal(encode(t, value), &result); err != nil {
		t.Fatal(err)
	}
}
