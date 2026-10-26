package api

import (
	"bytes"
	"encoding/json"
	"slices"
)

// PATCH replaces supplied top-level values (including objects and arrays).
// Nullable columns can be cleared with null; ID and timestamps are immutable.
func decodeSettings[T any](before T, patch map[string]json.RawMessage, create bool, nullable ...string) (T, error) {
	encoded, err := json.Marshal(before)
	if err != nil {
		return before, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return before, err
	}
	for field, value := range patch {
		if field == "id" && create {
			continue
		}
		if _, known := merged[field]; !known {
			return before, invalid("body", "invalid_json")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if !slices.Contains(nullable, field) {
				return before, invalid(field, "required")
			}
		}
		merged[field] = value
	}
	encoded, err = json.Marshal(merged)
	if err != nil {
		return before, err
	}
	var settings T
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&settings); err != nil {
		return before, invalid("body", "invalid_json")
	}
	return settings, nil
}
