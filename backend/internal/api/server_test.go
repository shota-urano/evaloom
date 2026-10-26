package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func runnerDB(t *testing.T, count int) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Insert in reverse order so tests cannot pass by relying on insertion order.
	for i := count; i > 0; i-- {
		id := fmt.Sprintf("runner-%03d", i)
		_, err := db.Exec(`INSERT INTO runners
			(id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
			VALUES (?, ?, 'sensitive-token-hash', '/private/worktrees', 'online', ?, ?)`,
			id, "Runner "+id, "2026-10-26T00:00:00Z", "2026-10-25T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func readPage(t *testing.T, handler http.Handler, query string) Page[runnerResponse] {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/runners"+query, nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sensitive-token-hash") || strings.Contains(w.Body.String(), "tokenHash") || strings.Contains(w.Body.String(), "token_hash") || strings.Contains(w.Body.String(), "/private/worktrees") {
		t.Fatalf("private runner data exposed: %s", w.Body.String())
	}
	var page Page[runnerResponse]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil {
		t.Fatal("items must be an array")
	}
	return page
}

func TestRunnerListSQLitePagination(t *testing.T) {
	db := runnerDB(t, 5)
	handler := NewHandler(db, nil)
	var ids []string
	query := "?limit=2"
	for index, wantCount := range []int{2, 2, 1} {
		page := readPage(t, handler, query)
		if len(page.Items) != wantCount {
			t.Fatalf("page %d: got %d rows; want %d", index, len(page.Items), wantCount)
		}
		for _, runner := range page.Items {
			ids = append(ids, runner.ID)
		}
		if index < 2 && page.NextCursor == "" {
			t.Fatalf("page %d missing cursor", index)
		}
		if index == 2 && page.NextCursor != "" {
			t.Fatalf("last cursor=%q; want empty", page.NextCursor)
		}
		query = "?limit=2&cursor=" + url.QueryEscape(page.NextCursor)
	}
	want := []string{"runner-001", "runner-002", "runner-003", "runner-004", "runner-005"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("IDs=%v; want %v without gaps or duplicates", ids, want)
	}
	page := readPage(t, handler, "?limit=2")
	if _, err := db.Exec("DELETE FROM runners WHERE id IN ('runner-001', 'runner-002')"); err != nil {
		t.Fatal(err)
	}
	page = readPage(t, handler, "?limit=200&cursor="+url.QueryEscape(page.NextCursor))
	if len(page.Items) != 3 || page.Items[0].ID != "runner-003" || page.NextCursor != "" {
		t.Fatalf("deleted cursor boundary=%+v", page)
	}
}

func TestRunnerListDefaultAndMaximumLimits(t *testing.T) {
	handler := NewHandler(runnerDB(t, 201), nil)
	for _, tc := range []struct {
		query string
		count int
	}{{"", 50}, {"?limit=200", 200}} {
		page := readPage(t, handler, tc.query)
		if len(page.Items) != tc.count || page.NextCursor == "" {
			t.Fatalf("query=%q count=%d cursor=%q", tc.query, len(page.Items), page.NextCursor)
		}
	}
}

func TestRunnerListEmptyAndExactLastPage(t *testing.T) {
	db := runnerDB(t, 2)
	handler := NewHandler(db, nil)
	page := readPage(t, handler, "?limit=2")
	if len(page.Items) != 2 || page.NextCursor != "" {
		t.Fatalf("exact page=%+v", page)
	}
	if _, err := db.Exec("DELETE FROM runners"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/runners", nil))
	if w.Code != 200 || w.Body.String() != "{\"items\":[],\"nextCursor\":\"\"}\n" {
		t.Fatalf("empty page: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRunnerListJSONFieldAndTimeContract(t *testing.T) {
	handler := NewHandler(runnerDB(t, 1), nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/runners", nil))
	const want = `{"items":[{"id":"runner-001","name":"Runner runner-001","status":"online","lastHeartbeatAt":"2026-10-26T00:00:00Z","createdAt":"2026-10-25T00:00:00Z"}],"nextCursor":""}` + "\n"
	if w.Code != 200 || w.Body.String() != want {
		t.Fatalf("status=%d body=%s; want %s", w.Code, w.Body.String(), want)
	}
}

func TestRunnerListRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	db := runnerDB(t, 0)
	db.Close()
	handler := NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct{ query, field string }{
		{"limit=201", "limit"}, {"limit=0", "limit"}, {"limit=-1", "limit"}, {"limit=oops", "limit"},
		{"limit=1&limit=2", "limit"}, {"cursor=invalid!", "cursor"}, {"cursor=a&cursor=b", "cursor"},
		{"limit=2&cursor=%zz", "query"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/runners?"+tc.query, nil))
			var envelope struct {
				Error Error `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if w.Code != 400 || envelope.Error.Code != "validation_failed" || len(envelope.Error.Details) != 1 || envelope.Error.Details[0].Field != tc.field {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRunnerListDatabaseFailuresAreInternal(t *testing.T) {
	for _, failure := range []string{"closed database", "invalid timestamp"} {
		t.Run(failure, func(t *testing.T) {
			db := runnerDB(t, 1)
			if failure == "closed database" {
				db.Close()
			} else if _, err := db.Exec("UPDATE runners SET created_at = 'invalid'"); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			w := httptest.NewRecorder()
			NewHandler(db, slog.New(slog.NewTextHandler(&logs, nil))).ServeHTTP(w, httptest.NewRequest("GET", "/api/runners", nil))
			if w.Code != 500 || w.Body.String() != "{\"error\":{\"code\":\"internal\",\"message\":\"internal server error\",\"details\":[]}}\n" || logs.Len() == 0 {
				t.Fatalf("status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
			}
		})
	}
}

func TestUnknownRouteUsesErrorEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	NewHandler(runnerDB(t, 0), nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/missing", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"error":{"code":"not_found"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
