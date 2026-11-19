package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// The remaining spec 14 3.4 cases reuse the condition 21/22 harness, real
// Control Plane and production Runner. Only the container runtime is fake
// (Q228); these tests do not prove Sandbox network isolation.
func TestAuthorizeAcceptance(t *testing.T) {
	t.Run("concurrent_same_approval", func(t *testing.T) {
		d := approvalDestination(t)
		f := newHarnessFixture(t, "concurrent", approvalProbeHarness(d, approvalProbe{request: true, attempts: 2, concurrent: true}))
		// Require both requests to reach the HTTP boundary before either can
		// enter the real handler. A sequential probe/relay cannot pass this.
		handler := f.server.Config.Handler
		var arrivals atomic.Int32
		both := make(chan struct{})
		f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/authorize") {
				if arrivals.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
				case <-r.Context().Done():
					return
				}
			}
			handler.ServeHTTP(w, r)
		})
		f.start()
		f.waitStatus("waiting_approval")
		f.approve()
		f.waitStatus("failed") // one denied attempt makes the probe fail
		replies := f.authorizations()
		allowed, denied := 0, 0
		for _, reply := range replies {
			if reply.Allowed && reply.Reason == "" {
				allowed++
			} else if !reply.Allowed && reply.Reason == "already_used" {
				denied++
			} else {
				t.Fatalf("unexpected authorize reply %+v", reply)
			}
		}
		if arrivals.Load() != 2 || allowed != 1 || denied != 1 {
			t.Fatalf("arrivals=%d allowed=%d denied=%d; want 2,1,1", arrivals.Load(), allowed, denied)
		}
		if a := f.oneApproval(); a.Status != "consumed" || a.UseCount != 1 {
			t.Fatalf("approval %+v; want consumed once", a)
		}
		f.wantConsumeRecords(d)
	})

	t.Run("rejected_approval", func(t *testing.T) {
		d := approvalDestination(t)
		gate := filepath.Join(t.TempDir(), "gate")
		f := newHarnessFixture(t, "rejected", approvalProbeHarness(d, approvalProbe{request: true, gate: gate}))
		f.start()
		f.waitStatus("waiting_approval")
		approved := f.approve()
		f.waitPublishing()
		// A real rejection also fails the Execution and prevents publish from
		// starting. Inject only the rejected row after claim to exercise a
		// stale publish request carrying that approval ID through the channel.
		if _, err := f.db.Exec(`UPDATE approvals SET status='rejected' WHERE id=?`, approved.ID); err != nil {
			t.Fatal(err)
		}
		release(t, gate)
		f.wantDenied(d, "not_approved")
		if a := f.oneApproval(); a.Status != "rejected" || a.UseCount != 0 {
			t.Fatalf("approval %+v; want rejected and unused", a)
		}
	})

	t.Run("cancelled_before_authorize", func(t *testing.T) {
		d := approvalDestination(t)
		gate := filepath.Join(t.TempDir(), "gate")
		f := newHarnessFixture(t, "cancelled", approvalProbeHarness(d, approvalProbe{request: true, gate: gate}))
		// Hold heartbeat responses so the stage can ask after cancellation,
		// before heartbeat stops its process. The cancellation API and the
		// authorize handler themselves remain the production handlers.
		handler := f.server.Config.Handler
		var heartbeat sync.Mutex
		f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/heartbeat") {
				heartbeat.Lock()
				defer heartbeat.Unlock()
			}
			handler.ServeHTTP(w, r)
		})
		f.start()
		f.waitStatus("waiting_approval")
		f.approve()
		f.waitPublishing()
		heartbeat.Lock()
		defer heartbeat.Unlock()
		var taskID string
		f.scalar(`SELECT task_id FROM executions WHERE id=?`, &taskID)
		f.owner("POST", "/api/tasks/"+taskID+"/cancel", `{}`, http.StatusNoContent)
		var notificationsBefore int
		f.scalar(`SELECT count(*) FROM notification_requests WHERE execution_id=?`, &notificationsBefore)
		release(t, gate)
		// The probe follows contract 7.4: a denied authorize ends the stage
		// with failed/publish_failed. Spec 04 3.4.1/Q230 requires cancellation
		// of the Execution and Task while preserving the stage's failure.
		f.waitProbePublishResult()
		f.waitTerminal("cancelled")
		var taskStatus, executionClass, stageResult string
		f.scalar(`SELECT status FROM tasks WHERE id=(SELECT task_id FROM executions WHERE id=?)`, &taskStatus)
		f.scalar(`SELECT error_class FROM executions WHERE id=?`, &executionClass)
		f.scalar(`SELECT status||'/'||error_class FROM stage_runs WHERE execution_id=? AND stage='publish' AND attempt=1`, &stageResult)
		if taskStatus != "cancelled" || executionClass != "cancelled" || stageResult != "failed/publish_failed" {
			t.Fatalf("task=%s execution class=%s publish=%s; want cancelled, cancelled, failed/publish_failed", taskStatus, executionClass, stageResult)
		}
		var notificationsAfter int
		f.scalar(`SELECT count(*) FROM notification_requests WHERE execution_id=?`, &notificationsAfter)
		if notificationsAfter != notificationsBefore {
			t.Fatalf("notification requests=%d; want unchanged from %d", notificationsAfter, notificationsBefore)
		}
		f.wantProbeReplies(contract.AuthorizeResponse{Reason: "cancelled"})
		if a := f.oneApproval(); a.Status != "invalidated" || a.Reason != "cancelled" || a.UseCount != 0 {
			t.Fatalf("approval %+v; want invalidated (cancelled)", a)
		}
		f.wantConsumeRecords(d)
	})

	t.Run("control_plane_stopped", func(t *testing.T) {
		d := approvalDestination(t)
		gate := filepath.Join(t.TempDir(), "gate")
		f := newHarnessFixture(t, "unavailable", approvalProbeHarness(d, approvalProbe{request: true, gate: gate}))
		f.start()
		f.waitStatus("waiting_approval")
		f.approve()
		f.waitPublishing()
		f.server.Close()
		release(t, gate)
		// The stopped Control Plane cannot accept the result report. Observe
		// the finished stage locally.
		f.waitProbePublishResult()
		f.wantProbeReplies(contract.AuthorizeResponse{Reason: "unavailable"})
		f.wantNoConsumption(d)
		f.mu.Lock()
		for _, path := range f.accesses {
			if strings.HasSuffix(path, "/authorize") {
				f.mu.Unlock()
				t.Fatalf("authorize unexpectedly reached the stopped Control Plane: %s", path)
			}
		}
		f.mu.Unlock()
		// Runner keeps its report pending while the Control Plane is down.
		// Bring the same handler back on the same address so the report is
		// delivered and Runner can stop; nothing may be consumed afterwards.
		listener, err := net.Listen("tcp", f.server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		restarted := &http.Server{Handler: f.server.Config.Handler}
		go func() { _ = restarted.Serve(listener) }()
		t.Cleanup(func() { _ = restarted.Close() })
		f.waitStatus("failed")
		f.wantNoConsumption(d)
	})

	for _, remove := range []bool{false, true} {
		name := "project_waived"
		if remove {
			name = "project_waiver_removed"
		}
		t.Run(name, func(t *testing.T) {
			d := approvalDestination(t)
			gate := filepath.Join(t.TempDir(), "gate")
			f := newHarnessFixture(t, name, approvalProbeHarness(d, approvalProbe{request: true, waivable: true, gate: gate}))
			f.owner("PATCH", "/api/projects/fixture", `{"approvalWaivers":["`+st.ProbeOperation+`"]}`, http.StatusOK)
			f.start()
			f.waitPublishing()
			// Check must really have waived the operation and passed a null ID
			// to publish; no approval may have been created along the way.
			if rows := f.approvalRows(); len(rows) != 0 {
				t.Fatalf("approvals %+v; want none", rows)
			}
			var waived int
			f.scalar(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type='approval_waived'
				AND json_extract(payload,'$.operation')='`+st.ProbeOperation+`'`, &waived)
			if waived != 1 {
				t.Fatalf("approval_waived events=%d; want 1", waived)
			}
			if remove {
				f.owner("PATCH", "/api/projects/fixture", `{"approvalWaivers":[]}`, http.StatusOK)
			}
			release(t, gate)
			if remove {
				f.wantDenied(d, "not_approved")
			} else {
				f.waitStatus("succeeded")
				f.wantProbeReplies(contract.AuthorizeResponse{Allowed: true})
			}
			if rows := f.approvalRows(); len(rows) != 0 {
				t.Fatalf("authorize created approvals %+v", rows)
			}
			f.wantConsumeRecords(d)
		})
	}

	// Fail both the consume itself and later writes, proving consumption is
	// rolled back when recording its result fails. Triggers exist only in
	// each disposable fixture DB, and target authorize's writes exclusively.
	for _, tc := range []struct{ name, trigger string }{
		{"consume", `BEFORE UPDATE OF status ON approvals WHEN NEW.status='consumed'`},
		{"event", `BEFORE INSERT ON execution_events WHEN NEW.type='approval_consumed'`},
		{"audit", `BEFORE INSERT ON audit_logs WHEN NEW.action='approval.consume'`},
	} {
		t.Run("write_failure/"+tc.name, func(t *testing.T) {
			d := approvalDestination(t)
			gate := filepath.Join(t.TempDir(), "gate")
			f := newHarnessFixture(t, "write_failure", approvalProbeHarness(d, approvalProbe{request: true, gate: gate}))
			// Capture the actual API reply, without replacing its handler, to
			// distinguish HTTP 500 allowed:false from Runner's unavailable.
			handler := f.server.Config.Handler
			responses := make(chan *httptest.ResponseRecorder, 2)
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/authorize") {
					handler.ServeHTTP(w, r)
					return
				}
				capture := httptest.NewRecorder()
				handler.ServeHTTP(capture, r)
				responses <- capture
				for k, values := range capture.Header() {
					w.Header()[k] = values
				}
				w.WriteHeader(capture.Code)
				_, _ = w.Write(capture.Body.Bytes())
			})
			f.start()
			f.waitStatus("waiting_approval")
			f.approve()
			f.waitPublishing()
			if _, err := f.db.Exec(`CREATE TRIGGER fail_authorize ` + tc.trigger + ` BEGIN SELECT RAISE(ABORT,'probe write failure'); END`); err != nil {
				t.Fatal(err)
			}
			release(t, gate)
			f.waitStatus("failed")
			f.wantProbeReplies(contract.AuthorizeResponse{Reason: "unavailable"})
			select {
			case response := <-responses:
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				_, hasReason := body["reason"]
				if response.Code != http.StatusInternalServerError || body["allowed"] != false || hasReason {
					t.Fatalf("API reply %d %s; want 500, allowed:false, no reason", response.Code, response.Body)
				}
			default:
				t.Fatal("authorize never reached the Control Plane")
			}
			if len(responses) != 0 {
				t.Fatal("Runner retried authorize after storage failure")
			}
			f.wantNoConsumption(d)
		})
	}
}

func (f *executionFixture) wantProbeReplies(want ...contract.AuthorizeResponse) {
	f.t.Helper()
	if got := f.authorizations(); !reflect.DeepEqual(got, want) {
		f.t.Fatalf("authorize replies %+v; want %+v", got, want)
	}
}

// wantConsumeRecords compares channel replies with independent Control Plane
// records and the destination. Concurrent replies need not be in commit order.
func (f *executionFixture) wantConsumeRecords(d *st.Destination) {
	f.t.Helper()
	replies := f.authorizations()
	want := map[string]int{}
	allowed := 0
	for _, reply := range replies {
		want[fmt.Sprintf("%v/%s", reply.Allowed, reply.Reason)]++
		if reply.Allowed {
			allowed++
		}
	}
	if n := d.Operations(); n != allowed {
		f.t.Fatalf("destination operations=%d; allowed replies=%d", n, allowed)
	}
	var approvalID any
	if rows := f.approvalRows(); len(rows) == 1 {
		approvalID = rows[0].ID
	}
	events, audits := f.consumeRecords()
	for _, records := range [][]map[string]any{events, audits} {
		got := map[string]int{}
		for _, record := range records {
			id, present := record["approvalId"]
			if !present || id != approvalID || record["operation"] != st.ProbeOperation {
				f.t.Fatalf("consume record %v; want approvalId=%v operation=%s", record, approvalID, st.ProbeOperation)
			}
			reason, _ := record["reason"].(string)
			got[fmt.Sprintf("%v/%s", record["allowed"], reason)]++
		}
		if !reflect.DeepEqual(got, want) {
			f.t.Fatalf("consume records %v; want outcomes %v", records, want)
		}
	}
}

func (f *executionFixture) wantNoConsumption(d *st.Destination) {
	f.t.Helper()
	for _, reply := range f.authorizations() {
		if reply.Allowed {
			f.t.Fatalf("unexpected allowed reply %+v", reply)
		}
	}
	if a := f.oneApproval(); a.Status != "approved" || a.UseCount != 0 {
		f.t.Fatalf("approval %+v; want still approved and unused", a)
	}
	var consumed int
	f.scalar(`SELECT count(*) FROM approvals WHERE execution_id=? AND consumed_at IS NOT NULL`, &consumed)
	events, audits := f.consumeRecords()
	if consumed != 0 || len(events) != 0 || len(audits) != 0 || d.Operations() != 0 {
		f.t.Fatalf("consumed=%d events=%v audits=%v operations=%d; want no consumption or operation", consumed, events, audits, d.Operations())
	}
}

func (f *executionFixture) waitProbePublishResult() {
	f.t.Helper()
	// Runner keeps the stage's result JSON at the stage directory root
	// (scratch, where the stage wrote it, is removed after the stage).
	path := filepath.Join(f.config.DataDir, "executions", f.id, "publish-1", "result.json")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		var result contract.Result
		if err == nil && json.Unmarshal(data, &result) == nil {
			if string(result.Status) != "failed" || result.ErrorClass == nil || *result.ErrorClass != "publish_failed" {
				f.t.Fatalf("publish result %s; want failed/publish_failed", data)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("publish result not written; output %s", f.output.String())
}

// waitTerminal waits until the Execution reaches one of the given terminal
// states and fails on any other terminal state.
func (f *executionFixture) waitTerminal(accept ...string) {
	f.t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		f.scalar(`SELECT status FROM executions WHERE id=?`, &status)
		for _, want := range accept {
			if status == want {
				f.t.Logf("execution ended %s", status)
				return
			}
		}
		switch status {
		case "succeeded", "failed", "timed_out", "cancelled", "interrupted":
			f.t.Fatalf("execution ended %s; want one of %v", status, accept)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("execution did not reach %v; output %s", accept, f.output.String())
}
