package securitytest

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	remoteAccessHost     = "orionmac-mini.tailb76d84.ts.net"
	remoteAccessPassword = "remote access test password"
	remoteAccessPatch    = `{"linear":{"triggerLabel":"remote-access-test"}}`
)

type remoteAccessControl struct {
	db         *sql.DB
	path       string
	server     *httptest.Server
	via        string
	remoteHost string
}

func newRemoteAccessControl(t *testing.T, via string) *remoteAccessControl {
	t.Helper()
	f := &remoteAccessControl{path: filepath.Join(t.TempDir(), "control.db"), via: via}
	if via == "tailnet" {
		f.remoteHost = remoteAccessHost
	}
	t.Cleanup(func() { f.close(t) })
	f.open(t)
	hash, err := ownerauth.HashPassword([]byte(remoteAccessPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwnerCredentials(t.Context(), f.db, "owner", hash); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *remoteAccessControl) open(t *testing.T) {
	t.Helper()
	var err error
	f.db, err = store.Open(t.Context(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(nil)
	auth, err := api.NewOwnerAuth(f.db, nil, f.server.Listener.Addr().String(), f.remoteHost)
	if err != nil {
		t.Fatal(err)
	}
	// Use the production router, without replacing any business handlers.
	f.server.Config.Handler = api.NewHandler(f.db, nil, auth)
	f.server.Start()
}

func (f *remoteAccessControl) close(t *testing.T) {
	t.Helper()
	if f.server != nil {
		f.server.Close()
		f.server = nil
	}
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			t.Error(err)
		}
		f.db = nil
	}
}

func (f *remoteAccessControl) request(t *testing.T, method, path, body string, cookie *http.Cookie, csrf string, alter func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", f.server.URL)
	if f.via == "tailnet" {
		r.Host = remoteAccessHost
		r.Header.Set("Origin", "https://"+remoteAccessHost)
	}
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if alter != nil {
		alter(r)
	}
	client := &http.Client{Transport: f.server.Client().Transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, raw
}

func (f *remoteAccessControl) login(t *testing.T, password string) (*http.Cookie, string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"id": "owner", "password": password})
	if err != nil {
		t.Fatal(err)
	}
	response, raw := f.request(t, "POST", "/auth/login", string(data), nil, "", nil)
	var body struct{ CSRFToken string }
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("login JSON: %v: %s", err, raw)
	}
	if response.StatusCode != http.StatusOK || len(response.Cookies()) != 1 || body.CSRFToken == "" {
		t.Fatalf("login: HTTP %d: %s cookies=%v", response.StatusCode, raw, response.Cookies())
	}
	return response.Cookies()[0], body.CSRFToken
}

func remoteAccessError(t *testing.T, response *http.Response, raw []byte, status int, code api.Code) {
	t.Helper()
	var body struct{ Error api.Error }
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("error JSON: %v: %s", err, raw)
	}
	if response.StatusCode != status || body.Error.Code != code {
		t.Fatalf("HTTP %d: %s; want HTTP %d %s", response.StatusCode, raw, status, code)
	}
	if len(response.Cookies()) != 0 || response.Header.Get("Location") != "" {
		t.Fatalf("rejected request set credentials or redirected: %v", response.Header)
	}
}

func (f *remoteAccessControl) assertRejection(t *testing.T, method, path, reason string, status int) {
	t.Helper()
	var target, detail string
	if err := f.db.QueryRow(`SELECT target_id, detail FROM audit_logs WHERE action = 'auth.reject' ORDER BY id DESC LIMIT 1`).Scan(&target, &detail); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Method, Reason string
		Status         int
	}
	if err := json.Unmarshal([]byte(detail), &got); err != nil {
		t.Fatal(err)
	}
	if target != path || got.Method != method || got.Reason != reason || got.Status != status {
		t.Fatalf("rejection audit: target=%q detail=%s", target, detail)
	}
}

// Spec 18 chapter 6 adds these six cases to spec 14's central boundary tests.
// These exercise HTTP authentication, not Sandbox network isolation; they run
// in make verify without Docker or model credentials.
func TestRemoteAccessPermissionBoundaries(t *testing.T) {
	for _, via := range []string{"loopback", "tailnet"} {
		t.Run(via, func(t *testing.T) {
			for _, test := range []struct {
				name string
				run  func(*testing.T, *remoteAccessControl)
			}{
				{"untrusted_and_lookalike_hosts", remoteAccessRejectHosts},
				{"expired_session", remoteAccessExpiredSession},
				{"restart_preserves_session", remoteAccessRestart},
				{"password_reset_revokes_sessions", remoteAccessReset},
				{"retired_login_url", remoteAccessRetiredURL},
				{"runner_cannot_call_owner_apis", remoteAccessRunnerBoundary},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, newRemoteAccessControl(t, via)) })
			}
		})
	}
}

