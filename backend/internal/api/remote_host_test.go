package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
)

const testRemoteHost = "orionmac-mini.tailb76d84.ts.net"

// The HTTP connection still terminates at loopback, as it does behind
// tailscale serve. No TLS or forwarded headers are needed at evaloomd.
func TestRemoteHostLoginSessionAndSecureCookie(t *testing.T) {
	f := newRemoteAuthFixture(t, testRemoteHost, nil)
	remote := func(r *http.Request) {
		r.Host = testRemoteHost
		r.Header.Set("Origin", "https://"+testRemoteHost)
		// Spoofed proxy metadata must not change the validated route.
		r.Header.Set("X-Forwarded-Host", f.auth.host)
		r.Header.Set("X-Forwarded-Proto", "http")
		r.Header.Set("Forwarded", "host="+f.auth.host+";proto=http")
	}
	login := f.passwordLogin(t, testOwnerID, testOwnerPassword, remote)
	if login.status != http.StatusOK || len(login.cookies) != 1 || login.csrf == "" {
		t.Fatalf("remote login: status=%d cookies=%v body=%s", login.status, login.cookies, login.raw)
	}
	cookie := login.cookies[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 3*24*60*60 {
		t.Fatalf("remote cookie attributes: %+v", cookie)
	}
	var via string
	if err := f.db.QueryRow(`SELECT via FROM owner_sessions WHERE token_hash = ?`, ownerauth.TokenHash(cookie.Value)).Scan(&via); err != nil || via != "tailnet" {
		t.Fatalf("remote session via=%q err=%v", via, err)
	}
	rows := auditRows(t, f.db, "auth.login")
	if len(rows) != 1 || rows[0].Detail["via"] != "tailnet" {
		t.Fatalf("remote login audit: %+v", rows)
	}
	f.cookie, f.csrf = cookie, login.csrf
	status, body := f.request(t, "POST", "/api/tasks", true, f.csrf, remote)
	if status != http.StatusNoContent || f.calls.Load() != 1 {
		t.Fatalf("remote mutation: %d %s calls=%d", status, body, f.calls.Load())
	}
	status, body = f.request(t, "GET", "/api/session", true, "", func(r *http.Request) {
		remote(r)
		r.Header.Del("Origin")
	})
	if status != http.StatusOK || !strings.Contains(string(body), login.csrf) {
		t.Fatalf("remote read without Origin: %d %s", status, body)
	}
	status, body, cookies := f.logout(t, true, login.csrf, remote)
	if status != http.StatusNoContent || len(cookies) != 1 || !cookies[0].Secure || cookies[0].MaxAge != -1 {
		t.Fatalf("remote logout: status=%d body=%s cookies=%v", status, body, cookies)
	}
	// Configuring a remote name leaves direct HTTP login cookies non-Secure.
	local := f.passwordLogin(t, testOwnerID, testOwnerPassword, func(r *http.Request) {
		r.Header.Set("X-Forwarded-Host", testRemoteHost)
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if local.status != http.StatusOK || len(local.cookies) != 1 || local.cookies[0].Secure {
		t.Fatalf("loopback login with spoofed headers: %+v", local)
	}
}

func TestRemoteHostBoundaryRejectsAndAuditsNonExactValues(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin, reason string
	}{
		{"suffix host", testRemoteHost + ".evil.example", "https://" + testRemoteHost, "host_mismatch"},
		{"prefix host", "evil." + testRemoteHost, "https://" + testRemoteHost, "host_mismatch"},
		{"wildcard host", "*.tailb76d84.ts.net", "https://" + testRemoteHost, "host_mismatch"},
		{"port host", testRemoteHost + ":443", "https://" + testRemoteHost, "host_mismatch"},
		{"case host", strings.ToUpper(testRemoteHost), "https://" + testRemoteHost, "host_mismatch"},
		{"trailing dot host", testRemoteHost + ".", "https://" + testRemoteHost, "host_mismatch"},
		{"foreign host", "evil.example", "https://" + testRemoteHost, "host_mismatch"},
		{"suffix origin", testRemoteHost, "https://" + testRemoteHost + ".evil.example", "origin_mismatch"},
		{"wildcard origin", testRemoteHost, "https://*.tailb76d84.ts.net", "origin_mismatch"},
		{"http origin", testRemoteHost, "http://" + testRemoteHost, "origin_mismatch"},
		{"port origin", testRemoteHost, "https://" + testRemoteHost + ":443", "origin_mismatch"},
		{"path origin", testRemoteHost, "https://" + testRemoteHost + "/", "origin_mismatch"},
		{"case origin", testRemoteHost, "https://" + strings.ToUpper(testRemoteHost), "origin_mismatch"},
		{"missing origin", testRemoteHost, "", "origin_mismatch"},
		{"null origin", testRemoteHost, "null", "origin_mismatch"},
		{"multiple origins", testRemoteHost, "https://" + testRemoteHost + " https://evil.example", "origin_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoteAuthFixture(t, testRemoteHost, nil)
			status, body := f.request(t, "POST", "/api/tasks?secret=query-canary", true, f.csrf, func(r *http.Request) {
				r.Host = tc.host
				if tc.origin == "" {
					r.Header.Del("Origin")
				} else {
					r.Header.Set("Origin", tc.origin)
				}
				r.Header.Set("X-Forwarded-Host", testRemoteHost)
				r.Header.Set("X-Forwarded-Proto", "https")
			})
			assertAuthError(t, status, body, 403, Forbidden)
			f.assertAudit(t, "POST", "/api/tasks", tc.reason, 403, Forbidden)
		})
	}
}

func TestRemoteHostRequiresConfigurationAndMatchingOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, configured, host, origin, reason string
		duplicateOrigin                        bool
	}{
		{"disabled host", "", testRemoteHost, "https://" + testRemoteHost, "host_mismatch", false},
		{"disabled origin", "", "", "https://" + testRemoteHost, "origin_mismatch", false},
		{"remote with local origin", testRemoteHost, testRemoteHost, "", "origin_mismatch", false},
		{"local with remote origin", testRemoteHost, "", "https://" + testRemoteHost, "origin_mismatch", false},
		{"duplicate origin", testRemoteHost, testRemoteHost, "https://" + testRemoteHost, "origin_mismatch", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoteAuthFixture(t, tc.configured, nil)
			login := f.passwordLogin(t, testOwnerID, testOwnerPassword, func(r *http.Request) {
				if tc.host != "" {
					r.Host = tc.host
				}
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
				if tc.duplicateOrigin {
					r.Header.Add("Origin", tc.origin)
				}
			})
			assertAuthError(t, login.status, login.raw, 403, Forbidden)
			f.assertAudit(t, "POST", "/auth/login", tc.reason, 403, Forbidden)
			var sessions int
			if err := f.db.QueryRow(`SELECT count(*) FROM owner_sessions`).Scan(&sessions); err != nil || sessions != 1 || len(login.cookies) != 0 {
				t.Fatalf("rejected login created session: count=%d cookies=%v err=%v", sessions, login.cookies, err)
			}
		})
	}
}

func TestRemoteHostConfigurationRejectsWildcardAndMultipleNames(t *testing.T) {
	db := runnerDB(t, 0)
	for _, hosts := range [][]string{
		{"*.tailb76d84.ts.net"}, {"https://" + testRemoteHost}, {testRemoteHost + ":443"},
		{testRemoteHost + ",other.ts.net"}, {testRemoteHost, "other.ts.net"},
		{"127.0.0.1"}, {"::1"}, {" " + testRemoteHost}, {testRemoteHost + "/"},
		{"a..ts.net"}, {"-a.ts.net"}, {"a_.ts.net"},
	} {
		t.Run(strings.Join(hosts, ","), func(t *testing.T) {
			if _, err := NewOwnerAuth(db, nil, "127.0.0.1:8080", hosts...); err == nil {
				t.Fatalf("accepted invalid remote host configuration: %q", hosts)
			}
		})
	}
}
