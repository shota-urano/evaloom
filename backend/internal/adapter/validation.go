package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ContractVersion is the Phase 1 wire contract version (spec 02, section 4.2).
const ContractVersion = "1"

// UnmarshalJSON validates the complete request before replacing the receiver.
// All fields shown in spec 02, section 5.7 are required, including zero limits
// and empty lists. Profile definition is an opaque JSON object owned by Registry.
func (r *HarnessRequest) UnmarshalJSON(data []byte) error {
	fields, err := requiredObject(data, "request", "contractVersion executionId task project profile context limits", "")
	if err != nil {
		return err
	}
	for _, object := range []struct{ name, fields string }{
		{"task", "id title body acceptanceCriteria"},
		{"project", "id workdir branch baseSha"},
		{"profile", "harnessId version commitSha modelId definition"},
		{"context", "instructions revisionIds"},
		{"limits", "timeoutSec"},
	} {
		if _, err := requiredObject(fields[object.name], object.name, object.fields, ""); err != nil {
			return err
		}
	}
	type plain HarnessRequest
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	value := HarnessRequest(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	// encoding/json accepts null as a string's zero value; array entries must
	// still be strings, just as scalar fields must not be null.
	var contextFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["context"], &contextFields); err != nil {
		return err
	}
	if err := stringArray(contextFields["revisionIds"], "context.revisionIds"); err != nil {
		return err
	}
	*r = value
	return nil
}

// Validate checks values constructed in Go. JSON decoding additionally checks
// field presence, which cannot be recovered from Go scalar zero values.
func (r HarnessRequest) Validate() error {
	if r.ContractVersion != ContractVersion {
		return fmt.Errorf("contractVersion: expected %q", ContractVersion)
	}
	if r.Task.AcceptanceCriteria == "" {
		return fmt.Errorf("task.acceptanceCriteria: must not be empty")
	}
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(r.Profile.Definition, &definition); err != nil || definition == nil {
		return fmt.Errorf("profile.definition: expected object")
	}
	if r.Context.RevisionIDs == nil {
		return fmt.Errorf("context.revisionIds: expected array")
	}
	if r.Limits.TimeoutSec < 0 {
		return fmt.Errorf("limits.timeoutSec: must not be negative")
	}
	return nil
}

// UnmarshalJSON validates spec 02, section 5.8. Nullable fields must still be
// present: null denotes unavailable usage or a PR URL not yet filled by Runner.
func (r *HarnessResult) UnmarshalJSON(data []byte) error {
	fields, err := requiredObject(data, "result",
		"contractVersion status errorClass sessionId headSha commits diffArtifactId summary logArtifactIds artifactIds usage prUrl",
		"errorClass prUrl")
	if err != nil {
		return err
	}
	if _, err := requiredObject(fields["usage"], "usage", "inputTokens outputTokens wallTimeMs", "inputTokens outputTokens"); err != nil {
		return err
	}
	type plain HarnessResult
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("result: %w", err)
	}
	value := HarnessResult(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	for _, field := range []string{"commits", "logArtifactIds", "artifactIds"} {
		if err := stringArray(fields[field], field); err != nil {
			return err
		}
	}
	*r = value
	return nil
}

func (r HarnessResult) Validate() error {
	if r.ContractVersion != ContractVersion {
		return fmt.Errorf("contractVersion: expected %q", ContractVersion)
	}
	if err := r.Status.Validate(); err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if r.ErrorClass != nil {
		if err := r.ErrorClass.Validate(); err != nil {
			return fmt.Errorf("errorClass: %w", err)
		}
	}
	if r.Commits == nil || r.LogArtifactIDs == nil || r.ArtifactIDs == nil {
		return fmt.Errorf("commits, logArtifactIds and artifactIds: expected arrays")
	}
	if r.Usage.WallTimeMS < 0 || (r.Usage.InputTokens != nil && *r.Usage.InputTokens < 0) ||
		(r.Usage.OutputTokens != nil && *r.Usage.OutputTokens < 0) {
		return fmt.Errorf("usage: counts and durations must not be negative")
	}
	return nil
}

// requiredObject uses exact wire names rather than encoding/json's
// case-insensitive field matching. Version 1 objects have a closed field set;
// profile.definition is deliberately not inspected here.
func requiredObject(data []byte, path, required, nullable string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("%s: expected object", path)
	}
	names := strings.Fields(required)
	nulls := strings.Fields(nullable)
	for _, name := range names {
		value, ok := fields[name]
		if !ok {
			return nil, fmt.Errorf("%s.%s: required field missing", path, name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !slices.Contains(nulls, name) {
			return nil, fmt.Errorf("%s.%s: must not be null", path, name)
		}
	}
	for name := range fields {
		if !slices.Contains(names, name) {
			return nil, fmt.Errorf("%s.%s: unknown field", path, name)
		}
	}
	return fields, nil
}

func stringArray(data []byte, path string) error {
	var elements []json.RawMessage
	if err := json.Unmarshal(data, &elements); err != nil {
		return fmt.Errorf("%s: expected string array", path)
	}
	for i, element := range elements {
		if bytes.Equal(bytes.TrimSpace(element), []byte("null")) {
			return fmt.Errorf("%s[%d]: must not be null", path, i)
		}
	}
	return nil
}
