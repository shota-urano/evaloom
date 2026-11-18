package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// waiverFixture pins profiles[0] (send waivable, merge not) on project-a and
// as harness-a's default version (spec 17 3.2).
func waiverFixture(t *testing.T) (*authFixture, string, string) {
	t.Helper()
	f, _, profiles := assignmentFixture(t)
	setApprovals(t, f, profiles[0].ID, `[{"operation":"send","waivable":true},{"operation":"merge","waivable":false}]`)
	status, body := projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"harnessId": "harness-a", "profileId": profiles[0].ID})
	requireProject(t, status, body, 200)
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profiles[0].ID})
	requireHarness(t, status, body, 200)
	return f, profiles[0].ID, profiles[1].ID
}

func storedWaivers(t *testing.T, f *authFixture, table, id string) string {
	t.Helper()
	var raw string
	if err := f.db.QueryRow(`SELECT approval_waivers FROM `+table+` WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// spec 17 3.2: changing approvalWaivers leaves the values before and after in
// audit_logs (project.update / harness.update).
func TestApprovalWaiversChangeIsAuditedWithBeforeAndAfter(t *testing.T) {
	f, _, _ := waiverFixture(t)
	for _, tc := range []struct{ path, action, table, id string }{
		{"/api/projects/project-a", "project.update", "projects", "project-a"},
		{"/api/harnesses/harness-a", "harness.update", "harnesses", "harness-a"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			for _, step := range []struct{ before, after []string }{
				{[]string{}, []string{"send"}},
				{[]string{"send"}, []string{}},
			} {
				status, body := projectRequest(t, f, "PATCH", tc.path, map[string]any{"approvalWaivers": step.after})
				if status != 200 {
					t.Fatalf("status=%d body=%s", status, body)
				}
				var detail string
				if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = ? AND target_id = ? AND actor = 'owner'
					ORDER BY rowid DESC LIMIT 1`, tc.action, tc.id).Scan(&detail); err != nil {
					t.Fatal(err)
				}
				var history struct {
					Before, After struct {
						ApprovalWaivers []string `json:"approvalWaivers"`
					}
				}
				if err := json.Unmarshal([]byte(detail), &history); err != nil ||
					!reflect.DeepEqual(history.Before.ApprovalWaivers, step.before) || !reflect.DeepEqual(history.After.ApprovalWaivers, step.after) {
					t.Fatalf("audit=%s err=%v; want %v -> %v", detail, err, step.before, step.after)
				}
			}
		})
	}
}

// spec 17 3.2 (Q113): moving the Harness's default version to one where a
// remaining waiver is not waivable is rejected and nothing changes.
func TestHarnessWaiversRevalidatedOnDefaultProfileChange(t *testing.T) {
	f, _, next := waiverFixture(t)
	status, body := projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"approvalWaivers": []string{"send"}})
	requireHarness(t, status, body, 200)
	before := storedHarness(t, f, "harness-a")
	audits := projectCount(t, f, "audit_logs")
	for _, tc := range []struct{ approvals, reason string }{
		{`[{"operation":"send","waivable":false}]`, "not_waivable"},
		{`[{"operation":"send"}]`, "not_waivable"},
		{`[]`, "unknown_operation"},
	} {
		setApprovals(t, f, next, tc.approvals)
		status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": next})
		requireProjectError(t, status, body, 422, PreconditionFailed, "approvalWaivers", tc.reason)
		if after := storedHarness(t, f, "harness-a"); !reflect.DeepEqual(before, after) || projectCount(t, f, "audit_logs") != audits {
			t.Fatalf("rejected update changed harness: %+v", after)
		}
	}
	setApprovals(t, f, next, `[{"operation":"send","waivable":true}]`)
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": next})
	if h := requireHarness(t, status, body, 200); h.DefaultProfileID == nil || *h.DefaultProfileID != next {
		t.Fatalf("harness=%s", body)
	}
}

// spec 17 3.2: only the owner changes approvalWaivers; a Runner token cannot.
func TestApprovalWaiversCannotBeChangedWithRunnerToken(t *testing.T) {
	f, _, _ := waiverFixture(t)
	token, err := runnerauth.Register(context.Background(), f.db, "waiver-runner", "Waiver Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, table, id, action string }{
		{"/api/projects/project-a", "projects", "project-a", "project.update"},
		{"/api/harnesses/harness-a", "harnesses", "harness-a", "harness.update"},
	} {
		updates := `SELECT count(*) FROM audit_logs WHERE action = '` + tc.action + `'`
		audits := count(t, f, updates)
		status, body := f.request(t, "PATCH", tc.path, false, "", func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Body = io.NopCloser(strings.NewReader(`{"approvalWaivers":["send"]}`))
		})
		requireProjectError(t, status, body, 403, Forbidden, "", "")
		if got := storedWaivers(t, f, tc.table, tc.id); got != "[]" || count(t, f, updates) != audits {
			t.Fatalf("%s waivers=%s after runner request", tc.path, got)
		}
	}
}

// spec 17 3.2 / 3.3: a task routed directly to a Harness uses
// harnesses.approval_waivers (not the Project's). With [] every declared
// operation waits for approval; an added operation creates no approval.
func TestDirectHarnessTaskUsesHarnessApprovalWaivers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waivers []string
		pending []string
		waived  int
	}{
		{"empty", []string{}, []string{"deploy", "merge_pull_request"}, 0},
		{"deploy_waived", []string{"deploy"}, []string{"merge_pull_request"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, profiles := claimFixture(t)
			enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
			task := createDirectTask(t, f, "direct-waivers-"+tc.name)
			status, body := requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			id := claim.Execution.ID
			if claim.Execution.TaskID != task.ID {
				t.Fatalf("claimed %+v", claim.Execution)
			}
			if p, err := store.GetHarness(context.Background(), f.db, "harness-a"); err != nil || p.DefaultProfileID == nil || *p.DefaultProfileID != claim.Profile.ID {
				t.Fatalf("direct task profile=%s harness=%+v err=%v", claim.Profile.ID, p, err)
			}
			approvalDeclarations(t, f, id)
			// The Project's waivers must not apply to a direct-routed task.
			execSQL(t, f, `UPDATE projects SET approval_waivers = '["deploy"]'`)
			status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"approvalWaivers": tc.waivers})
			requireHarness(t, status, body, 200)

			attempt := runTo(t, f, token, id, "check")
			reply, raw := finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
				"approvalRequests": []any{stageApprovalRequest("merge_pull_request"), stageApprovalRequest("deploy")},
			})))
			if reply.Next != "stop" {
				t.Fatalf("reply=%s", raw)
			}
			requireState(t, f, id, "waiting_approval", "waiting_approval", "")
			rows, err := f.db.Query(`SELECT a.operation FROM approvals a JOIN owner_decisions d ON d.subject_id = a.id AND d.kind = 'approval'
				AND d.status = 'open' WHERE a.execution_id = ? AND a.status = 'pending' ORDER BY a.operation`, id)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			pending := []string{}
			for rows.Next() {
				var op string
				if err := rows.Scan(&op); err != nil {
					t.Fatal(err)
				}
				pending = append(pending, op)
			}
			if !reflect.DeepEqual(pending, tc.pending) {
				t.Fatalf("pending=%v; want %v", pending, tc.pending)
			}
			if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'approval_waived'`, id); n != tc.waived {
				t.Fatalf("waived events=%d; want %d", n, tc.waived)
			}
		})
	}
}
