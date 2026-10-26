package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"
)

// DecodeJSON requires exactly one JSON object and rejects unknown fields.
// Handlers must perform domain validation after decoding and before mutation.
func DecodeJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' {
		return invalid("body", "invalid_json")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid("body", "invalid_json")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return invalid("body", "invalid_json")
	}
	return nil
}

type IDFormat string

const (
	ULID   IDFormat = "ulid"
	SlugID IDFormat = "slug"
	TaskID IDFormat = "task"
)

var idPatterns = map[IDFormat]*regexp.Regexp{
	ULID:   regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`),
	SlugID: regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`),
	TaskID: regexp.MustCompile(`^task-[0-9]+$`),
}

// ValidateID follows data-model 3.1: ULID by default, slugs for projects and
// harnesses, task-<sequence> for tasks. No unprovided length limit is imposed.
func ValidateID(field, value string, format IDFormat) error {
	if value == "" {
		return invalid(field, "required")
	}
	pattern, ok := idPatterns[format]
	if !ok || !pattern.MatchString(value) {
		return invalid(field, "invalid_format")
	}
	return nil
}

// Timestamp emits RFC 3339 UTC strings, retaining subsecond precision when
// present. Inputs must also be UTC; zero-offset forms normalize to Z.
type Timestamp struct{ time.Time }

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return t.UTC().MarshalJSON()
}

func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return invalid("timestamp", "invalid_format")
	}
	parsed, err := ParseTimestamp(value)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

func ParseTimestamp(value string) (Timestamp, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	_, offset := parsed.Zone()
	if err != nil || offset != 0 {
		return Timestamp{}, invalid("timestamp", "invalid_format")
	}
	return Timestamp{parsed.UTC()}, nil
}
