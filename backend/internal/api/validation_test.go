package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecodeJSONValidatesBeforeUse(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "true", "{", `{"name":1}`, `{"unknown":"value"}`, `{"name":"ok"} {}`, `{"name":"ok"} garbage`} {
		t.Run(body, func(t *testing.T) {
			var input struct {
				Name string `json:"name"`
			}
			err := DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &input)
			assertValidation(t, err, "body")
		})
	}
	var input struct {
		Name string `json:"name"`
	}
	if err := DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader("  {\"name\":\"ok\"}\n ")), &input); err != nil || input.Name != "ok" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
}

func TestIDFormatsDataModel31(t *testing.T) {
	for _, tc := range []struct {
		value  string
		format IDFormat
		valid  bool
	}{
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", ULID, true}, {"7ZZZZZZZZZZZZZZZZZZZZZZZZZ", ULID, true},
		{"8ZZZZZZZZZZZZZZZZZZZZZZZZZ", ULID, false}, {"01ARZ3NDEKTSV4RRFFQ69G5FAI", ULID, false},
		{"01arz3ndektsv4rrffq69g5fav", ULID, false}, {"01ARZ3NDEKTSV4RRFFQ69G5FA", ULID, false},
		{"project-a", SlugID, true}, {"claude-developer", SlugID, true}, {"a1", SlugID, true},
		{"../project", SlugID, false}, {"Project", SlugID, false}, {"project_1", SlugID, false}, {"-project", SlugID, false},
		{"task-001", TaskID, true}, {"task-1234", TaskID, true}, {"task-a", TaskID, false}, {"task-", TaskID, false},
		{"", ULID, false}, {"x", IDFormat("unknown"), false},
	} {
		t.Run(string(tc.format)+"/"+tc.value, func(t *testing.T) {
			err := ValidateID("id", tc.value, tc.format)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			assertValidation(t, err, "id")
		})
	}
}

func TestTimestampRejectsNonRFC3339Syntax(t *testing.T) {
	for _, value := range []string{"2026-10-26T1:34:56Z", "2026-10-26T12:34:56,123Z"} {
		t.Run(value, func(t *testing.T) {
			t.Run("ParseTimestamp", func(t *testing.T) {
				_, err := ParseTimestamp(value)
				assertValidation(t, err, "timestamp")
			})
			t.Run("UnmarshalJSON", func(t *testing.T) {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var timestamp Timestamp
				assertValidation(t, json.Unmarshal(data, &timestamp), "timestamp")
			})
		})
	}
}

func TestTimestampsAreRFC3339UTC(t *testing.T) {
	for _, value := range []string{"2026-10-26T12:34:56Z", "2026-10-26T12:34:56.123456789Z", "2026-10-26T12:34:56+00:00"} {
		parsed, err := ParseTimestamp(value)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(parsed)
		if err != nil || !strings.HasSuffix(string(data), "Z\"") {
			t.Fatalf("json=%s err=%v", data, err)
		}
		var decoded Timestamp
		if err := json.Unmarshal(data, &decoded); err != nil || !decoded.Equal(parsed.Time) {
			t.Fatalf("roundtrip=%v err=%v", decoded, err)
		}
	}
	for _, value := range []string{"", "now", "2026-10-26", "2026-10-26T12:34:56", "2026-10-26T12:34:56+09:00", "2026-02-30T00:00:00Z"} {
		_, err := ParseTimestamp(value)
		assertValidation(t, err, "timestamp")
	}
	for _, data := range []string{"null", "123", `"bad"`} {
		var timestamp Timestamp
		assertValidation(t, json.Unmarshal([]byte(data), &timestamp), "timestamp")
	}
	data, err := json.Marshal(Timestamp{time.Date(2026, 10, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))})
	if err != nil || string(data) != `"2026-10-26T03:00:00Z"` {
		t.Fatalf("UTC conversion=%s err=%v", data, err)
	}
}