func remoteAccessRejectHosts(t *testing.T, f *remoteAccessControl) {
	cookie, csrf := f.login(t, remoteAccessPassword)
	for _, host := range []string{"evil.example", remoteAccessHost + ".evil.example"} {
		for _, route := range []struct{ method, path, body string }{
			{"POST", "/auth/login", `{"id":"owner","password":"` + remoteAccessPassword + `"}`},
			{"GET", "/api/session", ""},
			{"PATCH", "/api/settings", remoteAccessPatch},
		} {
			response, raw := f.request(t, route.method, route.path, route.body, cookie, csrf, func(r *http.Request) {
				r.Host = host
			})
			remoteAccessError(t, response, raw, 403, api.Forbidden)
			f.assertRejection(t, route.method, route.path, "host_mismatch", 403)
		}
	}
	if n := remoteAccessCount(t, f.db, "SELECT count(*) FROM owner_sessions"); n != 1 {
		t.Fatalf("rejected login changed sessions: %d", n)
	}
	if n := remoteAccessCount(t, f.db, "SELECT count(*) FROM settings"); n != 0 {
		t.Fatal("rejected mutation changed settings")
	}
}

func remoteAccessExpiredSession(t *testing.T, f *remoteAccessControl) {
	cookie, csrf := f.login(t, remoteAccessPassword)
	response, raw := f.request(t, "GET", "/api/session", "", cookie, "", nil)
	if response.StatusCode != 200 {
		t.Fatalf("session before expiry: %d %s", response.StatusCode, raw)
	}
	// Age the stored login, keeping its specified three-day lifetime. No sleep
	// or test-only clock/API is needed in the production authentication path.
	created := time.Now().UTC().Add(-73 * time.Hour)
	if _, err := f.db.Exec(`UPDATE owner_sessions SET created_at = ?, expires_at = ? WHERE token_hash = ?`,
		created.Format(time.RFC3339), created.Add(72*time.Hour).Format(time.RFC3339), ownerauth.TokenHash(cookie.Value)); err != nil {
		t.Fatal(err)
	}
	remoteAccessRejectSession(t, f, cookie, csrf)
	f.login(t, remoteAccessPassword) // Expiry must not disable new logins.
}

func remoteAccessRejectSession(t *testing.T, f *remoteAccessControl, cookie *http.Cookie, csrf string) {
	t.Helper()
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/session", ""}, {"GET", "/api/runners", ""}, {"PATCH", "/api/settings", remoteAccessPatch},
	} {
		response, raw := f.request(t, route.method, route.path, route.body, cookie, csrf, nil)
		remoteAccessError(t, response, raw, 401, api.Unauthenticated)
		f.assertRejection(t, route.method, route.path, "invalid_session", 401)
	}
	if n := remoteAccessCount(t, f.db, "SELECT count(*) FROM settings"); n != 0 {
		t.Fatal("invalid session changed settings")
	}
}

