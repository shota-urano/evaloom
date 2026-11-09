package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const ownerCookieName = "evaloom_session"

// OwnerAuth authenticates the owner with sessions stored in owner_sessions
// (spec 18 3.4, 3.5), so sessions survive a Control Plane restart until they
// expire. Never log this struct.
type OwnerAuth struct {
	db         *sql.DB
	logger     *slog.Logger
	host       string
	remoteHost string
	// now is replaced only by tests that check session expiry and login cooldown.
	now func() time.Time
	// There is one owner, and proxy addresses cannot identify devices. Keep one
	// process-local counter; spec 18 defines no persistent throttle storage.
	// Serialize credential checks and updates so concurrent attempts cannot
	// pass the fifth failure before its cooldown has been recorded.
	loginMu           sync.Mutex
	loginFailures     int
	loginBlockedUntil time.Time
}

func NewOwnerAuth(db *sql.DB, logger *slog.Logger, host string, remoteHosts ...string) (*OwnerAuth, error) {
	ip, port, err := net.SplitHostPort(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ip != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, fmt.Errorf("api: owner authentication requires 127.0.0.1:<listening port>")
	}
	if db == nil {
		return nil, fmt.Errorf("api: owner authentication requires an audit database")
	}
	if len(remoteHosts) > 1 {
		return nil, fmt.Errorf("api: at most one remote host is allowed")
	}
	var remoteHost string
	if len(remoteHosts) == 1 {
		remoteHost = remoteHosts[0]
	}
	if remoteHost != "" && !validRemoteHost(remoteHost) {
		return nil, fmt.Errorf("api: remote host must be a single DNS name without a scheme, port, or wildcard")
	}
	return &OwnerAuth{db: db, logger: logger, host: host, remoteHost: remoteHost, now: time.Now}, nil
}

func validRemoteHost(host string) bool {
	if len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func secretEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func changesState(method string) bool {
	// Unknown methods also fail closed rather than acquiring safe-method status.
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// Boundary validates the listening authority or the configured HTTPS proxy
// authority (spec 18 3.2), never forwarded headers.
// Browser navigations and same-origin reads can omit Origin; writes cannot.
func (a *OwnerAuth) Boundary(next http.Handler) http.Handler {
	return a.boundary(next, true)
}

// Runner clients do not send browser Origin/CSRF credentials. Host checks and
// validation of any supplied Origin still apply to both authentication classes.
func (a *OwnerAuth) boundary(next http.Handler, requireOrigin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		var origin string
		switch {
		case r.Host == a.host:
			origin = "http://" + a.host
		case a.remoteHost != "" && r.Host == a.remoteHost:
			origin = "https://" + a.remoteHost
		default:
			a.reject(w, r, Forbidden, "host_mismatch", "request host is not allowed")
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && origins[0] != origin) || (len(origins) == 0 && requireOrigin && changesState(r.Method)) {
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
		if len(r.Header.Values("Authorization")) != 0 {
			id, err := a.runnerIdentity(r)
			if err != nil {
				WriteError(w, a.logger, err)
			} else if id == "" {
				a.reject(w, r, Unauthenticated, "invalid_runner_token", "Runner token is missing or invalid")
			} else {
				a.reject(w, r, Forbidden, "wrong_auth_class", "Runner credentials cannot access owner APIs")
			}
			return
		}
		token, session, err := a.ownerSession(r)
		if err != nil {
			WriteError(w, a.logger, err)
			return
		}
		if token == "" {
			a.reject(w, r, Unauthenticated, "invalid_session", "owner session is missing or invalid")
			return
		}
		tokens := r.Header.Values("X-CSRF-Token")
		if changesState(r.Method) && (len(tokens) != 1 || !secretEqual(ownerauth.TokenHash(tokens[0]), session.CSRFTokenHash)) {
			a.reject(w, r, Forbidden, "invalid_csrf", "CSRF token is missing or invalid")
			return
		}
		if r.URL.Path == "/api/session" && r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, struct {
				CSRFToken string `json:"csrfToken"`
			}{ownerauth.CSRFToken(token)})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ownerSession returns an empty token for an absent, ambiguous, malformed,
// unknown or expired session cookie. Database errors stay errors so that an
// unreadable session store never authenticates anyone.
func (a *OwnerAuth) ownerSession(r *http.Request) (string, store.OwnerSession, error) {
	var values []string
	for _, cookie := range r.Cookies() {
		if cookie.Name == ownerCookieName {
			values = append(values, cookie.Value)
		}
	}
	if len(values) != 1 || !ownerauth.CanonicalSessionToken(values[0]) {
		return "", store.OwnerSession{}, nil
	}
	session, err := store.OwnerSessionByTokenHash(r.Context(), a.db, ownerauth.TokenHash(values[0]))
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.OwnerSession{}, nil
	}
	if err != nil {
		return "", store.OwnerSession{}, err
	}
	if !a.now().Before(session.ExpiresAt) {
		return "", store.OwnerSession{}, nil
	}
	return values[0], session, nil
}

// runnerIdentity returns an empty ID for absent, malformed, or unknown tokens.
// Infrastructure errors remain errors, never a successful authentication.
func (a *OwnerAuth) runnerIdentity(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", nil
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", nil
	}
	id, err := runnerauth.Authenticate(r.Context(), a.db, parts[1])
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

type runnerIdentityKey struct{}

func (a *OwnerAuth) protectRunner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _, err := a.ownerSession(r)
		if err != nil {
			WriteError(w, a.logger, err)
			return
		}
		if token != "" {
			a.reject(w, r, Forbidden, "wrong_auth_class", "owner credentials cannot access Runner APIs")
			return
		}
		id, err := a.runnerIdentity(r)
		if err != nil {
			WriteError(w, a.logger, err)
			return
		}
		if id == "" {
			a.reject(w, r, Unauthenticated, "invalid_runner_token", "Runner token is missing or invalid")
			return
		}
		if target := r.PathValue("runnerID"); target != "" && target != id {
			a.reject(w, r, Forbidden, "runner_mismatch", "Runner cannot access another Runner")
			return
		}
		// Execution handlers verify ownership again inside their saving transaction.
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), runnerIdentityKey{}, id)))
	})
}

