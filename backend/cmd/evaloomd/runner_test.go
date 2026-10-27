package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func registrationArgs(path, id string) []string {
	return []string{"-db", path, "runner", "register", "-worktree-root", "/private/worktrees", "-name", "Mac mini", id}
}

// Exercise the issued CLI credential after closing and reopening the real DB,
// then cross both HTTP authentication boundaries and inspect persisted rows.
func TestRunnerRegistrationAndHTTPAuthenticationIsolation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evaloom.db")
	var output bytes.Buffer
	if err := run(ctx, registrationArgs(path, "mac-mini"), &output); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSuffix(output.String(), "\n")
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != 32 || strings.Count(output.String(), "\n") != 1 {
		t.Fatal("registration must display one 256-bit token, once")
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id, name, hash, root, status, heartbeat, created string
	if err := db.QueryRow(`SELECT id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at FROM runners`).Scan(&id, &name, &hash, &root, &status, &heartbeat, &created); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	if id != "mac-mini" || name != "Mac mini" || hash != hex.EncodeToString(digest[:]) || root != "/private/worktrees" || status != "offline" || heartbeat != "" {
		t.Fatal("registered Runner does not match the hash-only, offline contract")
	}
	if _, err := api.ParseTimestamp(created); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	auth, err := api.NewOwnerAuth(db, nil, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = api.NewHandler(db, nil, auth)
	server.Start()
	defer server.Close()
	login, err := server.Client().Get(auth.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	cookies := login.Cookies()
	login.Body.Close()
	if login.StatusCode != 200 || len(cookies) != 1 {
		t.Fatal("owner login failed")
	}
	for _, tc := range []struct {
		name, method, path, code string
		owner, runner            bool
		status                   int
	}{
		{"Runner token on owner API", "GET", "/api/runners", "forbidden", false, true, 403},
		{"owner session on Runner API", "POST", "/api/runners/mac-mini/claim", "forbidden", true, false, 403},
		{"Runner token on Runner API", "POST", "/api/runners/mac-mini/claim", "", false, true, 204},
		{"owner session on owner API", "GET", "/api/runners", "", true, false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(tc.method, server.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.owner {
				r.AddCookie(cookies[0])
			}
			if tc.runner {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			// A real Runner sends neither browser Origin nor CSRF credentials.
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if tc.status == http.StatusNoContent {
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != tc.status || len(body) != 0 {
					t.Fatalf("empty claim status=%d body=%s err=%v", response.StatusCode, body, err)
				}
				return
			}
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d body=%v; want %d", response.StatusCode, body, tc.status)
			}
			if tc.code != "" {
				envelope, ok := body["error"].(map[string]any)
				if !ok || envelope["code"] != tc.code || envelope["details"] == nil {
					t.Fatalf("wrong common error envelope: %v", body)
				}
			} else {
				items := body["items"].([]any)
				if len(items) != 1 || items[0].(map[string]any)["lastHeartbeatAt"] != nil {
					t.Fatalf("new offline Runner must have no heartbeat: %v", body)
				}
				encoded, _ := json.Marshal(body)
				if bytes.Contains(encoded, []byte(hash)) || bytes.Contains(encoded, []byte(token)) {
					t.Fatal("owner list exposed Runner credentials")
				}
			}
		})
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'auth.reject' AND json_extract(detail, '$.reason') = 'wrong_auth_class'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("authentication class rejections not audited: count=%d err=%v", count, err)
	}
	assertNoPlaintextInRows(t, db, token)
	// Re-registration cannot silently rotate an already deployed credential.
	output.Reset()
	if err := run(ctx, registrationArgs(path, "mac-mini"), &output); !errors.Is(err, store.ErrRunnerExists) || output.Len() != 0 {
		t.Fatalf("duplicate registration: err=%v output length=%d", err, output.Len())
	}
	var after string
	if err := db.QueryRow(`SELECT token_hash FROM runners WHERE id = 'mac-mini'`).Scan(&after); err != nil || after != hash {
		t.Fatal("duplicate registration changed the credential")
	}
}

func assertNoPlaintextInRows(t *testing.T, db *sql.DB, token string) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close() // store serializes on one connection; release before next query.
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				text := fmt.Sprint(value)
				if blob, ok := value.([]byte); ok {
					text = string(blob)
				}
				if strings.Contains(text, token) {
					t.Fatalf("plaintext token found in %s.%s", table, columns[i])
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
}

func TestRunnerRegisterRejectsInvalidInputBeforeCreatingDatabase(t *testing.T) {
	for _, tc := range []struct{ name, id, root string }{
		{"empty id", "", "/worktrees"}, {"multiple segments", "other/runner", "/worktrees"},
		{"dot id", "..", "/worktrees"}, {"whitespace", "runner one", "/worktrees"},
		{"control", "runner\n", "/worktrees"}, {"relative root", "mac-mini", "worktrees"},
		{"missing root", "mac-mini", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaloom.db")
			var output bytes.Buffer
			err := run(context.Background(), []string{"-db", path, "runner", "register", "-worktree-root", tc.root, tc.id}, &output)
			if err == nil || output.Len() != 0 {
				t.Fatal("invalid registration succeeded or printed a token")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid registration created a database: %v", err)
			}
		})
	}
}

func TestRunnerRegistrationWriteFailureDoesNotDisplayToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER deny_registration BEFORE INSERT ON runners BEGIN SELECT RAISE(FAIL, 'registration unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(context.Background(), registrationArgs(path, "mac-mini"), &output); err == nil || output.Len() != 0 {
		t.Fatalf("failed registration: err=%v output length=%d", err, output.Len())
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM runners`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed registration left a Runner: count=%d err=%v", count, err)
	}
}
