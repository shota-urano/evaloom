package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

func registerAuthRunner(t *testing.T, f *authFixture) string {
	t.Helper()
	token, err := runnerauth.Register(context.Background(), f.db, "mac-mini", "Mac mini", "/worktrees")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRunnerRejectsInvalidBearerCredentials(t *testing.T) {
	for _, test := range []string{"missing", "unknown", "malformed", "wrong scheme", "extra field", "duplicate", "comma joined", "noncanonical"} {
		t.Run(test, func(t *testing.T) {
			f := newAuthFixture(t)
			token := registerAuthRunner(t, f)
			status, body := f.request(t, "POST", "/api/runners/mac-mini/claim?secret=query-canary", false, "", func(r *http.Request) {
				r.Header.Del("Origin")
				value := "Bearer " + token
				switch test {
				case "missing":
					return
				case "unknown":
					value = "Bearer " + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				case "malformed":
					value = "Bearer invalid-token"
				case "wrong scheme":
					value = "Basic " + token
				case "extra field":
					value += " other"
				case "comma joined":
					value += ", Bearer " + token
				case "noncanonical":
					value += "="
				}
				r.Header.Set("Authorization", value)
				if test == "duplicate" {
					r.Header.Add("Authorization", value)
				}
			})
			assertAuthError(t, status, body, 401, Unauthenticated)
			f.assertAudit(t, "POST", "/api/runners/mac-mini/claim", "invalid_runner_token", 401, Unauthenticated)
			var detail string
			if err := f.db.QueryRow(`SELECT detail FROM audit_logs`).Scan(&detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail+f.logs.String()+string(body), token) {
				t.Fatal("Runner token leaked into rejection records")
			}
		})
	}
}

func TestOwnerCannotAccessAnyDeclaredRunnerAPI(t *testing.T) {
	for _, path := range []string{
		"/api/runners/mac-mini/claim", "/api/runners/mac-mini/heartbeat",
		"/api/executions/example/events", "/api/executions/example/result", "/api/executions/example/context",
	} {
		t.Run(path, func(t *testing.T) {
			f := newAuthFixture(t)
			status, body := f.request(t, "POST", path, true, f.csrf, nil)
			assertAuthError(t, status, body, 403, Forbidden)
			f.assertAudit(t, "POST", path, "wrong_auth_class", 403, Forbidden)
		})
	}
}

func TestRunnerIdentityAndBoundaryChecks(t *testing.T) {
	for _, test := range []string{"other Runner", "foreign host", "foreign origin", "mixed on Runner", "mixed on owner", "Runner on owner mutation"} {
		t.Run(test, func(t *testing.T) {
			f := newAuthFixture(t)
			token := registerAuthRunner(t, f)
			path, reason := "/api/runners/mac-mini/claim", "wrong_auth_class"
			if test == "other Runner" {
				path, reason = "/api/runners/other/claim", "runner_mismatch"
			} else if test == "foreign host" {
				reason = "host_mismatch"
			} else if test == "foreign origin" {
				reason = "origin_mismatch"
			} else if test == "mixed on owner" || test == "Runner on owner mutation" {
				path = "/api/tasks"
			}
			status, body := f.request(t, "POST", path, strings.HasPrefix(test, "mixed"), f.csrf, func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+token)
				if test == "foreign host" {
					r.Host = "evil.example"
				} else if test == "foreign origin" {
					r.Header.Set("Origin", "http://evil.example")
				}
			})
			assertAuthError(t, status, body, 403, Forbidden)
			f.assertAudit(t, "POST", path, reason, 403, Forbidden)
		})
	}
}

func TestRunnerCredentialsAreCheckedAgainstDatabaseOnEveryRequest(t *testing.T) {
	f := newAuthFixture(t)
	token := registerAuthRunner(t, f)
	request := func() (int, []byte) {
		return f.request(t, "POST", "/api/runners/mac-mini/claim", false, "", func(r *http.Request) {
			r.Header.Set("Authorization", "bEaReR "+token)
			r.Header.Del("Origin")
		})
	}
	status, body := request()
	if status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("empty claim status=%d body=%s", status, body)
	}
	if _, err := f.db.Exec(`UPDATE runners SET token_hash = 'revoked' WHERE id = 'mac-mini'`); err != nil {
		t.Fatal(err)
	}
	status, body = request()
	assertAuthError(t, status, body, 401, Unauthenticated)
	f.assertAudit(t, "POST", "/api/runners/mac-mini/claim", "invalid_runner_token", 401, Unauthenticated)
}

func TestRunnerAuthenticationFailsClosedOnDatabaseErrors(t *testing.T) {
	for _, failure := range []string{"audit", "token lookup", "ambiguous identity"} {
		t.Run(failure, func(t *testing.T) {
			f := newAuthFixture(t)
			token := registerAuthRunner(t, f)
			switch failure {
			case "audit":
				if _, err := f.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			case "token lookup":
				if _, err := f.db.Exec(`ALTER TABLE runners RENAME TO unavailable_runners`); err != nil {
					t.Fatal(err)
				}
			case "ambiguous identity":
				if _, err := f.db.Exec(`INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
					SELECT 'other', name, token_hash, worktree_root, status, last_heartbeat_at, created_at FROM runners`); err != nil {
					t.Fatal(err)
				}
			}
			// For audit failure, a valid owner on a Runner route triggers rejection.
			status, body := f.request(t, "POST", "/api/runners/mac-mini/claim", failure == "audit", "", func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+token)
			})
			assertAuthError(t, status, body, 500, Internal)
			if strings.Contains(string(body)+f.logs.String(), token) {
				t.Fatal("Runner token leaked on database failure")
			}
		})
	}
}