func (a *OwnerAuth) login(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.passwordLogin(w, r)
	case http.MethodGet:
		WriteError(w, a.logger, &Error{Code: NotFound, Message: "API endpoint not found"})
	default:
		a.reject(w, r, Forbidden, "invalid_login", "login requires POST /auth/login")
	}
}

// maxLoginBody bounds the unauthenticated request body.
const maxLoginBody = 64 << 10

// Spec 18 3.4, Q91: the fifth consecutive mismatch starts a fixed cooldown.
const (
	maxLoginFailures = 5
	loginCooldown    = 60 * time.Second
)

// passwordLogin implements POST /auth/login (spec 18 3.4). Wrong IDs and wrong
// passwords produce the same response, and the password hash is always
// evaluated so the timing does not reveal whether the ID matched.
func (a *OwnerAuth) passwordLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	// Read untrusted input before taking the lock, so a slow body cannot hold
	// up other attempts. Check cooldown before any credential lookup or hash.
	decodeErr := DecodeJSON(r, &body)
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.now().Before(a.loginBlockedUntil) {
		// 01 5.3 has no separate rate-limit code; retain the existing auth
		// error envelope with the HTTP status required by 18 6章.
		a.loginFailureStatus(w, r, "login_cooldown", &Error{Code: Unauthenticated,
			Message: "login is temporarily unavailable after repeated failures"}, http.StatusTooManyRequests)
		return
	}
	if !a.loginBlockedUntil.IsZero() {
		a.loginFailures = 0
		a.loginBlockedUntil = time.Time{}
	}
	if decodeErr != nil {
		var public *Error
		if !errors.As(decodeErr, &public) {
			public = invalid("body", "invalid_json").(*Error)
		}
		a.loginFailure(w, r, "invalid_request", public)
		return
	}
	id, hash, err := store.OwnerCredentials(r.Context(), a.db)
	if errors.Is(err, sql.ErrNoRows) {
		a.loginFailure(w, r, "password_not_set", &Error{Code: Unauthenticated,
			Message: "owner password is not set; run `evaloomd owner set-password` on the Mac mini",
			Details: []Detail{{Field: "owner_credentials", Reason: "not_set"}}})
		return
	}
	if err != nil {
		WriteError(w, a.logger, err)
		return
	}
	password := []byte(body.Password)
	defer clear(password)
	passwordOK, err := ownerauth.VerifyPassword(password, hash)
	if err != nil {
		WriteError(w, a.logger, err)
		return
	}
	if !secretEqual(body.ID, id) || !passwordOK {
		a.loginFailures++
		if a.loginFailures == maxLoginFailures {
			a.loginBlockedUntil = a.now().Add(loginCooldown)
		}
		a.loginFailure(w, r, "invalid_credentials", &Error{Code: Unauthenticated, Message: "ID or password is incorrect"})
		return
	}
	token, err := a.startSession(w, r)
	if err != nil {
		WriteError(w, a.logger, err)
		return
	}
	a.loginFailures = 0
	writeJSON(w, http.StatusOK, struct {
		CSRFToken string `json:"csrfToken"`
	}{ownerauth.CSRFToken(token)})
}

