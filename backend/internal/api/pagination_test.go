package api

import (
	"encoding/base64"
	"errors"
	"net/url"
	"testing"
)

func TestPaginationLimits(t *testing.T) {
	for _, tc := range []struct {
		query   string
		want    int
		invalid bool
	}{
		{"", 50, false}, {"limit=1", 1, false}, {"limit=200", 200, false},
		{"limit=201", 0, true}, {"limit=0", 0, true}, {"limit=-1", 0, true},
		{"limit=", 0, true}, {"limit=abc", 0, true}, {"limit=1.5", 0, true},
		{"limit=%2B1", 0, true}, {"limit=1&limit=2", 0, true}, {"limit=999999999999999999999", 0, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			query, _ := url.ParseQuery(tc.query)
			page, err := ParsePage(query, "runners")
			if tc.invalid {
				assertValidation(t, err, "limit")
				return
			}
			if err != nil || page.Limit != tc.want {
				t.Fatalf("page=%+v err=%v; want limit %d", page, err, tc.want)
			}
		})
	}
}

func TestCursorContract(t *testing.T) {
	page := NewPage([]string{"a", "b", "c"}, 2, "runners", func(id string) string { return id })
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	parsed, err := ParsePage(url.Values{"cursor": {page.NextCursor}}, "runners")
	if err != nil || parsed.After != "b" || parsed.Limit != 50 {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	for _, token := range []string{"%bad", "YQ==", base64.RawURLEncoding.EncodeToString([]byte("runners\n")), base64.RawURLEncoding.EncodeToString([]byte("runners\na\nb")), base64.RawURLEncoding.EncodeToString([]byte("runners\n\xff"))} {
		_, err := ParsePage(url.Values{"cursor": {token}}, "runners")
		assertValidation(t, err, "cursor")
	}
	_, err = ParsePage(url.Values{"cursor": {page.NextCursor}}, "projects")
	assertValidation(t, err, "cursor")
	_, err = ParsePage(url.Values{"cursor": {"", ""}}, "runners")
	assertValidation(t, err, "cursor")
	first, err := ParsePage(url.Values{"cursor": {""}}, "runners")
	if err != nil || first.After != "" {
		t.Fatalf("empty cursor=%+v err=%v", first, err)
	}
	for _, rows := range [][]string{nil, {"a", "b"}} {
		last := NewPage(rows, 2, "runners", func(id string) string { return id })
		if last.NextCursor != "" || last.Items == nil {
			t.Fatalf("last page=%+v", last)
		}
	}
}

func assertValidation(t *testing.T, err error, field string) {
	t.Helper()
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "validation_failed" || apiErr.StatusCode() != 400 || len(apiErr.Details) != 1 || apiErr.Details[0].Field != field {
		t.Fatalf("error=%#v; want 400 validation_failed for %s", err, field)
	}
}
