package linearintake

import (
	"sync/atomic"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

func classErr(class linear.ErrorClass) error { return &linear.Error{Op: "list issues", Class: class} }

// 受け入れ基準: Poller の認証エラーの通知は Team ごとの抑止ではなく共通の抑止で
// 決まる。複数の Team が認証エラーでも1回、続く間はもう出ず、どれか1つの Team の
// 照会が1回成功した後にまた認証エラーになると、もう一度出る（spec 16 3.4.3、Q84）。
func TestPollerAuthAlertUsesSharedGate(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:one")
	var okTeam atomic.Value // team that succeeds; "" = all fail auth
	okTeam.Store("")
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) {
		if okTeam.Load().(string) == teamID {
			return nil, nil
		}
		return nil, classErr(linear.ClassAuth)
	}}
	alerter := &recordingAlerter{}
	gate := &linear.AuthAlertGate{}
	p := newPoller(db, client, nil)
	p.Alerter, p.AuthGate = alerter, gate
	for round := 1; round <= FailureAlertThreshold+2; round++ {
		pollOnce(t, p)
		if got := alerter.count(); got != 1 {
			t.Fatalf("round %d: alerts = %d; want 1", round, got)
		}
	}
	if a := alerter.alerts[0]; a.Class != linear.ClassAuth || a.ConsecutiveFailures != 1 {
		t.Fatalf("alert = %+v", a)
	}
	// The suppression is the shared gate, not a per-Team flag.
	if gate.Acquire() {
		t.Fatal("shared gate not held after the auth alert")
	}
	gate.Release()
	if !gate.Acquire() {
		t.Fatal("gate stuck")
	}
	// Held again by "someone else": the Poller must stay quiet.
	pollOnce(t, p)
	if got := alerter.count(); got != 1 {
		t.Fatalf("alerts while the gate is held elsewhere = %d; want 1", got)
	}
	// One Team's success lifts the suppression; the next auth alerts again.
	// team-a fails auth while the gate is still held (no alert), then
	// team-b succeeds and releases it.
	okTeam.Store("team-b")
	pollOnce(t, p)
	if got := alerter.count(); got != 1 {
		t.Fatalf("alerts in the round with a success = %d; want 1", got)
	}
	okTeam.Store("")
	pollOnce(t, p)
	pollOnce(t, p)
	if got := alerter.count(); got != 2 {
		t.Fatalf("alerts after one Team succeeded = %d; want 2", got)
	}
}

// 受け入れ基準: 照会の5回目の失敗の分類が auth のときは Team ごとの「5回続けて
// 失敗した」通知が出ず、通知は共通の抑止に従う認証エラーの通知だけになる（Q86）。
func TestFifthFailureAuthDoesNotSendStreakAlert(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	var class atomic.Value
	class.Store(linear.ClassNetwork)
	client := &stubClient{list: func(string) ([]linear.Issue, error) { return nil, classErr(class.Load().(linear.ErrorClass)) }}
	alerter := &recordingAlerter{}
	gate := &linear.AuthAlertGate{}
	gate.Acquire() // the comment Worker already alerted the auth error
	p := newPoller(db, client, nil)
	p.Alerter, p.AuthGate = alerter, gate
	for range FailureAlertThreshold - 1 {
		pollOnce(t, p)
	}
	class.Store(linear.ClassAuth)
	for range 3 {
		pollOnce(t, p) // the 5th failure is auth, then more auth
	}
	if got := alerter.count(); got != 0 {
		t.Fatalf("alerts = %+v; want none (auth is only the shared gate's, which is held)", alerter.alerts)
	}
	if row := readTeam(t, db, "team-a"); row.failures != FailureAlertThreshold+2 || row.lastError.String != "auth" {
		t.Fatalf("row = %+v", row)
	}
	// With the gate free, the only alert is the shared auth alert.
	gate.Release()
	pollOnce(t, p)
	pollOnce(t, p)
	if alerter.count() != 1 || alerter.alerts[0].Class != linear.ClassAuth {
		t.Fatalf("alerts = %+v; want one auth alert", alerter.alerts)
	}
}

// 受け入れ基準: 5回目の失敗の分類が auth 以外（rate_limited・network・other）の
// ときは、Team ごとの抑止で「5回続けて失敗した」通知が1回出る（Q86）。
func TestFifthFailureNonAuthSendsStreakAlertOnce(t *testing.T) {
	for _, class := range []linear.ErrorClass{linear.ClassRateLimited, linear.ClassNetwork, linear.ClassOther} {
		t.Run(string(class), func(t *testing.T) {
			db := openDB(t)
			addProject(t, db, "p1", "team-a", "repo:one")
			client := &stubClient{list: func(string) ([]linear.Issue, error) { return nil, classErr(class) }}
			alerter := &recordingAlerter{}
			gate := &linear.AuthAlertGate{}
			gate.Acquire() // a held auth gate must not hide a non-auth streak
			p := newPoller(db, client, nil)
			p.Alerter, p.AuthGate = alerter, gate
			for round := 1; round <= FailureAlertThreshold+3; round++ {
				pollOnce(t, p)
				want := 0
				if round >= FailureAlertThreshold {
					want = 1
				}
				if got := alerter.count(); got != want {
					t.Fatalf("round %d: alerts = %d; want %d", round, got, want)
				}
			}
			if a := alerter.alerts[0]; a.Class != class || a.ConsecutiveFailures != FailureAlertThreshold {
				t.Fatalf("alert = %+v", a)
			}
		})
	}
}

// A round whose Teams fail both auth and non-auth (threshold) sends one
// notification, the auth one, whose suppression is already held.
func TestRoundWithAuthAndStreakSendsAuthAlertOnly(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:one")
	var authB atomic.Bool
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) {
		if teamID == "team-b" && authB.Load() {
			return nil, classErr(linear.ClassAuth)
		}
		return nil, classErr(linear.ClassNetwork)
	}}
	alerter := &recordingAlerter{}
	p := newPoller(db, client, nil)
	p.Alerter = alerter
	for range FailureAlertThreshold - 1 {
		pollOnce(t, p)
	}
	authB.Store(true)
	pollOnce(t, p) // team-a reaches 5 (network), team-b turns auth
	if alerter.count() != 1 || alerter.alerts[0].Class != linear.ClassAuth {
		t.Fatalf("alerts = %+v; want one auth alert", alerter.alerts)
	}
}
