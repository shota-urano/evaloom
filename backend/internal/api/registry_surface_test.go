package api

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func TestProjectCreateAndPatchRejectEveryNonSpecField(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	for _, key := range []string{"id", "defaultRole", "linearTeamId", "linearRoutingLabel", "localRepoPath", "gitUrl", "baseBranch", "testCommand", "setup", "copyAllowlist", "contextPath", "allowedOperations", "timeouts", "approvalPolicy", "nameExtra", "Name", "createdAt", "updatedAt"} {
		for _, method := range []string{"POST", "PATCH"} {
			if method == "POST" && key == "id" {
				continue
			}
			endpoint := "/api/projects/project-a"
			request := map[string]any{key: nil}
			if method == "POST" {
				endpoint = "/api/projects"
				for k, v := range input {
					request[k] = v
				}
				request["id"] = "project-b"
				request[key] = nil
			}
			status, body := projectRequest(t, f, method, endpoint, request)
			assertAuthError(t, status, body, 400, ValidationFailed)
		}
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) || projectCount(t, f, "projects") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatal("invalid fields changed Project or audit")
	}
}

func TestRoutesCRUDPaginationAndUniqueness(t *testing.T) {
	f := projectFixture(t)
	for _, id := range []string{"project-a", "project-b"} {
		status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, id))
		requireProject(t, status, body, 201)
	}
	for _, input := range []map[string]any{
		{"label": "route:a", "projectId": "project-a"},
		{"label": "route:b", "harnessId": "fixture-harness"},
	} {
		status, body := projectRequest(t, f, "POST", "/api/routes", input)
		var route store.Route
		if status != 201 || json.Unmarshal(body, &route) != nil || route.Label != input["label"] {
			t.Fatalf("create=%d %s", status, body)
		}
	}
	for _, input := range []map[string]any{
		{"label": "route:a", "projectId": "project-b"},
		{"label": "route:other", "projectId": "project-a"},
		{"label": "route:other", "harnessId": "fixture-harness"},
	} {
		status, body := projectRequest(t, f, "POST", "/api/routes", input)
		assertAuthError(t, status, body, 409, Conflict)
	}
	status, body := projectRequest(t, f, "GET", "/api/routes?limit=1", nil)
	var page Page[store.Route]
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].Label != "route:a" || page.NextCursor == "" {
		t.Fatalf("list=%d %s", status, body)
	}
	status, body = projectRequest(t, f, "GET", "/api/routes?limit=1&cursor="+url.QueryEscape(page.NextCursor), nil)
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].Label != "route:b" || page.NextCursor != "" {
		t.Fatalf("next=%d %s", status, body)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/routes/route%3Aa", map[string]any{"projectId": "project-b"})
	if status != 200 {
		t.Fatalf("patch=%d %s", status, body)
	}
	status, body = projectRequest(t, f, "DELETE", "/api/routes/route%3Aa", nil)
	if status != 204 || len(body) != 0 {
		t.Fatalf("delete=%d %s", status, body)
	}
	status, body = projectRequest(t, f, "DELETE", "/api/routes/route%3Aa", nil)
	assertAuthError(t, status, body, 404, NotFound)
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action IN ('route.create','route.update','route.delete')`); n != 4 {
		t.Fatalf("route audits=%d", n)
	}
}

func TestRoutesPaginationRoundTripsLabels(t *testing.T) {
	for _, label := range []string{"route:a\nb", "route:a\rb", "route:a\x00b", "route:a%+/?", "route:a日本語"} {
		t.Run(label, func(t *testing.T) {
			f := projectFixture(t)
			status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-a"))
			requireProject(t, status, body, 201)
			for _, input := range []map[string]any{
				{"label": label, "projectId": "project-a"},
				{"label": "route:z", "harnessId": "fixture-harness"},
			} {
				status, body := projectRequest(t, f, "POST", "/api/routes", input)
				var route store.Route
				if status != 201 || json.Unmarshal(body, &route) != nil || route.Label != input["label"] {
					t.Fatalf("create=%d %s", status, body)
				}
			}
			endpoint := "/api/routes?limit=1"
			for i, want := range []string{label, "route:z"} {
				status, body := projectRequest(t, f, "GET", endpoint, nil)
				var page Page[store.Route]
				if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].Label != want {
					t.Fatalf("page %d=%d %s; want label %q", i+1, status, body, want)
				}
				if (page.NextCursor != "") != (i == 0) {
					t.Fatalf("page %d nextCursor=%q", i+1, page.NextCursor)
				}
				endpoint = "/api/routes?limit=1&cursor=" + url.QueryEscape(page.NextCursor)
			}
		})
	}
}

func TestRoutesRejectMalformedLabelCursor(t *testing.T) {
	f := projectFixture(t)
	for _, boundary := range []string{"%bad", "YQ==", base64.RawURLEncoding.EncodeToString([]byte{0xff})} {
		page := NewPage([]string{boundary, "unused"}, 1, "routes", func(id string) string { return id })
		status, body := projectRequest(t, f, "GET", "/api/routes?cursor="+url.QueryEscape(page.NextCursor), nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
}

func TestRoutesRejectInvalidInputsAndRollbackAuditFailure(t *testing.T) {
	f := projectFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-a"))
	requireProject(t, status, body, 201)
	for _, input := range []map[string]any{
		{"label": "route:", "projectId": "project-a"},
		{"label": "other:a", "projectId": "project-a"},
		{"label": " route:a", "projectId": "project-a"},
		{"label": "route:a ", "projectId": "project-a"},
		{"label": "route:" + strings.Repeat("a", 95), "projectId": "project-a"},
		{"label": "route:a"},
		{"label": "route:a", "projectId": "project-a", "harnessId": "fixture-harness"},
		{"label": "route:a", "projectId": nil},
		{"label": "route:a", "projectId": "project-a", "extra": nil},
	} {
		status, body := projectRequest(t, f, "POST", "/api/routes", input)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	status, body = projectRequest(t, f, "POST", "/api/routes", map[string]any{"label": "route:a", "projectId": "missing"})
	assertAuthError(t, status, body, 422, PreconditionFailed)
	stageExec(t, f, `INSERT INTO settings VALUES ('linear.routingLabelPrefix','"custom:"','t'); INSERT INTO settings VALUES ('linear.triggerLabel','"custom:trigger"','t')`)
	for _, label := range []string{"route:a", "custom:trigger"} {
		status, body = projectRequest(t, f, "POST", "/api/routes", map[string]any{"label": label, "projectId": "project-a"})
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	stageExec(t, f, `CREATE TRIGGER fail_route_audit BEFORE INSERT ON audit_logs WHEN NEW.action='route.create' BEGIN SELECT RAISE(ABORT,'audit failed'); END`)
	status, body = projectRequest(t, f, "POST", "/api/routes", map[string]any{"label": "custom:a", "projectId": "project-a"})
	assertAuthError(t, status, body, 500, Internal)
	if n := stageCount(t, f, `SELECT count(*) FROM routes`); n != 0 {
		t.Fatal("failed write persisted route")
	}
}

func TestRegistryDetailRoutesAndRemovedEndpoints(t *testing.T) {
	f, _, _, taskID, approval := waitingApprovalFixture(t)
	for _, endpoint := range []string{"/api/projects/project-a/harnesses", "/api/tasks/" + taskID + "/resume", "/api/approvals/" + approval.ID + "/decision"} {
		status, body := approvalRequest(t, f, "POST", endpoint, `{}`)
		assertAuthError(t, status, body, 404, NotFound)
	}
	status, body := projectRequest(t, f, "GET", "/api/harnesses/fixture-harness", nil)
	if status != 200 {
		t.Fatalf("harness detail=%d %s", status, body)
	}
	status, body = projectRequest(t, f, "GET", "/api/harnesses/missing", nil)
	assertAuthError(t, status, body, 404, NotFound)
	// Profile registration/detail uses a real ULID from its existing fixture.
	pf, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	status, body = postProfile(t, pf, "1.2.0")
	profile := requireProfile(t, status, body, 201)
	status, body = projectRequest(t, pf, "GET", "/api/harness-profiles/"+profile.ID, nil)
	if status != 200 || !strings.Contains(string(body), `"definition"`) {
		t.Fatalf("profile detail=%d %s", status, body)
	}
	status, body = projectRequest(t, pf, "GET", "/api/harness-profiles/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	assertAuthError(t, status, body, 404, NotFound)
}

func TestRoutesRejectTriggerLabelWithDefaultSettings(t *testing.T) {
	f := projectFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-a"))
	requireProject(t, status, body, 201)
	for _, input := range []map[string]any{
		{"label": "ai-ready", "projectId": "project-a"},
		{"label": "ai-ready", "harnessId": "fixture-harness"},
		{"label": "route:a", "projectId": "project-a", "harnessId": "fixture-harness"},
		{"label": "route:a"},
	} {
		status, body := projectRequest(t, f, "POST", "/api/routes", input)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	// The trigger label is rejected even when it carries the routing prefix.
	stageExec(t, f, `INSERT INTO settings VALUES ('linear.triggerLabel','"route:ai-ready"','t')`)
	status, body = projectRequest(t, f, "POST", "/api/routes", map[string]any{"label": "route:ai-ready", "projectId": "project-a"})
	assertAuthError(t, status, body, 400, ValidationFailed)
	if n := stageCount(t, f, `SELECT count(*) FROM routes`); n != 0 {
		t.Fatalf("routes=%d", n)
	}
}

func TestRoutesPatchToLabeledDestinationConflicts(t *testing.T) {
	f := projectFixture(t)
	for _, id := range []string{"project-a", "project-b"} {
		status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, id))
		requireProject(t, status, body, 201)
	}
	for _, input := range []map[string]any{
		{"label": "route:a", "projectId": "project-a"},
		{"label": "route:b", "projectId": "project-b"},
	} {
		if status, body := projectRequest(t, f, "POST", "/api/routes", input); status != 201 {
			t.Fatalf("create=%d %s", status, body)
		}
	}
	status, body := projectRequest(t, f, "PATCH", "/api/routes/route%3Aa", map[string]any{"projectId": "project-b"})
	assertAuthError(t, status, body, 409, Conflict)
	status, body = projectRequest(t, f, "PATCH", "/api/routes/route%3Aa", map[string]any{"label": "route:c", "projectId": "project-a"})
	assertAuthError(t, status, body, 400, ValidationFailed)
	if n := stageCount(t, f, `SELECT count(*) FROM routes WHERE label='route:a' AND project_id='project-a'`); n != 1 {
		t.Fatal("rejected PATCH changed the route")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='route.update'`); n != 0 {
		t.Fatalf("rejected PATCH audited %d times", n)
	}
}

