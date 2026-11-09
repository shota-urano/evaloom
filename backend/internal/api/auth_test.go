package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
)

type authFixture struct {
	adapters *adapter.Registry
	db       *sql.DB
	auth     *OwnerAuth
	server   *httptest.Server
	cookie   *http.Cookie
	csrf     string
	calls    atomic.Int32
	logs     bytes.Buffer
	settings *linearintake.LabelSettings
}

func newAuthFixture(t *testing.T, contexts ...ContextConfig) *authFixture {
	return newConfiguredAuthFixture(t, nil, contexts...)
}

func newConfiguredAuthFixture(t *testing.T, configure func(*sql.DB) ArtifactConfig, contexts ...ContextConfig) *authFixture {
	t.Helper()
	f := &authFixture{db: runnerDB(t, 0), adapters: &adapter.Registry{}}
	f.settings = linearintake.NewLabelSettings(f.db, nil)
	f.server = httptest.NewUnstartedServer(nil)
	var err error
	f.auth, err = NewOwnerAuth(f.db, slog.New(slog.NewTextHandler(&f.logs, nil)), f.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// Business endpoints are separate work. A probe verifies that rejected
	// requests never reach a handler capable of changing state.
	mux.Handle("/api/tasks", f.auth.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})))
	var config ContextConfig
	if len(contexts) > 0 {
		config = contexts[0]
	}
	var artifacts ArtifactConfig
	if configure != nil {
		artifacts = configure(f.db)
	}
	mux.Handle("/", NewHandlerWithSettings(f.db, nil, f.auth, nil, config, artifacts, f.settings, f.adapters))
	f.server.Config.Handler = mux
	f.server.Start()
	t.Cleanup(f.server.Close)
	setOwnerPassword(t, f.db, testOwnerID, testOwnerPassword)
	login := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	if login.status != 200 || len(login.cookies) != 1 || login.csrf == "" {
		t.Fatalf("login status=%d cookies=%d body=%s", login.status, len(login.cookies), login.raw)
	}
	f.cookie, f.csrf = login.cookies[0], login.csrf
	// Tests observe audit_logs from a clean state; the setup's password and
	// login records are checked by the dedicated login tests.
	if _, err := f.db.Exec(`DELETE FROM audit_logs`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *authFixture) request(t *testing.T, method, path string, cookie bool, csrf string, alter func(*http.Request)) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, f.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", f.server.URL)
	if cookie {
		r.AddCookie(f.cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if alter != nil {
		alter(r)
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("credential response may be cached or leaked: %v", response.Header)
	}
	return response.StatusCode, body
}

func assertAuthError(t *testing.T, status int, body []byte, wantStatus int, code Code) {
	t.Helper()
	var envelope struct {
		Error Error `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("invalid error JSON: %s: %v", body, err)
	}
	if status != wantStatus || envelope.Error.Code != code || envelope.Error.Message == "" || envelope.Error.Details == nil {
		t.Fatalf("status=%d body=%s; want %d %s with details array", status, body, wantStatus, code)
	}
}

func (f *authFixture) assertAudit(t *testing.T, method, path, reason string, status int, code Code) {
	t.Helper()
	var id int64
	var actor, action, targetType, targetID, detail, created string
	if err := f.db.QueryRow(`SELECT id, actor, action, target_type, target_id, detail, created_at FROM audit_logs WHERE action = 'auth.reject'`).Scan(
		&id, &actor, &action, &targetType, &targetID, &detail, &created); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if id <= 0 || count != 1 || actor != "system" || action != "auth.reject" || targetType != "request" || targetID != path {
		t.Fatalf("audit row: id=%d count=%d actor=%q action=%q type=%q target=%q", id, count, actor, action, targetType, targetID)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(detail), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"method": method, "reason": reason, "status": float64(status), "code": string(code)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit detail=%v; want %v", got, want)
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil || !strings.HasSuffix(created, "Z") {
		t.Fatalf("invalid audit time %q: %v", created, err)
	}
	for _, secret := range []string{f.cookie.Value, f.csrf, "query-canary"} {
		if strings.Contains(detail+targetID+f.logs.String(), secret) {
			t.Fatal("credential leaked into audit/logs")
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("rejected request reached state-changing handler")
	}
}

func TestOwnerWithoutCookieCannotChangeState(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			f := newAuthFixture(t)
			status, body := f.request(t, method, "/api/tasks?secret=query-canary", false, f.csrf, nil)
			assertAuthError(t, status, body, 401, Unauthenticated)
			f.assertAudit(t, method, "/api/tasks", "invalid_session", 401, Unauthenticated)
		})
	}
}

func TestOwnerCSRFIsRequiredAndValidated(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, value := range []string{"", "wrong-token"} {
			t.Run(method+"/"+value, func(t *testing.T) {
				f := newAuthFixture(t)
				status, body := f.request(t, method, "/api/tasks", true, value, nil)
				assertAuthError(t, status, body, 403, Forbidden)
				f.assertAudit(t, method, "/api/tasks", "invalid_csrf", 403, Forbidden)
			})
		}
	}
}

func TestOwnerRejectsForeignOriginAndHost(t *testing.T) {
	for _, tc := range []struct{ name, header, value, reason string }{
		{"foreign", "Origin", "http://evil.example", "origin_mismatch"},
		{"localhost", "Origin", "http://localhost:8080", "origin_mismatch"},
		{"wrong port", "Origin", "http://127.0.0.1:1", "origin_mismatch"},
		{"TLS", "Origin", "https://127.0.0.1:8080", "origin_mismatch"},
		{"null", "Origin", "null", "origin_mismatch"},
		{"missing origin", "Origin", "", "origin_mismatch"},
		{"foreign host", "Host", "evil.example:8080", "host_mismatch"},
		{"localhost host", "Host", "localhost:8080", "host_mismatch"},
		{"wrong host port", "Host", "127.0.0.1:1", "host_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			status, body := f.request(t, "POST", "/api/tasks", true, f.csrf, func(r *http.Request) {
				if tc.header == "Host" {
					r.Host = tc.value
				} else if tc.value == "" {
					r.Header.Del(tc.header)
				} else {
					r.Header.Set(tc.header, tc.value)
				}
				r.Header.Set("X-Forwarded-Host", f.auth.host)
				r.Header.Set("X-Forwarded-Proto", "http")
			})
			assertAuthError(t, status, body, 403, Forbidden)
			f.assertAudit(t, "POST", "/api/tasks", tc.reason, 403, Forbidden)
		})
	}
	// Origin validation also applies to reads and the retired login URL.
	for _, path := range []string{"/api/runners", "/auth/login?token=secret"} {
		f := newAuthFixture(t)
		status, body := f.request(t, "GET", path, true, "", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
		assertAuthError(t, status, body, 403, Forbidden)
	}
}

func TestOwnerValidSessionAndCSRFAllowRequests(t *testing.T) {
	f := newAuthFixture(t)
	if !f.cookie.HttpOnly || f.cookie.SameSite != http.SameSiteStrictMode || f.cookie.Path != "/" || f.cookie.Domain != "" || f.cookie.MaxAge != 3*24*60*60 || f.cookie.Secure {
		t.Fatalf("invalid owner cookie attributes: %+v", f.cookie)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		status, body := f.request(t, method, "/api/tasks", true, f.csrf, nil)
		if status != 204 {
			t.Fatalf("allowed mutation: %d %s", status, body)
		}
	}
	for _, path := range []string{"/api/runners", "/api/session"} {
		status, body := f.request(t, "GET", path, true, "", func(r *http.Request) { r.Header.Del("Origin") })
		if status != 200 {
			t.Fatalf("allowed read: %d %s", status, body)
		}
		if path == "/api/session" && !bytes.Contains(body, []byte(f.csrf)) {
			t.Fatal("session endpoint did not return CSRF token")
		}
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected rejection: count=%d err=%v", count, err)
	}
}

func TestOwnerRejectsInvalidOrAmbiguousCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		alter        func(*http.Request)
	}{
		{"invalid cookie", "invalid_session", func(r *http.Request) { r.Header.Set("Cookie", ownerCookieName+"=invalid") }},
		{"duplicate cookie", "invalid_session", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: ownerCookieName, Value: "invalid"}) }},
		{"duplicate csrf", "invalid_csrf", func(r *http.Request) { r.Header.Add("X-CSRF-Token", "other") }},
		{"duplicate origin", "origin_mismatch", func(r *http.Request) { r.Header.Add("Origin", "http://evil.example") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			status, body := f.request(t, "POST", "/api/tasks", true, f.csrf, tc.alter)
			code, want := Forbidden, 403
			if tc.reason == "invalid_session" {
				code, want = Unauthenticated, 401
			}
			assertAuthError(t, status, body, want, code)
			f.assertAudit(t, "POST", "/api/tasks", tc.reason, want, code)
		})
	}
	f := newAuthFixture(t)
	status, body := f.request(t, "GET", "/api/runners", false, "", nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
}

// Spec 18 3.6: GET login never creates a session, including browser navigation.
func TestOwnerLoginURLIsRetired(t *testing.T) {
	f := newAuthFixture(t)
	for _, path := range []string{"/auth/login", "/auth/login?token=query-canary", "/auth/login?token=", "/auth/login?token=a&token=b"} {
		for _, accept := range []string{"application/json", "text/html"} {
			for _, authenticated := range []bool{false, true} {
				r, err := http.NewRequest("GET", f.server.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Accept", accept)
				if authenticated {
					r.AddCookie(f.cookie)
				}
				response, err := f.server.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				assertAuthError(t, response.StatusCode, body, 404, NotFound)
				if len(response.Cookies()) != 0 || response.Header.Get("Location") != "" {
					t.Fatalf("retired URL sets credentials or redirects: %v", response.Header)
				}
			}
		}
	}
	var sessions, logins int
	if err := f.db.QueryRow("SELECT count(*) FROM owner_sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM audit_logs WHERE action = 'auth.login'").Scan(&logins); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || logins != 0 || strings.Contains(f.logs.String(), "query-canary") {
		t.Fatalf("retired URL changed sessions or leaked token: sessions=%d logins=%d logs=%s", sessions, logins, f.logs.String())
	}
}

func TestOwnerAuditFailureFailsClosed(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := f.request(t, "POST", "/api/tasks", true, "", nil)
	assertAuthError(t, status, body, 500, Internal)
	if f.calls.Load() != 0 || !strings.Contains(f.logs.String(), "audit unavailable") || bytes.Contains(body, []byte("audit unavailable")) {
		t.Fatalf("audit failure: calls=%d logs=%s body=%s", f.calls.Load(), f.logs.String(), body)
	}
}
