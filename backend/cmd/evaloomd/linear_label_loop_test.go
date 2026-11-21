package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Use the owner API so the decision answer, route, first Execution and label
// outbox row are committed together, as in production (15 3.6.4, 16 3.5.5).
// Authentication is a fixture session; no password or external service is used.
func approveLoopRoute(t *testing.T, h *loopHarness, taskID string) string {
	t.Helper()
	var decisionID, proposalID string
	if err := h.db.QueryRow(`SELECT id, subject_id FROM owner_decisions
		WHERE task_id = ? AND kind = 'route' AND status = 'open'`, taskID).Scan(&decisionID, &proposalID); err != nil {
		t.Fatal(err)
	}
	token, err := ownerauth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	csrf := ownerauth.CSRFToken(token)
	now := time.Now().UTC()
	if err := store.CreateOwnerSession(t.Context(), h.db, store.OwnerSession{
		ID: "loop-owner", TokenHash: ownerauth.TokenHash(token), CSRFTokenHash: ownerauth.TokenHash(csrf),
		Via: "loopback", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, store.AuditLog{Actor: "owner", Action: "auth.login", TargetType: "owner_session", TargetID: "loop-owner"}); err != nil {
		t.Fatal(err)
	}
	const host = "127.0.0.1:8080"
	auth, err := api.NewOwnerAuth(h.db, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"action": "approve", "label": "route:p", "basis": proposalID})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/decisions/"+decisionID+"/answer", strings.NewReader(string(body)))
	req.Header.Set("Origin", "http://"+host)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: "evaloom_session", Value: token})
	response := httptest.NewRecorder()
	api.NewHandler(h.db, nil, auth).ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("approve route: %d %s", response.Code, response.Body.String())
	}
	return decisionID
}

// 受け入れ基準: 振り分けを承認して Evaloom がラベルを付け、更新時刻が
// 進んだ後に照会しても、タスク・Execution・提案・判断待ちは増えない。
// 既存の未知ラベルも残し、追加後も振り分け先が決まらない場合を含める。
func TestLinearApprovedRouteLabelDoesNotRestartOrRepropose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"missing routing label", []string{"ai-ready", "bug"}},
		{"unknown routing label", []string{"ai-ready", "bug", "route:unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t)
			id := attachLoopIssue(t, h, loopCriteria, tc.labels...)
			h.send(t) // Deliver decision_open before isolating the label update.
			h.poll(t) // Consume the comment's updatedAt while still routing.
			decisionID := approveLoopRoute(t, h, id)
			var queued, executions, proposals, decisions, pendingLabels int
			if err := h.db.QueryRow(`SELECT
				(SELECT count(*) FROM tasks WHERE id = ? AND status = 'queued' AND project_id = 'p1'),
				(SELECT count(*) FROM executions WHERE task_id = ? AND seq = 1 AND status = 'pending'),
				(SELECT count(*) FROM route_proposals WHERE task_id = ? AND status = 'decided'),
				(SELECT count(*) FROM owner_decisions WHERE id = ? AND status = 'answered'),
				(SELECT count(*) FROM linear_outbox WHERE task_id = ? AND action = 'add_label'
				 AND kind = 'route_approved' AND label_name = 'route:p' AND status = 'pending')`,
				id, id, id, decisionID, id).Scan(&queued, &executions, &proposals, &decisions, &pendingLabels); err != nil {
				t.Fatal(err)
			}
			if queued != 1 || executions != 1 || proposals != 1 || decisions != 1 || pendingLabels != 1 {
				t.Fatalf("after approval: queued=%d executions=%d proposals=%d decisions=%d labels=%d",
					queued, executions, proposals, decisions, pendingLabels)
			}
			before := h.state(t, "issue-1")
			h.send(t) // Production Worker and client add the approved label.
			advanced := h.fake.updatedAt("issue-1")
			if sameInstant(t, advanced, before.updatedAt) {
				t.Fatal("label delivery did not advance updatedAt")
			}
			issue, err := h.client.Issue(t.Context(), "issue-1")
			if err != nil {
				t.Fatal(err)
			}
			wantLabels := append(append([]string(nil), tc.labels...), "route:p")
			if !reflect.DeepEqual(issue.Labels, wantLabels) {
				t.Fatalf("labels = %v, want %v", issue.Labels, wantLabels)
			}
			var sent int
			if err := h.db.QueryRow(`SELECT count(*) FROM linear_outbox WHERE dedupe_key = ?
				AND status = 'sent'`, id+":label:"+decisionID).Scan(&sent); err != nil {
				t.Fatal(err)
			}
			if sent != 1 {
				t.Fatal("approved label was not sent")
			}
			effects := h.effects(t)
			if len(effects["tasks"]) != 1 || len(effects["executions"]) != 1 ||
				len(effects["route_proposals"]) != 1 || len(effects["owner_decisions"]) != 1 {
				t.Fatalf("unexpected rows before repolling: %v", h.counts(t))
			}
			intake := h.poller.Handler.(*linearintake.Intake)
			probe := &loopRecheckProbe{HeldRechecker: intake.Rechecker}
			intake.Rechecker = probe
			for i := 0; i < 2; i++ {
				h.poll(t)
				h.send(t)
				h.wantEffects(t, effects) // Compare every column, not only counts.
				got := h.state(t, "issue-1")
				if !got.aiReady || got.seq != before.seq || got.decision != before.decision ||
					!sameInstant(t, got.updatedAt, advanced) {
					t.Fatalf("state after label update = %+v, before = %+v", got, before)
				}
			}
			if probe.calls != 1 {
				t.Fatalf("rechecks = %d, want 1 for the label's updatedAt", probe.calls)
			}
			h.fake.mu.Lock()
			adds := append([]map[string]any(nil), h.fake.labelAdds...)
			h.fake.mu.Unlock()
			if !reflect.DeepEqual(adds, []map[string]any{{"id": "issue-1", "labelId": "route-p"}}) {
				t.Fatalf("label additions = %v, want the approved label exactly once", adds)
			}
		})
	}
}
