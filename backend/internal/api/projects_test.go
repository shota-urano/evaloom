package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func projectFixture(t *testing.T) *authFixture {
	t.Helper()
	f := newAuthFixture(t)
	if err := store.RegisterRunner(context.Background(), f.db, "mac-mini", "Mac mini", "hash", t.TempDir(), "2026-10-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	return f
}

func projectGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitRepository creates a local repository with an origin remote, as used by
// Harness registration.
func gitRepository(t *testing.T) (repo, remote string) {
	t.Helper()
	dir := t.TempDir()
	repo, remote = filepath.Join(dir, "repo"), filepath.Join(dir, "remote.git")
	projectGit(t, dir, "init", "--quiet", "--initial-branch=main", repo)
	projectGit(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	projectGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "initial")
	projectGit(t, repo, "remote", "add", "origin", remote)
	projectGit(t, repo, "push", "--quiet", "origin", "main")
	return repo, remote
}

func projectInput(t *testing.T, id string) map[string]any {
	t.Helper()
	return map[string]any{"id": id, "name": "Project " + id, "runnerId": "mac-mini", "defaultRole": "developer"}
}

func projectRequest(t *testing.T, f *authFixture, method, endpoint string, value any) (int, []byte) {
	t.Helper()
	var data []byte
	if value != nil {
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	return f.request(t, method, endpoint, true, f.csrf, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		r.ContentLength = int64(len(data))
		r.Header.Set("Content-Type", "application/json")
	})
}

func requireProject(t *testing.T, status int, body []byte, want int) model.Project {
	t.Helper()
	var p model.Project
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	if err := json.Unmarshal(body, &p); err != nil || p.ID == "" {
		t.Fatalf("project=%+v body=%s err=%v", p, body, err)
	}
	for _, timestamp := range []string{p.CreatedAt, p.UpdatedAt} {
		if _, err := ParseTimestamp(timestamp); err != nil {
			t.Fatalf("invalid project timestamp %q", timestamp)
		}
	}
	return p
}

func requireProjectError(t *testing.T, status int, body []byte, want int, code Code, field, reason string) {
	t.Helper()
	assertAuthError(t, status, body, want, code)
	var envelope struct{ Error Error }
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if field != "" && !reflect.DeepEqual(envelope.Error.Details, []Detail{{Field: field, Reason: reason}}) {
		t.Fatalf("details=%+v; want %s %s", envelope.Error.Details, field, reason)
	}
}

func projectCount(t *testing.T, f *authFixture, table string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestProjectsRegisterAndRetrieveTwoProjects(t *testing.T) {
	f := projectFixture(t)
	inputs := []map[string]any{projectInput(t, "project-b"), projectInput(t, "project-a")}
	registered := map[string]model.Project{}
	for _, input := range inputs {
		status, body := projectRequest(t, f, "POST", "/api/projects", input)
		p := requireProject(t, status, body, 201)
		if p.Name != input["name"] || p.RunnerID != "mac-mini" || p.DefaultRole != "developer" {
			t.Fatalf("registered=%+v input=%v", p, input)
		}
		// The development settings moved to software-engineer (spec 19 3.11);
		// 02 projects has no allowed operations or timeouts.
		for _, removed := range []string{"localRepoPath", "gitRemoteUrl", "baseBranch", "contextLocation", "testCommand",
			"setupCommand", "setupAllowedHosts", "copyAllowlist", "approvalPolicy", "allowedOperations", "timeouts", "reviewRole"} {
			if strings.Contains(string(body), `"`+removed+`"`) {
				t.Fatalf("response has removed field %s: %s", removed, body)
			}
		}
		registered[p.ID] = p
		status, body = projectRequest(t, f, "GET", "/api/projects/"+p.ID, nil)
		if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, p) {
			t.Fatalf("GET=%+v; registered=%+v", got, p)
		}
	}
	status, body := projectRequest(t, f, "GET", "/api/projects?limit=1", nil)
	var page Page[model.Project]
	if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Items) != 1 || page.Items[0].ID != "project-a" || page.NextCursor == "" {
		t.Fatalf("first page: %d %s err=%v", status, body, err)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects?limit=1&cursor="+url.QueryEscape(page.NextCursor), nil)
	if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Items) != 1 || page.Items[0].ID != "project-b" || page.NextCursor != "" {
		t.Fatalf("second page: %d %s err=%v", status, body, err)
	}
	if projectCount(t, f, "projects") != 2 || projectCount(t, f, "audit_logs") != 2 {
		t.Fatal("each registration must persist one project and one audit row")
	}
}