// via names the login route recorded in owner_sessions. Boundary has already
// validated Host; TLS terminates at tailscale serve, not at this HTTP listener.
func (a *OwnerAuth) via(r *http.Request) string {
	if r.Host == a.host {
		return "loopback"
	}
	return "tailnet"
}

// startSession stores a new per-device session with its success audit record
// and sets the cookie only after both are committed (fail closed).
func (a *OwnerAuth) startSession(w http.ResponseWriter, r *http.Request) (string, error) {
	token, err := ownerauth.NewSessionToken()
	if err != nil {
		return "", err
	}
	now := a.now().UTC().Truncate(time.Second)
	session := store.OwnerSession{
		ID: ulid.Make().String(), TokenHash: ownerauth.TokenHash(token),
		CSRFTokenHash: ownerauth.TokenHash(ownerauth.CSRFToken(token)), Via: a.via(r),
		CreatedAt: now, ExpiresAt: now.Add(ownerauth.SessionLifetime),
	}
	if err := store.CreateOwnerSession(r.Context(), a.db, session, store.AuditLog{
		Actor: "owner", Action: "auth.login", TargetType: "owner_sessions", TargetID: session.ID,
		Detail: map[string]any{"method": r.Method, "via": session.Via},
	}); err != nil {
		return "", fmt.Errorf("api: start owner session: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name: ownerCookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(session.ExpiresAt.Sub(now) / time.Second), Secure: session.Via == "tailnet",
	})
	return token, nil
}

// loginFailure records a failed login attempt without the submitted ID or
// password, then returns public. A failed audit write fails closed with 500.
func (a *OwnerAuth) loginFailure(w http.ResponseWriter, r *http.Request, reason string, public *Error) {
	a.loginFailureStatus(w, r, reason, public, public.StatusCode())
}

func (a *OwnerAuth) loginFailureStatus(w http.ResponseWriter, r *http.Request, reason string, public *Error, status int) {
	_, err := store.RecordAuditLog(r.Context(), a.db, store.AuditLog{
		Actor: "system", Action: "auth.login_failure", TargetType: "request", TargetID: r.URL.Path,
		Detail: map[string]any{"method": r.Method, "reason": reason, "via": a.via(r), "code": public.Code, "status": status},
	})
	if err != nil {
		WriteError(w, a.logger, fmt.Errorf("api: record login failure: %w", err))
		return
	}
	writeErrorResponse(w, status, public)
}

// logout is reached only through Protect, including its per-session CSRF check.
func (a *OwnerAuth) logout(w http.ResponseWriter, r *http.Request) {
	_, session, err := a.ownerSession(r)
	if err != nil {
		WriteError(w, a.logger, err)
		return
	}
	if session.ID == "" {
		a.reject(w, r, Unauthenticated, "invalid_session", "owner session is missing or invalid")
		return
	}
	if err := store.DeleteOwnerSession(r.Context(), a.db, session.ID); err != nil {
		WriteError(w, a.logger, err)
		return
	}
	// Clear the browser cookie only after the deletion succeeds (fail closed).
	http.SetCookie(w, &http.Cookie{
		Name: ownerCookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0).UTC(), Secure: session.Via == "tailnet",
	})
	w.WriteHeader(http.StatusNoContent)
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
