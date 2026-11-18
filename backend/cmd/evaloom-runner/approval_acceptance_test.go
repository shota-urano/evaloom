package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// Acceptance tests of spec 17 (conditions 21 and 22) in the spec 14 suite.
// The probe special harness declares one approval-required operation
// (st.ProbeOperation) and runs against the real Control Plane and the
// production Runner command; its publish stage asks authorize on the run
// channel and sends the operation to the test destination only on
// allowed: true. Each case judges by the Control Plane records (approvals,
// execution_events, audit_logs), the destination's operation count and the
// reply the stage received. The authorize unit tests in internal/api cover
// every row of spec 17 3.5 against the store; these run the whole path.
//
// Only the container runtime CLI is a fake (no Docker daemon), which runs the
// stage on the host. The publish stage therefore declares no network and the
// destination listens on loopback; the Sandbox's own isolation is the subject
// of the security_docker tests, not of these.

// approvalProbe configures one case's probe special harness.
type approvalProbe struct {
	// request makes the check stage ask approval for st.ProbeOperation.
	request bool
	// waivable declares the operation waivable: true (the Project still
	// waives nothing, so the approval stays required).
	waivable bool
	// attempts repeats ask-then-operate with the same approval.
	attempts int
	// gate, when set, holds the publish stage before it asks authorize until
	// the file exists, so the test can act after the approval was given and
	// the publish stage was claimed.
	gate string
}

const approvalProbeInstructions = "Probe work instructions. The probe Adapter never calls an AI.\n"

func approvalProbeHarness(d *st.Destination, p approvalProbe) fixtureHarness {
	return fixtureHarness{instructions: approvalProbeInstructions, taskBody: "approval acceptance", populate: func(t *testing.T, repo, _ string) {
		h := st.ProbeHarness{
			Authorize: &st.ProbeAuthorize{Operation: st.ProbeOperation, Destination: d.URL(st.DestinationOperationPath), Attempts: p.attempts},
			Results:   map[string]map[string]any{},
			Plans:     map[string]json.RawMessage{},
		}
		if p.request {
			h.Results["check"] = map[string]any{"status": "needs_approval", "approvalRequests": []any{map[string]any{
				"operation": st.ProbeOperation, "contentId": "{{contentId}}",
				"target": map[string]any{"label": "probe destination"}, "summary": "send the probe operation"}}}
		}
		if p.gate != "" {
			plan, _ := json.Marshal([]any{map[string]any{"id": "gate", "kind": "command", "timeoutSec": 40,
				"argv": []string{"/bin/sh", "-c", "while [ ! -e " + shellQuote(p.gate) + " ]; do sleep 0.05; done"}}})
			h.Plans["publish"] = plan
		}
		if p.waivable {
			h.Profile = func(d *contract.ProfileDefinition) { d.Approvals[0].Waivable = true }
		}
		if err := st.WriteProbeHarness(repo, h); err != nil {
			t.Fatal(err)
		}
	}}
}

func approvalDestination(t *testing.T) *st.Destination {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: %v", err)
	}
	d := st.NewDestination(listener)
	t.Cleanup(func() { d.Close() })
	return d
}

type approvalRow struct {
	ID, Status, ContentID, Reason string
	UseCount                      int
}