func TestProjectsPatchRecordsBeforeAndAfterInAuditLogs(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	patch := map[string]any{"name": "Renamed", "defaultRole": "reviewer"}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", patch)
	after := requireProject(t, status, body, 200)
	if after.ID != before.ID || after.CreatedAt != before.CreatedAt || after.Name != "Renamed" || after.RunnerID != before.RunnerID ||
		after.UpdatedAt == before.UpdatedAt || after.DefaultRole != "reviewer" {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	var actor, targetType, targetID, detail string
	if err := f.db.QueryRow(`SELECT actor, target_type, target_id, detail FROM audit_logs WHERE action = 'project.update'`).Scan(
		&actor, &targetType, &targetID, &detail); err != nil {
		t.Fatal(err)
	}
	var history struct{ Before, After model.ProjectSettings }
	if err := json.Unmarshal([]byte(detail), &history); err != nil || actor != "owner" || targetType != "project" || targetID != "project-a" ||
		!reflect.DeepEqual(history.Before, before.ProjectSettings) || !reflect.DeepEqual(history.After, after.ProjectSettings) {
		t.Fatalf("audit=%s actor=%s target=%s/%s err=%v", detail, actor, targetType, targetID, err)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, after) {
		t.Fatalf("persisted=%+v; want=%+v", got, after)
	}
	if projectCount(t, f, "audit_logs") != 2 {
		t.Fatal("update must append an audit row")
	}
}

func TestProjectsRunnerPreconditionFailsClosed(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	for _, tc := range []struct {
		name, key     string
		value         any
		field, reason string
	}{
		{"missing runner", "runnerId", "missing", "runnerId", "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"POST", "PATCH"} {
				request := map[string]any{tc.key: tc.value}
				endpoint := "/api/projects/project-a"
				if method == "POST" {
					for key, value := range input {
						request[key] = value
					}
					request["id"], request[tc.key] = "project-b", tc.value
					endpoint = "/api/projects"
				}
				status, body := projectRequest(t, f, method, endpoint, request)
				requireProjectError(t, status, body, 422, PreconditionFailed, tc.field, tc.reason)
			}
		})
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) || projectCount(t, f, "projects") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatalf("failed preconditions mutated data: %+v", got)
	}
}

