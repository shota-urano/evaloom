package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	testOwnerID       = "orion"
	testOwnerPassword = "correct horse battery staple"
)

func setOwnerPassword(t *testing.T, db *sql.DB, id, password string) {
	t.Helper()
	hash, err := ownerauth.HashPassword([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwnerCredentials(t.Context(), db, id, hash); err != nil {
		t.Fatal(err)
	}
}

type loginResult struct {
	status  int
	raw     []byte
	csrf    string
	cookies []*http.Cookie
}

func (f *authFixture) passwordLogin(t *testing.T, id, password string, alter func(*http.Request)) loginResult {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"id": id, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("POST", f.server.URL+"/auth/login", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", f.server.URL)
	r.Header.Set("Content-Type", "application/json")
	if alter != nil {
		alter(r)
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	result := loginResult{status: response.StatusCode, raw: raw, cookies: response.Cookies()}
	if response.StatusCode == 200 {
		var body struct {
			CSRFToken string `json:"csrfToken"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		result.csrf = body.CSRFToken
	}
	return result
}

// ownerCall calls an owner API with an explicit cookie and CSRF token.
func ownerCall(t *testing.T, handler http.Handler, host, method, path string, cookie *http.Cookie, csrf string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	r.Header.Set("Origin", "http://"+host)
	r.AddCookie(cookie)
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

type auditRow struct {
	Actor, Action, TargetType, TargetID string
	Detail                              map[string]any
}

func auditRows(t *testing.T, db *sql.DB, action string) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT actor, action, target_type, target_id, detail FROM audit_logs WHERE action = ? ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		var detail string
		if err := rows.Scan(&row.Actor, &row.Action, &row.TargetType, &row.TargetID, &detail); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(detail), &row.Detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 受け入れ基準: 正しい ID とパスワードで Cookie を受け取り本人用 API を呼べる。
func TestOwnerPasswordLoginCookieCallsOwnerAPIs(t *testing.T) {
	f := newAuthFixture(t)
	if status, body := f.request(t, "GET", "/api/runners", true, "", nil); status != 200 {
		t.Fatalf("owner read after login: %d %s", status, body)
	}
	if status, body := f.request(t, "POST", "/api/tasks", true, f.csrf, nil); status != 204 {
		t.Fatalf("owner write after login: %d %s", status, body)
	}
	status, body := f.request(t, "GET", "/api/session", true, "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"csrfToken":"`+f.csrf+`"`)) {
		t.Fatalf("session endpoint: %d %s", status, body)
	}
	// Only hashes are stored (spec 18 3.5, 3.8).
	var id, tokenHash, csrfHash, created, expires, via string
	if err := f.db.QueryRow(`SELECT id, token_hash, csrf_token_hash, created_at, expires_at, via FROM owner_sessions`).Scan(
		&id, &tokenHash, &csrfHash, &created, &expires, &via); err != nil {
		t.Fatal(err)
	}
	if tokenHash != ownerauth.TokenHash(f.cookie.Value) || csrfHash != ownerauth.TokenHash(f.csrf) || via != "loopback" {
		t.Fatalf("stored session: token=%q csrf=%q via=%q", tokenHash, csrfHash, via)
	}
	if err := ValidateID("id", id, ULID); err != nil {
		t.Fatalf("session id %q is not a ULID", id)
	}
	createdAt, err1 := time.Parse(time.RFC3339, created)
	expiresAt, err2 := time.Parse(time.RFC3339, expires)
	if err1 != nil || err2 != nil || expiresAt.Sub(createdAt) != 72*time.Hour {
		t.Fatalf("session lifetime: created=%q expires=%q", created, expires)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM owner_sessions WHERE token_hash = ? OR csrf_token_hash = ? OR id = ?`,
		f.cookie.Value, f.csrf, f.cookie.Value); n != 0 {
		t.Fatal("raw session or CSRF value stored in owner_sessions")
	}
}

// 受け入れ基準: 違うと 401 で、どちらが違うかを返さない。
func TestOwnerPasswordLoginRejectsWithoutRevealingWhichFieldIsWrong(t *testing.T) {
	f := newAuthFixture(t)
	sessions := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`)
	cases := []struct{ name, id, password string }{
		{"wrong id", "someone-else", testOwnerPassword},
		{"wrong password", testOwnerID, "incorrect horse battery"},
		{"both wrong", "someone-else", "incorrect horse battery"},
		{"empty", "", ""},
		{"id case differs", strings.ToUpper(testOwnerID), testOwnerPassword},
	}
	var first []byte
	for _, tc := range cases {
		login := f.passwordLogin(t, tc.id, tc.password, nil)
		assertAuthError(t, login.status, login.raw, 401, Unauthenticated)
		if len(login.cookies) != 0 {
			t.Fatalf("%s: failed login set cookies %v", tc.name, login.cookies)
		}
		if first == nil {
			first = login.raw
		} else if !bytes.Equal(first, login.raw) {
			t.Fatalf("%s: response differs between failure causes: %s vs %s", tc.name, first, login.raw)
		}
		for _, word := range []string{`"field":"id"`, `"field":"password"`} {
			if bytes.Contains(login.raw, []byte(word)) {
				t.Fatalf("%s: response names the wrong field: %s", tc.name, login.raw)
			}
		}
	}
	if got := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`); got != sessions {
		t.Fatalf("failed logins created sessions: %d -> %d", sessions, got)
	}
}

// 受け入れ基準: Control Plane を再起動しても期限内のセッションで本人用 API を呼べる。
// The process-level restart is covered by cmd/evaloomd; this checks that a new
// OwnerAuth with no in-memory state accepts the stored session.
func TestOwnerSessionSurvivesNewOwnerAuth(t *testing.T) {
	f := newAuthFixture(t)
	restarted, err := NewOwnerAuth(f.db, nil, f.auth.host)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(f.db, nil, restarted)
	if status, body := ownerCall(t, handler, f.auth.host, "GET", "/api/runners", f.cookie, ""); status != 200 {
		t.Fatalf("read after restart: %d %s", status, body)
	}
	status, body := ownerCall(t, handler, f.auth.host, "GET", "/api/session", f.cookie, "")
	if status != 200 || !bytes.Contains(body, []byte(f.csrf)) {
		t.Fatalf("CSRF token after restart: %d %s", status, body)
	}
	if status, body := ownerCall(t, handler, f.auth.host, "PATCH", "/api/settings", f.cookie, "wrong"); status != 403 {
		t.Fatalf("restart must keep CSRF validation: %d %s", status, body)
	}
}

// 受け入れ基準: ログインから3日を過ぎたセッションは 401。
func TestOwnerSessionExpiresThreeDaysAfterLogin(t *testing.T) {
	f := newAuthFixture(t)
	var created string
	if err := f.db.QueryRow(`SELECT created_at FROM owner_sessions`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	loginAt, err := time.Parse(time.RFC3339, created)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		after time.Duration
		want  int
	}{
		{72*time.Hour - time.Second, 200}, // use does not extend the session (Q92)
		{72 * time.Hour, 401},
		{72*time.Hour + time.Second, 401},
		{30 * 24 * time.Hour, 401},
	} {
		f.auth.now = func() time.Time { return loginAt.Add(tc.after) }
		status, body := f.request(t, "GET", "/api/runners", true, "", nil)
		if tc.want == 401 {
			assertAuthError(t, status, body, 401, Unauthenticated)
		} else if status != tc.want {
			t.Fatalf("after %v: %d %s", tc.after, status, body)
		}
		// An expired session cannot fetch its CSRF token either.
		if tc.want == 401 {
			if status, body := f.request(t, "GET", "/api/session", true, "", nil); status != 401 {
				t.Fatalf("expired session returned CSRF token: %d %s", status, body)
			}
		}
	}
	rejects := auditRows(t, f.db, "auth.reject")
	if len(rejects) == 0 || rejects[0].Detail["reason"] != "invalid_session" {
		t.Fatalf("expired session rejection not audited: %+v", rejects)
	}
}

// The login cookie lasts exactly as long as the stored session.
func TestOwnerLoginCookieMaxAgeMatchesSessionLifetime(t *testing.T) {
	f := newAuthFixture(t)
	if f.cookie.MaxAge != int((72*time.Hour).Seconds()) || !f.cookie.HttpOnly || f.cookie.SameSite != http.SameSiteStrictMode || f.cookie.Path != "/" || f.cookie.Secure {
		t.Fatalf("cookie attributes: %+v", f.cookie)
	}
}

// 受け入れ基準: 端末ごとに別のセッションができ、片方を使ってももう片方が無効にならない。
func TestOwnerEachLoginCreatesIndependentSession(t *testing.T) {
	f := newAuthFixture(t)
	other := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	if other.status != 200 || len(other.cookies) != 1 {
		t.Fatalf("second device login: %d %s", other.status, other.raw)
	}
	phone, phoneCSRF := other.cookies[0], other.csrf
	if phone.Value == f.cookie.Value || phoneCSRF == f.csrf {
		t.Fatal("second login reused the first device's session or CSRF token")
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`); n != 2 {
		t.Fatalf("sessions=%d; want 2", n)
	}
	handler := f.server.Config.Handler
	host := f.auth.host
	for i, step := range []struct {
		cookie *http.Cookie
		csrf   string
	}{{f.cookie, f.csrf}, {phone, phoneCSRF}, {f.cookie, f.csrf}, {phone, phoneCSRF}} {
		if status, body := ownerCall(t, handler, host, "POST", "/api/tasks", step.cookie, step.csrf); status != 204 {
			t.Fatalf("step %d: %d %s", i, status, body)
		}
	}
	// A CSRF token belongs to its own session only.
	if status, body := ownerCall(t, handler, host, "POST", "/api/tasks", phone, f.csrf); status != 403 {
		t.Fatalf("cross-session CSRF accepted: %d %s", status, body)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`); n != 2 {
		t.Fatalf("using one session changed the others: sessions=%d", n)
	}
}

// 受け入れ基準: ログインの成功・失敗が audit_logs に残る。
func TestOwnerLoginSuccessAndFailureAreAudited(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.db.Exec(`DELETE FROM owner_sessions`); err != nil {
		t.Fatal(err)
	}
	wrong := "incorrect horse battery"
	if login := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil); login.status != 200 {
		t.Fatalf("login: %d %s", login.status, login.raw)
	}
	f.passwordLogin(t, "someone-else", wrong, nil)
	successes := auditRows(t, f.db, "auth.login")
	var sessionID string
	if err := f.db.QueryRow(`SELECT id FROM owner_sessions`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	want := []auditRow{{Actor: "owner", Action: "auth.login", TargetType: "owner_sessions", TargetID: sessionID,
		Detail: map[string]any{"method": "POST", "via": "loopback"}}}
	if !reflect.DeepEqual(successes, want) {
		t.Fatalf("success audit=%+v; want %+v", successes, want)
	}
	failures := auditRows(t, f.db, "auth.login_failure")
	wantFailure := []auditRow{{Actor: "system", Action: "auth.login_failure", TargetType: "request", TargetID: "/auth/login",
		Detail: map[string]any{"method": "POST", "reason": "invalid_credentials", "via": "loopback", "code": "unauthenticated", "status": float64(401)}}}
	if !reflect.DeepEqual(failures, wantFailure) {
		t.Fatalf("failure audit=%+v; want %+v", failures, wantFailure)
	}
	var all string
	if err := f.db.QueryRow(`SELECT group_concat(target_id || detail, ' ') FROM audit_logs`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testOwnerPassword, wrong, "someone-else", f.cookie.Value, f.csrf} {
		if strings.Contains(all+f.logs.String(), secret) {
			t.Fatalf("audit or log contains submitted credential %q", secret)
		}
	}
	// Malformed requests are failures too.
	r, err := http.NewRequest("POST", f.server.URL+"/auth/login", strings.NewReader(`{"id":"orion","password":"x","extra":1}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", f.server.URL)
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	failures = auditRows(t, f.db, "auth.login_failure")
	if response.StatusCode != 400 || len(failures) != 2 || failures[1].Detail["reason"] != "invalid_request" {
		t.Fatalf("malformed login: status=%d audit=%+v", response.StatusCode, failures)
	}
}

// If the audit record cannot be written, the login fails closed.
func TestOwnerLoginAuditFailureFailsClosed(t *testing.T) {
	for _, password := range []string{testOwnerPassword, "incorrect horse battery"} {
		f := newAuthFixture(t)
		before := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`)
		if _, err := f.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		login := f.passwordLogin(t, testOwnerID, password, nil)
		assertAuthError(t, login.status, login.raw, 500, Internal)
		if len(login.cookies) != 0 || countRows(t, f.db, `SELECT count(*) FROM owner_sessions`) != before {
			t.Fatalf("login without audit issued a session: cookies=%v", login.cookies)
		}
	}
}

func TestOwnerLoginWithoutPasswordIsRefused(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.db.Exec(`DELETE FROM owner_credentials`); err != nil {
		t.Fatal(err)
	}
	login := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	assertAuthError(t, login.status, login.raw, 401, Unauthenticated)
	if !bytes.Contains(login.raw, []byte(`"reason":"not_set"`)) || !bytes.Contains(login.raw, []byte("evaloomd owner set-password")) || len(login.cookies) != 0 {
		t.Fatalf("password-not-set response: %s", login.raw)
	}
	failures := auditRows(t, f.db, "auth.login_failure")
	if len(failures) != 1 || failures[0].Detail["reason"] != "password_not_set" {
		t.Fatalf("audit=%+v", failures)
	}
}

// Login keeps the Host / Origin boundary of 01 3.4.
func TestOwnerLoginRequiresAllowedOrigin(t *testing.T) {
	for name, alter := range map[string]func(*http.Request){
		"missing origin": func(r *http.Request) { r.Header.Del("Origin") },
		"foreign origin": func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		"foreign host":   func(r *http.Request) { r.Host = "evil.example" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAuthFixture(t)
			login := f.passwordLogin(t, testOwnerID, testOwnerPassword, alter)
			assertAuthError(t, login.status, login.raw, 403, Forbidden)
			if len(login.cookies) != 0 || countRows(t, f.db, `SELECT count(*) FROM owner_sessions`) != 1 {
				t.Fatal("rejected login created a session")
			}
		})
	}
}

// Resetting the password revokes every stored session (spec 18 3.3, 6章).
func TestOwnerPasswordResetRevokesStoredSessions(t *testing.T) {
	f := newAuthFixture(t)
	setOwnerPassword(t, f.db, testOwnerID, "another long password")
	status, body := f.request(t, "GET", "/api/runners", true, "", nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
	if login := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil); login.status != 401 {
		t.Fatalf("old password still accepted: %d", login.status)
	}
	if login := f.passwordLogin(t, testOwnerID, "another long password", nil); login.status != 200 {
		t.Fatalf("new password rejected: %d %s", login.status, login.raw)
	}
}

// Expired rows are removed when a new session is created.
func TestOwnerLoginRemovesExpiredSessions(t *testing.T) {
	f := newAuthFixture(t)
	f.auth.now = func() time.Time { return time.Now().Add(73 * time.Hour) }
	if login := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil); login.status != 200 {
		t.Fatalf("login: %d %s", login.status, login.raw)
	}
	if n := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`); n != 1 {
		t.Fatalf("sessions=%d; want only the new one", n)
	}
	status, body := f.request(t, "GET", "/api/runners", true, "", nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
}

// An unreadable session store never authenticates anyone (Fail Closed).
func TestOwnerSessionStoreFailureFailsClosed(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.db.Exec(`ALTER TABLE owner_sessions RENAME TO owner_sessions_gone`); err != nil {
		t.Fatal(err)
	}
	status, body := f.request(t, "POST", "/api/tasks", true, f.csrf, nil)
	assertAuthError(t, status, body, 500, Internal)
	if f.calls.Load() != 0 {
		t.Fatal("request reached the handler without a readable session")
	}
}