func (f *executionFixture) approvalRows() []approvalRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT id,status,content_id,coalesce(invalidated_reason,''),use_count FROM approvals WHERE execution_id=? ORDER BY created_at,id`, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var got []approvalRow
	for rows.Next() {
		var r approvalRow
		if err := rows.Scan(&r.ID, &r.Status, &r.ContentID, &r.Reason, &r.UseCount); err != nil {
			f.t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *executionFixture) oneApproval() approvalRow {
	f.t.Helper()
	rows := f.approvalRows()
	if len(rows) != 1 {
		f.t.Fatalf("approvals %+v; want one", rows)
	}
	return rows[0]
}

// authorizations is every reply the publish stage received, in order.
func (f *executionFixture) authorizations() []contract.AuthorizeResponse {
	f.t.Helper()
	var replies []contract.AuthorizeResponse
	if err := json.Unmarshal(f.stageFile("publish-1", "authorizations.json"), &replies); err != nil {
		f.t.Fatal(err)
	}
	return replies
}

// consumeRecords is what the Control Plane recorded for each authorize:
// the approval_consumed events and the approval.consume audit logs.
func (f *executionFixture) consumeRecords() (events, audits []map[string]any) {
	f.t.Helper()
	read := func(query string) []map[string]any {
		rows, err := f.db.Query(query, f.id)
		if err != nil {
			f.t.Fatal(err)
		}
		defer rows.Close()
		var got []map[string]any
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				f.t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				f.t.Fatal(err)
			}
			got = append(got, payload)
		}
		return got
	}
	return read(`SELECT payload FROM execution_events WHERE execution_id=? AND type='approval_consumed' ORDER BY id`),
		read(`SELECT detail FROM audit_logs WHERE target_type='execution' AND target_id=? AND action='approval.consume' ORDER BY id`)
}

// wantDenied checks that the publish stage was refused with reason and that
// nothing reached the destination: the Execution fails with publish_failed.
func (f *executionFixture) wantDenied(d *st.Destination, reason string) {
	f.t.Helper()
	f.waitStatus("failed")
	f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
	var errorClass string
	f.scalar(`SELECT error_class FROM executions WHERE id=?`, &errorClass)
	if errorClass != "publish_failed" {
		f.t.Fatalf("error_class %s; want publish_failed", errorClass)
	}
	if replies := f.authorizations(); len(replies) != 1 || replies[0].Allowed || replies[0].Reason != reason {
		f.t.Fatalf("authorize replies %+v; want one %s", replies, reason)
	}
	events, audits := f.consumeRecords()
	if len(events) != 1 || events[0]["allowed"] != false || events[0]["reason"] != reason || len(audits) != 1 || audits[0]["reason"] != reason {
		f.t.Fatalf("records events=%v audits=%v; want one %s", events, audits, reason)
	}
	if n := d.Operations(); n != 0 {
		f.t.Fatalf("destination received %d operations without allowed", n)
	}
	if _, err := os.Stat(filepath.Join(f.config.DataDir, "executions", f.id, "publish-1", "out", "operation.json")); !os.IsNotExist(err) {
		f.t.Fatalf("publish stage sent the operation: %v", err)
	}
}

// approve answers the open approval decision through the management API,
// with the content identifier the owner was shown.
func (f *executionFixture) approve() approvalRow {
	f.t.Helper()
	decision := f.decision("approval")
	a := f.oneApproval()
	if a.Status != "pending" {
		f.t.Fatalf("approval %+v; want pending", a)
	}
	f.owner("POST", "/api/decisions/"+decision+"/answer", `{"action":"approve","contentId":"`+a.ContentID+`"}`, 200)
	return a
}

// waitPublishing waits until the publish stage was claimed after approval.
func (f *executionFixture) waitPublishing() {
	f.t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		f.scalar(`SELECT count(*) FROM stage_runs WHERE execution_id=? AND stage='publish'`, &n)
		if n == 1 {
			var status string
			f.scalar(`SELECT status FROM executions WHERE id=?`, &status)
			if status != "publishing" {
				f.t.Fatalf("Execution %s with a publish stage run; want publishing", status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("publish stage was not started; output %s", f.output.String())
}

func release(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalAcceptance(t *testing.T) {
	// Condition 21: an operation the harness declared as approval-required
	// is not allowed without the owner's approval, even when declared
	// waivable, as long as the Project does not waive it.
	for _, tc := range []struct {
		name     string
		waivable bool
	}{{"without_approval", false}, {"without_approval_waivable_not_waived", true}} {
		t.Run(tc.name, func(t *testing.T) {
			d := approvalDestination(t)
			f := newHarnessFixture(t, tc.name, approvalProbeHarness(d, approvalProbe{waivable: tc.waivable}))
			f.start()
			f.wantDenied(d, "not_approved")
			if rows := f.approvalRows(); len(rows) != 0 {
				t.Fatalf("approvals %+v; want none", rows)
			}
		})
	}

	// Positive control for the cases below: the approved operation with the
	// approved content is allowed once and reaches the destination once.
	t.Run("approved", func(t *testing.T) {
		d := approvalDestination(t)
		f := newHarnessFixture(t, "approved", approvalProbeHarness(d, approvalProbe{request: true}))
		f.start()
		f.waitStatus("waiting_approval")
		approved := f.approve()
		f.waitStatus("succeeded")
		if replies := f.authorizations(); len(replies) != 1 || !replies[0].Allowed {
			t.Fatalf("authorize replies %+v", replies)
		}
		if a := f.oneApproval(); a.Status != "consumed" || a.UseCount != 1 || a.ID != approved.ID {
			t.Fatalf("approval %+v; want consumed once", a)
		}
		if n := d.Operations(); n != 1 {
			t.Fatalf("destination received %d operations; want 1", n)
		}
	})

	// Condition 22: the content changes after the approval and before the
	// publish stage asks. The probe takes contentId from the workspace after
	// its plan (the gate) finished, right before authorize, so the change
	// made while the gate holds the stage is always what authorize sees
	// (securitytest.TestProbePublishTakesContentIDRightBeforeAuthorize).
	t.Run("content_changed_after_approval", func(t *testing.T) {
		d := approvalDestination(t)
		gate := filepath.Join(t.TempDir(), "gate")
		f := newHarnessFixture(t, "content_changed", approvalProbeHarness(d, approvalProbe{request: true, gate: gate}))
		f.start()
		f.waitStatus("waiting_approval")
		approved := f.approve()
		f.waitPublishing()
		if err := os.WriteFile(filepath.Join(f.claims[0].Workspace.Path, "probe-prepared.txt"), []byte("changed after approval\n"), 0600); err != nil {
			t.Fatal(err)
		}
		release(t, gate)
		f.wantDenied(d, "content_changed")
		// The gate (the plan) completed before the stage asked authorize.
		var gateReport struct {
			Observations []struct {
				ID        string `json:"id"`
				Succeeded bool   `json:"succeeded"`
			} `json:"observations"`
		}
		if err := json.Unmarshal(f.stageFile("publish-1", "probe-publish.json"), &gateReport); err != nil {
			t.Fatal(err)
		}
		if len(gateReport.Observations) != 1 || gateReport.Observations[0].ID != "gate" || !gateReport.Observations[0].Succeeded {
			t.Fatalf("gate observations %+v", gateReport.Observations)
		}
		if a := f.oneApproval(); a.Status != "invalidated" || a.Reason != "content_changed" || a.UseCount != 0 || a.ContentID != approved.ContentID {
			t.Fatalf("approval %+v; want invalidated (content_changed)", a)
		}
	})

	// A used approval does not allow a second operation.
	t.Run("used_approval_second_time", func(t *testing.T) {
		d := approvalDestination(t)
		f := newHarnessFixture(t, "used_twice", approvalProbeHarness(d, approvalProbe{request: true, attempts: 2}))
		f.start()
		f.waitStatus("waiting_approval")
		f.approve()
		f.waitStatus("failed")
		replies := f.authorizations()
		if len(replies) != 2 || !replies[0].Allowed || replies[1].Allowed || replies[1].Reason != "already_used" {
			t.Fatalf("authorize replies %+v; want allowed then already_used", replies)
		}
		if n := d.Operations(); n != 1 {
			t.Fatalf("destination received %d operations; want exactly 1", n)
		}
		if a := f.oneApproval(); a.Status != "consumed" || a.UseCount != 1 {
			t.Fatalf("approval %+v; want consumed once", a)
		}
		events, audits := f.consumeRecords()
		if len(events) != 2 || events[1]["reason"] != "already_used" || len(audits) != 2 || audits[1]["reason"] != "already_used" {
			t.Fatalf("records events=%v audits=%v", events, audits)
		}
	})

	// The approval expires after it was given and the publish stage was
	// claimed (Q166 covers expiry before the claim). Waiting out the
	// shortest ttlSec (60s) is replaced by moving expires_at into the past,
	// which stands for the clock passing it.
	t.Run("expired_approval", func(t *testing.T) {
		d := approvalDestination(t)
		gate := filepath.Join(t.TempDir(), "gate")
		f := newHarnessFixture(t, "expired", approvalProbeHarness(d, approvalProbe{request: true, gate: gate}))
		f.start()
		f.waitStatus("waiting_approval")
		approved := f.approve()
		f.waitPublishing()
		past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := f.db.Exec(`UPDATE approvals SET expires_at=? WHERE id=? AND status='approved'`, past, approved.ID); err != nil {
			t.Fatal(err)
		}
		release(t, gate)
		f.wantDenied(d, "expired")
		if a := f.oneApproval(); a.Status != "expired" || a.UseCount != 0 {
			t.Fatalf("approval %+v; want expired", a)
		}
	})

	// Approval is accepted only through the management UI: a Linear comment
	// and the Runner API cannot approve (owner decision 3, spec 17 4章).
	t.Run("only_management_ui_approves", func(t *testing.T) {
		d := approvalDestination(t)
		f := newHarnessFixture(t, "owner_ui_only", approvalProbeHarness(d, approvalProbe{request: true}))
		// The task comes from Linear, so the approval wait is announced on
		// its Issue and the owner can reply there.
		var taskID string
		f.scalar(`SELECT task_id FROM executions WHERE id=?`, &taskID)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, q := range []string{
			`UPDATE tasks SET source='linear' WHERE id=?`,
			`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
				VALUES ('link-approval', 'issue-approval', 'ENG-21', 'https://linear.app/x/issue/ENG-21', 'team-a', ?, 1, '` + now + `')`,
		} {
			if _, err := f.db.Exec(q, taskID); err != nil {
				t.Fatal(err)
			}
		}
		f.start()
		f.waitStatus("waiting_approval")
		decision := f.decision("approval")
		pending := f.oneApproval()

		// Runner API: the Runner's own token cannot answer the decision, and
		// authorize does not turn a pending approval into an allowed one.
		token, err := os.ReadFile(f.config.TokenFile)
		if err != nil {
			t.Fatal(err)
		}
		runner := func(path, body string) int {
			req, err := http.NewRequest("POST", f.server.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
			req.Header.Set("Content-Type", "application/json")
			response, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			return response.StatusCode
		}
		answer := `{"action":"approve","contentId":"` + pending.ContentID + `"}`
		if code := runner("/api/decisions/"+decision+"/answer", answer); code != http.StatusForbidden {
			t.Fatalf("Runner answer: %d; want 403", code)
		}
		var rejected int
		f.scalar(`SELECT count(*) FROM audit_logs WHERE action='auth.reject' AND target_id='/api/decisions/'||(SELECT id FROM owner_decisions WHERE execution_id=? AND kind='approval')||'/answer'`, &rejected)
		if rejected != 1 {
			t.Fatalf("Runner answer rejections in audit_logs: %d; want 1", rejected)
		}
		ask, _ := json.Marshal(map[string]any{"operation": st.ProbeOperation, "approvalId": pending.ID, "contentId": pending.ContentID})
		if code := runner("/api/executions/"+f.id+"/authorize", string(ask)); code != http.StatusConflict {
			t.Fatalf("Runner authorize while waiting: %d; want 409", code)
		}

		// Linear: the owner replies "approve" on the Issue. The only Evaloom
		// component that reads Issue comments is the comment Worker, before
		// resending a comment whose result is unknown; make it read them.
		var outbox int
		f.scalar(`SELECT count(*) FROM linear_outbox WHERE execution_id=? AND kind='decision_open' AND status='pending'`, &outbox)
		if outbox != 1 {
			t.Fatalf("decision_open rows %d; want 1", outbox)
		}
		if _, err := f.db.Exec(`UPDATE linear_outbox SET outcome_unknown=1 WHERE kind='decision_open'`); err != nil {
			t.Fatal(err)
		}
		// State changes need the Team's states; this case is about comments.
		if _, err := f.db.Exec(`UPDATE linear_outbox SET status='skipped' WHERE action='set_state'`); err != nil {
			t.Fatal(err)
		}
		fake := &ownerReplyLinear{replies: []string{"承認します", "approve", "/approve " + pending.ContentID, answer}}
		worker := approvalLinearWorker(t, f, fake)
		if err := worker.SendDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		if reads, created := fake.counts(); reads == 0 || created != 1 {
			t.Fatalf("Linear comment reads=%d creates=%d; want the replies read and one comment sent", reads, created)
		}

		if a := f.oneApproval(); a != pending {
			t.Fatalf("approval %+v changed; want %+v", a, pending)
		}
		var decisionStatus, executionStatus string
		var answered int
		f.scalar(`SELECT status FROM owner_decisions WHERE execution_id=? AND kind='approval'`, &decisionStatus)
		f.scalar(`SELECT status FROM executions WHERE id=?`, &executionStatus)
		if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action='decision.answer'`).Scan(&answered); err != nil {
			t.Fatal(err)
		}
		if decisionStatus != "open" || executionStatus != "waiting_approval" || answered != 0 {
			t.Fatalf("decision %s, Execution %s, decision.answer logs %d; want open, waiting_approval, 0", decisionStatus, executionStatus, answered)
		}
		f.claimCount(1)
		if n := d.Operations(); n != 0 {
			t.Fatalf("destination received %d operations before approval", n)
		}

		// The management UI approves, and only then the operation runs once.
		f.owner("POST", "/api/decisions/"+decision+"/answer", answer, 200)
		f.waitStatus("succeeded")
		if a := f.oneApproval(); a.Status != "consumed" || a.UseCount != 1 {
			t.Fatalf("approval %+v; want consumed after the owner's approval", a)
		}
		if n := d.Operations(); n != 1 {
			t.Fatalf("destination received %d operations; want 1", n)
		}
	})
}

