package api

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func requireLoginFailure(t *testing.T, f *authFixture, id, password string, status int) {
	t.Helper()
	result := f.passwordLogin(t, id, password, nil)
	assertAuthError(t, result.status, result.raw, status, Unauthenticated)
	if len(result.cookies) != 0 {
		t.Fatalf("failed login issued cookies: %v", result.cookies)
	}
}

// Q91: five failures produce 401; subsequent attempts are 429 until exactly
// 60 seconds after the fifth failure, even with the correct credentials.
func TestOwnerLoginCooldownSkipsCredentialsAndExpiresAfterSixtySeconds(t *testing.T) {
	f := newAuthFixture(t)
	start := time.Now()
	var elapsed atomic.Int64
	f.auth.now = func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	for i := 0; i < 5; i++ {
		// A wrong ID with the correct password counts just like a wrong password.
		if i%2 == 0 {
			requireLoginFailure(t, f, "wrong-owner", testOwnerPassword, 401)
		} else {
			requireLoginFailure(t, f, testOwnerID, "wrong-password", 401)
		}
	}
	// Temporarily hide the credentials table. A credential lookup would now
	// fail with 500; 429 proves neither lookup nor password verification occurs.
	if _, err := f.db.Exec(`ALTER TABLE owner_credentials RENAME TO hidden_credentials`); err != nil {
		t.Fatal(err)
	}
	for _, after := range []time.Duration{0, 30 * time.Second, 60*time.Second - time.Nanosecond} {
		elapsed.Store(int64(after))
		requireLoginFailure(t, f, testOwnerID, testOwnerPassword, 429)
	}
	if got := countRows(t, f.db, `SELECT count(*) FROM owner_sessions`); got != 1 {
		t.Fatalf("blocked login created sessions: %d", got)
	}
	if _, err := f.db.Exec(`ALTER TABLE hidden_credentials RENAME TO owner_credentials`); err != nil {
		t.Fatal(err)
	}
	elapsed.Store(int64(60 * time.Second))
	result := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	if result.status != 200 || len(result.cookies) != 1 || result.csrf == "" {
		t.Fatalf("login at cooldown boundary: %d %s", result.status, result.raw)
	}
	if status, body := ownerCall(t, f.server.Config.Handler, f.auth.host, "GET", "/api/session", result.cookies[0], ""); status != 200 {
		t.Fatalf("session after cooldown: %d %s", status, body)
	}
	failures := auditRows(t, f.db, "auth.login_failure")
	if len(failures) != 8 {
		t.Fatalf("login failures were not audited: %+v", failures)
	}
	for _, row := range failures[5:] {
		if row.Detail["status"] != float64(429) || row.Detail["reason"] != "login_cooldown" || row.Detail["via"] != "loopback" {
			t.Fatalf("cooldown audit: %+v", row)
		}
	}
}

// A successful login resets the consecutive count, including after four failures.
func TestOwnerLoginSuccessResetsFailureCount(t *testing.T) {
	f := newAuthFixture(t)
	for round := 0; round < 2; round++ {
		for i := 0; i < 4; i++ {
			requireLoginFailure(t, f, testOwnerID, "wrong-password", 401)
		}
		result := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
		if result.status != 200 || len(result.cookies) != 1 {
			t.Fatalf("login after four failures in round %d: %d %s", round, result.status, result.raw)
		}
	}
}

// Expiry starts a new run of failures rather than retaining the previous five.
func TestOwnerLoginCooldownExpiryResetsFailureCount(t *testing.T) {
	f := newAuthFixture(t)
	start := time.Now()
	var elapsed atomic.Int64
	f.auth.now = func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	for i := 0; i < 5; i++ {
		requireLoginFailure(t, f, testOwnerID, "wrong-password", 401)
	}
	elapsed.Store(int64(time.Minute))
	for i := 0; i < 5; i++ {
		requireLoginFailure(t, f, testOwnerID, "wrong-password", 401)
	}
	requireLoginFailure(t, f, testOwnerID, testOwnerPassword, 429)
}

// Parallel requests must not each observe an old failure count and bypass Q91.
func TestOwnerConcurrentLoginFailuresStartOneCooldown(t *testing.T) {
	f := newAuthFixture(t)
	const attempts = 12
	results := make(chan loginResult, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- f.passwordLogin(t, testOwnerID, "wrong-password", func(r *http.Request) {
				// Client-controlled proxy headers cannot split the single owner's count.
				r.Header.Set("X-Forwarded-For", "192.0.2."+strconv.Itoa(i+1))
			})
		}(i)
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for result := range results {
		if result.status != 401 && result.status != 429 {
			t.Errorf("unexpected parallel response: %d %s", result.status, result.raw)
		}
		if len(result.cookies) != 0 {
			t.Error("parallel failed login issued a session")
		}
		counts[result.status]++
	}
	if counts[401] != 5 || counts[429] != attempts-5 {
		t.Fatalf("parallel statuses=%v; want five 401s and seven 429s", counts)
	}
	requireLoginFailure(t, f, testOwnerID, testOwnerPassword, 429)
}

// An otherwise correct login that cannot commit its audit is not a success
// and must not reset the count. Audit errors during cooldown also fail closed.
func TestOwnerLoginAuditFailurePreservesThrottle(t *testing.T) {
	f := newAuthFixture(t)
	for i := 0; i < 4; i++ {
		requireLoginFailure(t, f, testOwnerID, "wrong-password", 401)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	result := f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	assertAuthError(t, result.status, result.raw, 500, Internal)
	if len(result.cookies) != 0 || countRows(t, f.db, `SELECT count(*) FROM owner_sessions`) != 1 {
		t.Fatal("login with failed audit created a session")
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_audit`); err != nil {
		t.Fatal(err)
	}
	requireLoginFailure(t, f, testOwnerID, "wrong-password", 401)
	requireLoginFailure(t, f, testOwnerID, testOwnerPassword, 429)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	result = f.passwordLogin(t, testOwnerID, testOwnerPassword, nil)
	assertAuthError(t, result.status, result.raw, 500, Internal)
	if len(result.cookies) != 0 {
		t.Fatal("cooldown audit failure issued cookies")
	}
}
