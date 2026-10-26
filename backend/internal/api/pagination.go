package api

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	DefaultLimit = 50
	MaxLimit     = 200
)

type PageRequest struct {
	Limit int
	After string
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor"`
}

// ParsePage accepts only positive decimal limits up to MaxLimit. Cursors are
// opaque, resource-bound base64url tokens holding an exclusive ID boundary.
// An absent or empty cursor starts at the beginning; malformed tokens are 400.
func ParsePage(query url.Values, resource string) (PageRequest, error) {
	page := PageRequest{Limit: DefaultLimit}
	if values, ok := query["limit"]; ok {
		if len(values) != 1 || values[0] == "" || strings.Trim(values[0], "0123456789") != "" {
			return page, invalid("limit", "invalid")
		}
		limit, err := strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > MaxLimit {
			return page, invalid("limit", "out_of_range")
		}
		page.Limit = limit
	}
	if values, ok := query["cursor"]; ok {
		if len(values) != 1 {
			return page, invalid("cursor", "invalid")
		}
		if values[0] != "" {
			decoded, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
			prefix := resource + "\n"
			if err != nil || !utf8.Valid(decoded) || !strings.HasPrefix(string(decoded), prefix) {
				return page, invalid("cursor", "invalid")
			}
			page.After = strings.TrimPrefix(string(decoded), prefix)
			if page.After == "" || strings.ContainsAny(page.After, "\x00\r\n") {
				return page, invalid("cursor", "invalid")
			}
		}
	}
	return page, nil
}

// NewPage consumes at most limit+1 rows, using the extra row only to determine
// whether another page exists. End-of-list is represented by nextCursor: "".
func NewPage[T any](rows []T, limit int, resource string, id func(T) string) Page[T] {
	page := Page[T]{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(resource + "\n" + id(rows[limit-1])))
	}
	if page.Items == nil {
		page.Items = []T{}
	}
	return page
}
