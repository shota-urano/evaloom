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
)

// spec 17 3.2 (Q60): registration without approvalPolicy stores the default
// {"version":1,"mergePullRequest":"require_approval"}.
func TestProjectsApprovalPolicyDefaultsToRequireApproval(t *testing.T) {
	f := projectFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-a"))
	requireProject(t, status, body, 201)
	var stored string
	if err := f.db.QueryRow(`SELECT approval_policy FROM projects WHERE id = 'project-a'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(stored), &policy); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"version": float64(1), "mergePullRequest": "require_approval"}; !reflect.DeepEqual(policy, want) {
		t.Fatalf("stored approval_policy = %s; want %v", stored, want)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if !strings.Contains(string(body), `"approvalPolicy":{"mergePullRequest":"require_approval","version":1}`) {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

// spec 17 3.2: only mergePullRequest with require_approval / direct_push and
// version 1 are accepted. Other keys and unknown values are 400 on both
// registration and update, and a rejected update changes nothing.
func TestProjectsApprovalPolicyRejectsOtherKeysAndUnknownValues(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	for _, tc := range []struct {
		name          string
		policy        any
		field, reason string
	}{
		{"empty", map[string]any{}, "approvalPolicy.version", "invalid"},
		{"other operation", map[string]any{"version": 1, "mergePullRequest": "require_approval", "deployProduction": "direct_push"}, "approvalPolicy", "invalid"},
		{"other key only", map[string]any{"publish": "ask"}, "approvalPolicy", "invalid"},
		{"unknown value", map[string]any{"version": 1, "mergePullRequest": "auto_merge"}, "approvalPolicy.mergePullRequest", "invalid"},
		{"non-string value", map[string]any{"version": 1, "mergePullRequest": true}, "approvalPolicy.mergePullRequest", "invalid"},
		{"missing mergePullRequest", map[string]any{"version": 1}, "approvalPolicy.mergePullRequest", "invalid"},
		{"missing version", map[string]any{"mergePullRequest": "direct_push"}, "approvalPolicy.version", "invalid"},
		{"unknown version", map[string]any{"version": 2, "mergePullRequest": "direct_push"}, "approvalPolicy.version", "invalid"},
		{"string version", map[string]any{"version": "1", "mergePullRequest": "direct_push"}, "approvalPolicy.version", "invalid"},
		{"fractional version", map[string]any{"version": json.Number("1.0"), "mergePullRequest": "direct_push"}, "approvalPolicy.version", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{}
			for key, value := range input {
				request[key] = value
			}
			request["id"], request["approvalPolicy"] = "project-b", tc.policy
			status, body := projectRequest(t, f, "POST", "/api/projects", request)
			requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
			status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"approvalPolicy": tc.policy})
			requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
		})
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) || projectCount(t, f, "projects") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatalf("rejected policies mutated data: %+v", got)
	}
}

// spec 17 3.2: switching to direct_push and back is recorded in audit_logs
// with the policy before and after the change.
func TestProjectsApprovalPolicyChangeIsAudited(t *testing.T) {
	f := projectFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-a"))
	requireProject(t, status, body, 201)
	type policy struct {
		Version          int    `json:"version"`
		MergePullRequest string `json:"mergePullRequest"`
	}
	for i, step := range []struct{ from, to string }{{"require_approval", "direct_push"}, {"direct_push", "require_approval"}} {
		status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a",
			map[string]any{"approvalPolicy": map[string]any{"version": 1, "mergePullRequest": step.to}})
		after := requireProject(t, status, body, 200)
		if after.ApprovalPolicy["mergePullRequest"] != step.to {
			t.Fatalf("response policy = %#v; want %s", after.ApprovalPolicy, step.to)
		}
		rows, err := f.db.Query(`SELECT actor, action, target_type, target_id, detail FROM audit_logs ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var actor, action, targetType, targetID, detail string
		count := 0
		for rows.Next() {
			if err := rows.Scan(&actor, &action, &targetType, &targetID, &detail); err != nil {
				t.Fatal(err)
			}
			count++
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if count != i+2 || actor != "owner" || action != "project.update" || targetType != "project" || targetID != "project-a" {
			t.Fatalf("audit count=%d last=%s %s %s/%s", count, actor, action, targetType, targetID)
		}
		var history struct {
			Before, After struct {
				ApprovalPolicy policy `json:"approvalPolicy"`
			}
		}
		if err := json.Unmarshal([]byte(detail), &history); err != nil {
			t.Fatal(err)
		}
		if history.Before.ApprovalPolicy != (policy{1, step.from}) || history.After.ApprovalPolicy != (policy{1, step.to}) {
			t.Fatalf("audit detail=%s; want %s -> %s", detail, step.from, step.to)
		}
	}
}

// spec 17 3.2 and 6: a Runner token cannot change the approval policy. The
// owner API rejects it with 403 and records the rejection in audit_logs.
func TestProjectsApprovalPolicyRunnerTokenCannotChange(t *testing.T) {
	f := projectFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-a"))
	before := requireProject(t, status, body, 201)
	token, err := runnerauth.Register(context.Background(), f.db, "runner-2", "Runner 2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := `{"approvalPolicy":{"version":1,"mergePullRequest":"direct_push"}}`
	for _, withCookie := range []bool{false, true} {
		status, body = f.request(t, "PATCH", "/api/projects/project-a", withCookie, f.csrf, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Body = io.NopCloser(strings.NewReader(data))
			r.ContentLength = int64(len(data))
		})
		assertAuthError(t, status, body, 403, Forbidden)
	}
	var stored string
	if err := f.db.QueryRow(`SELECT approval_policy FROM projects WHERE id = 'project-a'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != `{"mergePullRequest":"require_approval","version":1}` {
		t.Fatalf("Runner token changed approval_policy to %s", stored)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) {
		t.Fatalf("Runner token mutated the Project: %+v", got)
	}
	var rejected, updates int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'auth.reject' AND target_id = '/api/projects/project-a'
		AND json_extract(detail, '$.reason') = 'wrong_auth_class' AND json_extract(detail, '$.status') = 403`).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'project.update'`).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if rejected != 2 || updates != 0 {
		t.Fatalf("auth.reject rows=%d project.update rows=%d; want 2 and 0", rejected, updates)
	}
}