func TestRoutesAuditBeforeAndAfter(t *testing.T) {
	f := projectFixture(t)
	for _, id := range []string{"project-a", "project-b"} {
		status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, id))
		requireProject(t, status, body, 201)
	}
	if status, body := projectRequest(t, f, "POST", "/api/routes", map[string]any{"label": "route:a", "projectId": "project-a"}); status != 201 {
		t.Fatalf("create=%d %s", status, body)
	}
	if status, body := projectRequest(t, f, "PATCH", "/api/routes/route%3Aa", map[string]any{"harnessId": "fixture-harness"}); status != 200 {
		t.Fatalf("patch=%d %s", status, body)
	}
	if status, body := projectRequest(t, f, "DELETE", "/api/routes/route%3Aa", nil); status != 204 {
		t.Fatalf("delete=%d %s", status, body)
	}
	type side struct {
		ProjectID *string `json:"projectId"`
		HarnessID *string `json:"harnessId"`
	}
	destination := func(s *side) string {
		switch {
		case s == nil:
			return "none"
		case s.ProjectID != nil && s.HarnessID == nil:
			return "project:" + *s.ProjectID
		case s.HarnessID != nil && s.ProjectID == nil:
			return "harness:" + *s.HarnessID
		}
		return "invalid"
	}
	want := map[string][2]string{
		"route.create": {"none", "project:project-a"},
		"route.update": {"project:project-a", "harness:fixture-harness"},
		"route.delete": {"harness:fixture-harness", "none"},
	}
	rows, err := f.db.Query(`SELECT action, actor, target_type, target_id, detail FROM audit_logs WHERE action LIKE 'route.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var action, actor, targetType, targetID, raw string
		if err := rows.Scan(&action, &actor, &targetType, &targetID, &raw); err != nil {
			t.Fatal(err)
		}
		var detail struct {
			Before *side `json:"before"`
			After  *side `json:"after"`
		}
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		got := [2]string{destination(detail.Before), destination(detail.After)}
		if actor != "owner" || targetType != "route" || targetID != "route:a" || got != want[action] {
			t.Fatalf("%s audit=%s/%s/%s %v; want %v", action, actor, targetType, targetID, got, want[action])
		}
		seen++
	}
	if err := rows.Err(); err != nil || seen != 3 {
		t.Fatalf("route audits=%d err=%v", seen, err)
	}
}

func TestRouteChangesDoNotMoveQueuedTasks(t *testing.T) {
	f := projectFixture(t)
	for _, id := range []string{"project-a", "project-b"} {
		status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, id))
		requireProject(t, status, body, 201)
	}
	for _, input := range []map[string]any{
		{"label": "route:a", "projectId": "project-a"},
		{"label": "route:h", "harnessId": "fixture-harness"},
	} {
		if status, body := projectRequest(t, f, "POST", "/api/routes", input); status != 201 {
			t.Fatalf("create=%d %s", status, body)
		}
	}
	const now = "2026-11-11T00:00:00Z"
	for _, status := range []string{"queued", "running", "completed"} {
		stageExec(t, f, `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
			VALUES (?, 'project-a', 't', 'b', '[]', 'solo', ?, ?, ?, ?)`, "task-p"+status, status, "key-p"+status, now, now)
		stageExec(t, f, `INSERT INTO tasks (id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
			VALUES (?, 'fixture-harness', 't', 'b', '[]', 'solo', ?, ?, ?, ?)`, "task-h"+status, status, "key-h"+status, now, now)
	}
	if status, body := projectRequest(t, f, "PATCH", "/api/routes/route%3Aa", map[string]any{"projectId": "project-b"}); status != 200 {
		t.Fatalf("patch=%d %s", status, body)
	}
	for _, label := range []string{"route%3Aa", "route%3Ah"} {
		if status, body := projectRequest(t, f, "DELETE", "/api/routes/"+label, nil); status != 204 {
			t.Fatalf("delete=%d %s", status, body)
		}
	}
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id LIKE 'task-p%' AND project_id='project-a' AND harness_id IS NULL`); n != 3 {
		t.Fatalf("project tasks kept=%d", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id LIKE 'task-h%' AND harness_id='fixture-harness' AND project_id IS NULL`); n != 3 {
		t.Fatalf("harness tasks kept=%d", n)
	}
}
