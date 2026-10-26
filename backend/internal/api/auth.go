package api

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

const ownerCookieName = "evaloom_session"

// OwnerAuth holds process-local credentials. Restarting evaloomd invalidates
// the session and issues a new one-time login URL. Never log this struct.
type OwnerAuth struct {
	db                              *sql.DB
	logger                          *slog.Logger
	host, session, csrf, loginToken string
	loginMu                         sync.Mutex
	loginUsed                       bool
}

func NewOwnerAuth(db *sql.DB, logger *slog.Logger, host string) (*OwnerAuth, error) {
	ip, port, err := net.SplitHostPort(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ip != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, fmt.Errorf("api: owner authentication requires 127.0.0.1:<listening port>")
	}
	if db == nil {
		return nil, fmt.Errorf("api: owner authentication requires an audit database")
	}
	a := &OwnerAuth{db: db, logger: logger, host: host}
	for _, target := range []*string{&a.session, &a.csrf, &a.loginToken} {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("api: generate owner credential: %w", err)
		}
		*target = base64.RawURLEncoding.EncodeToString(secret)
	}
	return a, nil
}

// LoginURL is a credential: expose it only through the owner's startup terminal.
func (a *OwnerAuth) LoginURL() string {
	return "http://" + a.host + "/auth/login?token=" + a.loginToken
}

func secretEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func changesState(method string) bool {
	// Unknown methods also fail closed rather than acquiring safe-method status.
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// Boundary validates the actual listening authority, never forwarded headers.
// Browser navigations and same-origin reads can omit Origin; writes cannot.
func (a *OwnerAuth) Boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Host != a.host {
			a.reject(w, r, Forbidden, "host_mismatch", "request host is not allowed")
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && origins[0] != "http://"+a.host) || (len(origins) == 0 && changesState(r.Method)) {
			a.reject(w, r, Forbidden, "origin_mismatch", "request origin is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Protect must surround owner routes, including reads, before handler side effects.
func (a *OwnerAuth) Protect(next http.Handler) http.Handler {
	return a.Boundary(a.protect(next))
}

func (a *OwnerAuth) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sessions []string
		for _, cookie := range r.Cookies() {
			if cookie.Name == ownerCookieName {
				sessions = append(sessions, cookie.Value)
			}
		}
		if len(sessions) != 1 || !secretEqual(sessions[0], a.session) {
			a.reject(w, r, Unauthenticated, "invalid_session", "owner session is missing or invalid")
			return
		}
		tokens := r.Header.Values("X-CSRF-Token")
		if changesState(r.Method) && (len(tokens) != 1 || !secretEqual(tokens[0], a.csrf)) {
			a.reject(w, r, Forbidden, "invalid_csrf", "CSRF token is missing or invalid")
			return
		}
		if r.URL.Path == "/api/session" && r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, struct {
				CSRFToken string `json:"csrfToken"`
			}{a.csrf})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *OwnerAuth) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.reject(w, r, Forbidden, "invalid_login", "login requires the one-time URL")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	tokens := query["token"]
	a.loginMu.Lock()
	valid := err == nil && len(tokens) == 1 && !a.loginUsed && secretEqual(tokens[0], a.loginToken)
	if valid {
		a.loginUsed = true
	}
	a.loginMu.Unlock()
	if !valid {
		a.reject(w, r, Unauthenticated, "invalid_login", "login URL is invalid or already used")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ownerCookieName, Value: a.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, struct {
		CSRFToken string `json:"csrfToken"`
	}{a.csrf})
}

func (a *OwnerAuth) reject(w http.ResponseWriter, r *http.Request, code Code, reason, message string) {
	// Neither query strings nor raw headers/bodies belong in the audit record:
	// they may contain login, session, CSRF, or Runner credentials.
	_, err := store.RecordAuditLog(r.Context(), a.db, store.AuditLog{
		Actor: "system", Action: "auth.reject", TargetType: "request", TargetID: r.URL.Path,
		Detail: map[string]any{"method": r.Method, "reason": reason, "code": code, "status": (&Error{Code: code}).StatusCode()},
	})
	if err != nil {
		WriteError(w, a.logger, fmt.Errorf("api: record authentication rejection: %w", err))
		return
	}
	WriteError(w, a.logger, &Error{Code: code, Message: message})
}
