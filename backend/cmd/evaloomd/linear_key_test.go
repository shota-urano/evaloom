package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// linearKeyCanary is a fake API key; it must never reach the logs or the DB.
const linearKeyCanary = "lin_api_EVALOOM_DCJ2_FAKE_KEY_b81e40"

// dumpDB returns every value of every table as text, to search for a leak.
func dumpDB(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var out strings.Builder
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range values {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				fmt.Fprintf(&out, "%s|%v\n", table, v)
			}
		}
		rows.Close()
	}
	return out.String()
}

// The production wiring keeps the API key value out of the logs and the DB
// through an unusable key file, an authentication error that echoes the key,
// and successful rounds that create a task (spec 15 3.1, 6章).
func TestLinearAPIKeyNeverReachesLogsOrDB(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setupLinearProject(t, db)

	fake := &fakeLinear{issues: map[string]map[string]any{}}
	var rejectAuth atomic.Bool
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != linearKeyCanary {
			t.Errorf("Authorization header is not the key from the file")
		}
		if rejectAuth.Load() {
			w.Header().Set("X-Echo", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{
				"message": "invalid key " + linearKeyCanary, "extensions": map[string]any{"code": "AUTHENTICATION_ERROR"}}}})
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	keyPath := filepath.Join(t.TempDir(), linear.APIKeyFileName)
	if err := os.WriteFile(keyPath, []byte(linearKeyCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	poller := linearPoller(db, client, logger)
	lastError := func() string {
		t.Helper()
		var class sql.NullString
		if err := db.QueryRow(`SELECT last_error FROM linear_poll_teams WHERE team_id = 'team-a'`).Scan(&class); err != nil {
			t.Fatal(err)
		}
		return class.String
	}

	// Key file not 0600: no request is sent, last_error is auth, the reason is logged.
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	poller.PollOnce(ctx)
	if calls.Load() != 0 || lastError() != "auth" || !strings.Contains(logs.String(), string(linear.KeyPermission)) {
		t.Fatalf("unusable key: calls=%d last_error=%q logs=%q", calls.Load(), lastError(), logs.String())
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}

	// Linear rejects the key and echoes it back.
	rejectAuth.Store(true)
	poller.PollOnce(ctx)
	if calls.Load() == 0 || lastError() != "auth" {
		t.Fatalf("auth error: calls=%d last_error=%q", calls.Load(), lastError())
	}

	// Successful rounds: baseline, then an attached Issue becomes a task.
	rejectAuth.Store(false)
	if _, err := poller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fake.set("issue-1", linearIssueJSON("issue-1", "## Acceptance Criteria\n- ok",
		time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "ai-ready", "route:p"))
	if _, err := poller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var tasks int
	if err := db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks=%d err=%v", tasks, err)
	}

	dump := dumpDB(t, db)
	if !strings.Contains(dump, "issue-1") {
		t.Fatalf("DB dump does not cover the intake tables: %q", dump)
	}
	for name, text := range map[string]string{"logs": logs.String(), "db": dump} {
		if strings.Contains(text, linearKeyCanary) {
			t.Fatalf("API key value leaked into %s", name)
		}
	}
}
