package api

import (
	"io"
	"net/http"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
)

func (f *authFixture) logout(t *testing.T, withCookie bool, csrf string, alter func(*http.Request)) (int, []byte, []*http.Cookie) {
	t.Helper()
	r, err := http.NewRequest("POST", f.server.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", f.server.URL)
	if withCookie {
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
	return response.StatusCode, body, response.Cookies()
}

// Q94: revoke this device and clear its cookie; other devices keep their
// existing sessions and their own CSRF tokens, for both reads and writes.
func TestOwnerLogoutRevokesOnlyCurrentDevice(t *testing.T) {
	f := newAuthFixture(t)
	other := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	if other.status != 200 || len(other.cookies) != 1 {
		t.Fatalf("other device login: %d %s", other.status, other.raw)
	}
	status, body, cookies := f.logout(t, true, f.csrf, nil)
	if status != 204 || len(body) != 0 || len(cookies) != 1 {
		t.Fatalf("logout: %d %s cookies=%v", status, body, cookies)
	}
	cleared := cookies[0]
	if cleared.Name != ownerCookieName || cleared.Value != "" || cleared.Path != "/" || cleared.MaxAge != -1 || !cleared.HttpOnly || cleared.SameSite != http.SameSiteStrictMode || cleared.Secure {
		t.Fatalf("cookie was not cleared correctly: %+v", cleared)
	}
	if countRows(t, f.db, `SELECT count(*) FROM owner_sessions`) != 1 || countRows(t, f.db, `SELECT count(*) FROM owner_sessions WHERE token_hash = ?`, ownerauth.TokenHash(f.cookie.Value)) != 0 {
		t.Fatal("logout did not delete exactly the requesting session")
	}
	for _, path := range []string{"/api/session", "/api/runners"} {
		status, body := f.request(t, "GET", path, true, "", nil)
		assertAuthError(t, status, body, 401, Unauthenticated)
		status, body = ownerCall(t, f.server.Config.Handler, f.auth.host, "GET", path, other.cookies[0], "")
		if status != 200 {
			t.Fatalf("other device read %s: %d %s", path, status, body)
		}
	}
	status, body = f.request(t, "POST", "/api/tasks", true, f.csrf, nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
	if status, body := ownerCall(t, f.server.Config.Handler, f.auth.host, "POST", "/api/tasks", other.cookies[0], other.csrf); status != 204 {
		t.Fatalf("other device write after logout: %d %s", status, body)
	}
	// Replaying logout with the revoked cookie cannot authenticate either.
	status, body, cookies = f.logout(t, true, f.csrf, nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
	if len(cookies) != 0 {
		t.Fatal("rejected logout changed cookies")
	}
}

func TestOwnerLogoutRequiresSessionCSRFAndBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		cookie       bool
		status       int
		alter        func(*http.Request)
	}{
		{"missing cookie", "invalid_session", false, 401, nil},
		{"missing CSRF", "invalid_csrf", true, 403, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"wrong CSRF", "invalid_csrf", true, 403, func(r *http.Request) { r.Header.Set("X-CSRF-Token", "wrong") }},
		{"duplicate CSRF", "invalid_csrf", true, 403, func(r *http.Request) { r.Header.Add("X-CSRF-Token", "other") }},
		{"missing origin", "origin_mismatch", true, 403, func(r *http.Request) { r.Header.Del("Origin") }},
		{"wrong origin", "origin_mismatch", true, 403, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }},
		{"wrong host", "host_mismatch", true, 403, func(r *http.Request) { r.Host = "evil.example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			status, body, cookies := f.logout(t, tc.cookie, f.csrf, tc.alter)
			code := Forbidden
			if tc.status == 401 {
				code = Unauthenticated
			}
			assertAuthError(t, status, body, tc.status, code)
			if len(cookies) != 0 || countRows(t, f.db, `SELECT count(*) FROM owner_sessions`) != 1 {
				t.Fatal("rejected logout changed the session or cookie")
			}
			f.assertAudit(t, "POST", "/auth/logout", tc.reason, tc.status, code)
			if status, body := f.request(t, "GET", "/api/session", true, "", nil); status != 200 {
				t.Fatalf("rejected logout revoked session: %d %s", status, body)
			}
		})
	}
}

// A database write error must fail logout without clearing the cookie or
// revoking another device. The caller can retry with the original session.
func TestOwnerLogoutDeleteFailureFailsClosed(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_logout BEFORE DELETE ON owner_sessions BEGIN SELECT RAISE(FAIL, 'session store unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body, cookies := f.logout(t, true, f.csrf, nil)
	assertAuthError(t, status, body, 500, Internal)
	if len(cookies) != 0 || countRows(t, f.db, `SELECT count(*) FROM owner_sessions`) != 1 {
		t.Fatal("failed logout cleared the cookie or deleted a session")
	}
	if status, body := f.request(t, "GET", "/api/session", true, "", nil); status != 200 {
		t.Fatalf("failed logout revoked session: %d %s", status, body)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_logout`); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := f.logout(t, true, f.csrf, nil); status != 204 {
		t.Fatalf("logout retry: %d %s", status, body)
	}
}
