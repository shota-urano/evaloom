package contract_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
)

// prepareRequest turns the contract 6.1 example (publish) into prepare.
func prepareRequest(t *testing.T) map[string]any {
	t.Helper()
	value := document(t, requestExample(t))
	value["stage"] = "prepare"
	delete(value, "checkedContentId")
	delete(value, "approvals")
	return value
}

// contract 6.1 taskLinks: prepare only, distinct https links, at most 50.
func TestRequestTaskLinks(t *testing.T) {
	links := func(n int) []any {
		out := []any{}
		for i := range n {
			out = append(out, fmt.Sprintf("https://l%d.example.com/", i))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]any)
		valid bool
	}{
		{"absent", func(map[string]any) {}, true},
		{"empty array", func(v map[string]any) { v["taskLinks"] = []any{} }, true},
		{"fifty links", func(v map[string]any) { v["taskLinks"] = links(50) }, true},
		{"fifty-one links", func(v map[string]any) { v["taskLinks"] = links(51) }, false},
		{"null", func(v map[string]any) { v["taskLinks"] = nil }, false},
		{"duplicate", func(v map[string]any) { v["taskLinks"] = []any{"https://a.example.com/", "https://a.example.com/"} }, false},
		{"not https", func(v map[string]any) { v["taskLinks"] = []any{"http://a.example.com/"} }, false},
		{"in work", func(v map[string]any) { v["stage"] = "work"; v["taskLinks"] = []any{} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := prepareRequest(t)
			tc.edit(value)
			var request contract.Request
			err := json.Unmarshal(encode(t, value), &request)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