// ownerReplyLinear is a stand-in for the Linear GraphQL API whose Issue
// carries the owner's replies. It accepts the comment Evaloom sends.
type ownerReplyLinear struct {
	replies []string
	mu      sync.Mutex
	reads   int
	created int
}

func (l *ownerReplyLinear) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads, l.created
}

func (l *ownerReplyLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case strings.Contains(req.Query, "commentCreate"):
		l.created++
		_, _ = io.WriteString(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-evaloom"}}}}`)
	case strings.Contains(req.Query, "EvaloomIssueComments"):
		l.reads++
		var nodes []map[string]string
		for i, body := range l.replies {
			nodes = append(nodes, map[string]string{"id": "owner-reply-" + string(rune('a'+i)), "body": body})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": req.Variables["id"], "comments": map[string]any{"nodes": nodes,
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}}})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

// approvalLinearWorker is the production comment Worker on the fixture's
// Control Plane DB, talking to fake through the real Linear client.
func approvalLinearWorker(t *testing.T, f *executionFixture, fake http.Handler) *linearfeedback.Worker {
	t.Helper()
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	keyPath := filepath.Join(t.TempDir(), linear.APIKeyFileName)
	if err := os.WriteFile(keyPath, []byte("lin_api_EVALOOM_APPROVAL_FAKE_KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	return &linearfeedback.Worker{DB: f.db, Client: client, Logger: logger}
}