func TestProjectsInvalidInputReturns400Details(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	for _, tc := range []struct {
		key           string
		value         any
		field, reason string
	}{
		{"id", "Invalid_ID", "id", "invalid_format"},
		{"name", "", "name", "required"}, {"name", nil, "name", "required"},
		{"runnerId", "", "runnerId", "required"}, {"defaultRole", "", "defaultRole", "required"},
		{"unknown", true, "body", "invalid_json"}, {"createdAt", "2026-10-26T00:00:00Z", "body", "invalid_json"},
		// The development settings moved to software-engineer (spec 19 3.11);
		// 02 projects has no allowed operations, timeouts or review role.
		{"allowedOperations", map[string]any{}, "body", "invalid_json"},
		{"timeouts", map[string]int{"harnessSec": 1}, "body", "invalid_json"},
		{"reviewRole", nil, "body", "invalid_json"},
		{"localRepoPath", "/repos/project", "body", "invalid_json"},
		{"testCommand", "go test ./...", "body", "invalid_json"},
		{"copyAllowlist", []string{"fixtures/data.json"}, "body", "invalid_json"},
		{"approvalPolicy", map[string]any{}, "body", "invalid_json"},
	} {
		t.Run(tc.key+"/"+tc.reason, func(t *testing.T) {
			request := map[string]any{}
			for key, value := range input {
				request[key] = value
			}
			request[tc.key] = tc.value
			status, body := projectRequest(t, f, "POST", "/api/projects", request)
			requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
		})
	}
	for _, raw := range []string{"null", "[]", "{", `{"id":"a"} {}`, `{"id":true}`} {
		status, body := f.request(t, "POST", "/api/projects", true, f.csrf, func(r *http.Request) {
			r.Body, r.ContentLength = io.NopCloser(strings.NewReader(raw)), int64(len(raw))
		})
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	if projectCount(t, f, "projects") != 0 || projectCount(t, f, "audit_logs") != 0 {
		t.Fatal("invalid input was persisted")
	}
}

func TestProjectsPatchConflictAndImmutableFields(t *testing.T) {
	f := projectFixture(t)
	a, b := projectInput(t, "project-a"), projectInput(t, "project-b")
	for _, input := range []map[string]any{a, b} {
		status, body := projectRequest(t, f, "POST", "/api/projects", input)
		requireProject(t, status, body, 201)
	}
	status, body := projectRequest(t, f, "POST", "/api/projects", a)
	requireProjectError(t, status, body, 409, Conflict, "", "")
	for _, patch := range []map[string]any{{"id": "renamed"}, {"updatedAt": "2026-10-26T00:00:00Z"}, {"name": nil}} {
		status, body = projectRequest(t, f, "PATCH", "/api/projects/project-b", patch)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-b", nil)
	if p := requireProject(t, status, body, 200); p.Name != b["name"] || projectCount(t, f, "audit_logs") != 2 {
		t.Fatalf("rejected PATCH changed project: %+v", p)
	}
}

func TestProjectsAuditFailureRollsBackCreateAndUpdate(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_project_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"name": "Changed"})
	requireProjectError(t, status, body, 500, Internal, "", "")
	if strings.Contains(string(body), "audit unavailable") {
		t.Fatal("internal DB error leaked")
	}
	status, body = projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-b"))
	requireProjectError(t, status, body, 500, Internal, "", "")
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if p := requireProject(t, status, body, 200); !reflect.DeepEqual(p, before) || projectCount(t, f, "projects") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatalf("audit failure did not roll back: %+v", p)
	}
}

func TestProjectsAuthenticationAndCSRFProtectMutations(t *testing.T) {
	f := projectFixture(t)
	for _, method := range []string{"POST", "PATCH"} {
		endpoint := "/api/projects"
		if method == "PATCH" {
			endpoint += "/project-a"
		}
		status, body := f.request(t, method, endpoint, false, f.csrf, nil)
		requireProjectError(t, status, body, 401, Unauthenticated, "", "")
		status, body = f.request(t, method, endpoint, true, "", nil)
		requireProjectError(t, status, body, 403, Forbidden, "", "")
	}
	if projectCount(t, f, "projects") != 0 {
		t.Fatal("unauthorized request created a project")
	}
}

func TestProjectsEmptyListNotFoundAndPaginationErrors(t *testing.T) {
	f := projectFixture(t)
	status, body := projectRequest(t, f, "GET", "/api/projects", nil)
	if status != 200 || string(body) != "{\"items\":[],\"nextCursor\":\"\"}\n" {
		t.Fatalf("empty list: %d %s", status, body)
	}
	for _, method := range []string{"GET", "PATCH"} {
		status, body = projectRequest(t, f, method, "/api/projects/missing", map[string]any{})
		requireProjectError(t, status, body, 404, NotFound, "", "")
	}
	for _, query := range []string{"limit=201", "cursor=invalid!", "cursor=cnVubmVycwph", "limit=2&cursor=%zz"} {
		status, body = projectRequest(t, f, "GET", "/api/projects?"+query, nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
}

func TestProjectsConcurrentRegistrationHasOneWinner(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, name := range []string{"Project one", "Project two"} {
		wg.Go(func() {
			request := map[string]any{}
			for k, v := range input {
				request[k] = v
			}
			request["name"] = name
			status, _ := projectRequest(t, f, "POST", "/api/projects", request)
			results <- status
		})
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[201] != 1 || counts[409] != 1 || projectCount(t, f, "projects") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatalf("concurrent registration results=%v", counts)
	}
}