func remoteAccessRestart(t *testing.T, f *remoteAccessControl) {
	cookie, csrf := f.login(t, remoteAccessPassword)
	before, err := store.OwnerSessionByTokenHash(t.Context(), f.db, ownerauth.TokenHash(cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	f.close(t)
	f.open(t) // Reopen the file and build fresh auth/router/server instances.
	after, err := store.OwnerSessionByTokenHash(t.Context(), f.db, ownerauth.TokenHash(cookie.Value))
	if err != nil || !reflect.DeepEqual(before, after) || after.Via != f.via {
		t.Fatalf("session changed on restart: before=%+v after=%+v err=%v", before, after, err)
	}
	response, raw := f.request(t, "GET", "/api/session", "", cookie, "", nil)
	var body struct{ CSRFToken string }
	if err := json.Unmarshal(raw, &body); err != nil || response.StatusCode != 200 || body.CSRFToken != csrf {
		t.Fatalf("session after restart: %d %s err=%v", response.StatusCode, raw, err)
	}
	response, raw = f.request(t, "PATCH", "/api/settings", remoteAccessPatch, cookie, "", nil)
	remoteAccessError(t, response, raw, 403, api.Forbidden)
	response, raw = f.request(t, "PATCH", "/api/settings", remoteAccessPatch, cookie, csrf, nil)
	if response.StatusCode != 200 || remoteAccessCount(t, f.db, "SELECT count(*) FROM settings") != 1 {
		t.Fatalf("mutation after restart: %d %s", response.StatusCode, raw)
	}
}

func remoteAccessReset(t *testing.T, f *remoteAccessControl) {
	first, firstCSRF := f.login(t, remoteAccessPassword)
	second, secondCSRF := f.login(t, remoteAccessPassword)
	const newPassword = "replacement remote access password"
	hash, err := ownerauth.HashPassword([]byte(newPassword))
	if err != nil {
		t.Fatal(err)
	}
	// This is the atomic credential-reset operation used by the owner CLI.
	if err := store.SetOwnerCredentials(t.Context(), f.db, "owner", hash); err != nil {
		t.Fatal(err)
	}
	remoteAccessRejectSession(t, f, first, firstCSRF)
	remoteAccessRejectSession(t, f, second, secondCSRF)
	if remoteAccessCount(t, f.db, "SELECT count(*) FROM owner_sessions") != 0 {
		t.Fatal("password reset left sessions behind")
	}
	cookie, _ := f.login(t, newPassword)
	response, raw := f.request(t, "GET", "/api/runners", "", cookie, "", nil)
	if response.StatusCode != 200 {
		t.Fatalf("new session after reset: %d %s", response.StatusCode, raw)
	}
}

func remoteAccessRetiredURL(t *testing.T, f *remoteAccessControl) {
	for _, accept := range []string{"application/json", "text/html"} {
		response, raw := f.request(t, "GET", "/auth/login?token=retired-token", "", nil, "", func(r *http.Request) {
			r.Header.Set("Accept", accept)
		})
		remoteAccessError(t, response, raw, 404, api.NotFound)
	}
	if remoteAccessCount(t, f.db, "SELECT count(*) FROM owner_sessions") != 0 ||
		remoteAccessCount(t, f.db, "SELECT count(*) FROM audit_logs WHERE action = 'auth.login'") != 0 {
		t.Fatal("retired URL logged in or created a session")
	}
}

func remoteAccessRunnerBoundary(t *testing.T, f *remoteAccessControl) {
	token, err := runnerauth.Register(t.Context(), f.db, "test-runner", "Test Runner", "/worktrees")
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	response, raw := f.request(t, "POST", "/api/runners/test-runner/claim", "", nil, "", bearer)
	if response.StatusCode != 204 {
		t.Fatalf("Runner positive control: %d %s", response.StatusCode, raw)
	}
	// All owner endpoints in specs 01 5.1, 12/15/17 5 and 18 5. A 404 or
	// validation error would mean the request got past the auth boundary.
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/session"}, {"POST", "/auth/logout"},
		{"GET", "/api/projects"}, {"POST", "/api/projects"},
		{"GET", "/api/projects/example"}, {"PATCH", "/api/projects/example"},
		{"POST", "/api/projects/example/harnesses"},
		{"GET", "/api/harnesses"}, {"POST", "/api/harnesses"}, {"PATCH", "/api/harnesses/example"},
		{"GET", "/api/harness-profiles"}, {"POST", "/api/harness-profiles"},
		{"GET", "/api/tasks"}, {"POST", "/api/tasks"}, {"GET", "/api/tasks/task-001"},
		{"POST", "/api/tasks/task-001/cancel"}, {"POST", "/api/tasks/task-001/retry"}, {"POST", "/api/tasks/task-001/resume"},
		{"GET", "/api/executions/example"}, {"GET", "/api/executions/example/events"},
		{"GET", "/api/executions/example/logs"}, {"GET", "/api/executions/example/artifacts/example"},
		{"GET", "/api/runners"}, {"GET", "/api/notification-failures"}, {"GET", "/api/linear/status"},
		{"GET", "/api/settings"}, {"PATCH", "/api/settings"},
		{"GET", "/api/approvals"}, {"GET", "/api/approvals/example"},
		{"POST", "/api/approvals/example/decision"}, {"POST", "/api/approvals/example/renew"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response, raw := f.request(t, route.method, route.path, remoteAccessPatch, nil, "", bearer)
			remoteAccessError(t, response, raw, 403, api.Forbidden)
			f.assertRejection(t, route.method, route.path, "wrong_auth_class", 403)
		})
	}
}

func remoteAccessCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
