package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Only registration is exercised here; the API must never execute the Adapter.
type registryOnlyAdapter struct{ adapter.Adapter }

func harnessFixture(t *testing.T) *authFixture {
	t.Helper()
	f := projectFixture(t)
	for _, name := range []string{"codex", "claude_code"} {
		if err := f.adapters.Register(name, &registryOnlyAdapter{}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func harnessInput(t *testing.T, id string) map[string]any {
	t.Helper()
	repo, _ := gitRepository(t)
	// repoUrl is display-only (spec 03 3.3): it need not match a Git remote.
	return map[string]any{"id": id, "name": "Harness " + id,
		"repoPath": repo, "repoUrl": "https://example.com/harness/" + id + ".git"}
}

func requireHarness(t *testing.T, status int, body []byte, want int) model.Harness {
	t.Helper()
	var h model.Harness
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	if err := json.Unmarshal(body, &h); err != nil || h.ID == "" {
		t.Fatalf("harness=%+v body=%s err=%v", h, body, err)
	}
	for _, timestamp := range []string{h.CreatedAt, h.UpdatedAt} {
		if _, err := ParseTimestamp(timestamp); err != nil {
			t.Fatalf("invalid harness timestamp %q", timestamp)
		}
	}
	return h
}

func storedHarness(t *testing.T, f *authFixture, id string) model.Harness {
	t.Helper()
	h, err := store.GetHarness(context.Background(), f.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHarnessesRegisterAndListWithPagination(t *testing.T) {
	f := harnessFixture(t)
	status, body := projectRequest(t, f, "GET", "/api/harnesses", nil)
	if status != 200 || string(body) != "{\"items\":[],\"nextCursor\":\"\"}\n" {
		t.Fatalf("empty list: %d %s", status, body)
	}
	registered := map[string]model.Harness{}
	for _, id := range []string{"harness-b", "harness-a"} {
		input := harnessInput(t, id)
		original := input["repoPath"].(string)
		link := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(original, link); err != nil {
			t.Fatal(err)
		}
		input["repoPath"] = link
		status, body := projectRequest(t, f, "POST", "/api/harnesses", input)
		h := requireHarness(t, status, body, 201)
		real, err := filepath.EvalSymlinks(original)
		if err != nil || h.RepoPath != real || h.RepoURL != input["repoUrl"] || h.DefaultProfileID != nil || h.ApprovalWaivers == nil {
			t.Fatalf("registered=%+v input=%v err=%v", h, input, err)
		}
		registered[id] = h
	}
	endpoint := "/api/harnesses?limit=1"
	for _, id := range []string{"harness-a", "harness-b"} {
		status, body = projectRequest(t, f, "GET", endpoint, nil)
		var page Page[model.Harness]
		if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], registered[id]) {
			t.Fatalf("page: %d %s err=%v", status, body, err)
		}
		if (id == "harness-a") != (page.NextCursor != "") {
			t.Fatalf("unexpected next cursor: %s", body)
		}
		endpoint = "/api/harnesses?limit=1&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if projectCount(t, f, "harnesses") != 2 || projectCount(t, f, "audit_logs") != 2 {
		t.Fatal("each registration must persist one Harness and one audit row")
	}
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'harness.register' AND target_id = 'harness-a'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var history struct {
		Before *model.HarnessSettings
		After  model.HarnessSettings
	}
	if err := json.Unmarshal([]byte(detail), &history); err != nil || history.Before != nil || !reflect.DeepEqual(history.After, registered["harness-a"].HarnessSettings) {
		t.Fatalf("registration audit=%s err=%v", detail, err)
	}
}

func TestHarnessesDuplicatePathsIDsAndConcurrentRegistration(t *testing.T) {
	f := harnessFixture(t)
	input := harnessInput(t, "harness-a")
	status, body := projectRequest(t, f, "POST", "/api/harnesses", input)
	requireHarness(t, status, body, 201)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(input["repoPath"].(string), link); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []any{input["repoPath"], link} {
		input["id"], input["repoPath"] = "harness-b", repo
		status, body = projectRequest(t, f, "POST", "/api/harnesses", input)
		requireProjectError(t, status, body, 409, Conflict, "", "")
	}
	other := harnessInput(t, "harness-a")
	status, body = projectRequest(t, f, "POST", "/api/harnesses", other)
	requireProjectError(t, status, body, 409, Conflict, "", "")
	other["id"] = "harness-c"
	status, body = projectRequest(t, f, "POST", "/api/harnesses", other)
	before := requireHarness(t, status, body, 201)
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-c", map[string]any{"repoPath": input["repoPath"], "repoUrl": input["repoUrl"]})
	requireProjectError(t, status, body, 409, Conflict, "", "")
	if !reflect.DeepEqual(storedHarness(t, f, before.ID), before) || projectCount(t, f, "audit_logs") != 2 {
		t.Fatal("conflict changed persisted data")
	}
	// A single connection serializes duplicate checks and persistence.
	f = harnessFixture(t)
	input = harnessInput(t, "harness-a")
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, id := range []string{"harness-a", "harness-b"} {
		wg.Go(func() {
			request := map[string]any{}
			for k, v := range input {
				request[k] = v
			}
			request["id"] = id
			status, _ := projectRequest(t, f, "POST", "/api/harnesses", request)
			results <- status
		})
	}
	wg.Wait()
	counts := map[int]int{}
	for range 2 {
		counts[<-results]++
	}
	if counts[201] != 1 || counts[409] != 1 || projectCount(t, f, "harnesses") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatalf("concurrent results=%v", counts)
	}
}

func TestHarnessesAuditFailureRollsBackCreateAndUpdate(t *testing.T) {
	f := harnessFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
	before := requireHarness(t, status, body, 201)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_harness_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"name": "Changed"})
	requireProjectError(t, status, body, 500, Internal, "", "")
	if strings.Contains(string(body), "audit unavailable") {
		t.Fatal("internal DB error leaked")
	}
	status, body = projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
	requireProjectError(t, status, body, 500, Internal, "", "")
	if !reflect.DeepEqual(storedHarness(t, f, before.ID), before) || projectCount(t, f, "harnesses") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatal("audit failure did not roll back")
	}
}

func TestHarnessesAuthenticationCSRFAndRunnerSeparation(t *testing.T) {
	f := harnessFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
	before := requireHarness(t, status, body, 201)
	token, err := runnerauth.Register(context.Background(), f.db, "auth-runner", "Auth Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"POST", "PATCH"} {
		endpoint := "/api/harnesses"
		if method == "PATCH" {
			endpoint += "/harness-a"
		}
		status, body = f.request(t, method, endpoint, false, f.csrf, nil)
		requireProjectError(t, status, body, 401, Unauthenticated, "", "")
		status, body = f.request(t, method, endpoint, true, "", nil)
		requireProjectError(t, status, body, 403, Forbidden, "", "")
		status, body = f.request(t, method, endpoint, false, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
		requireProjectError(t, status, body, 403, Forbidden, "", "")
	}
	if !reflect.DeepEqual(storedHarness(t, f, before.ID), before) || projectCount(t, f, "harnesses") != 1 {
		t.Fatal("unauthorized request changed Harness")
	}
}

func TestHarnessesInvalidInputsAndPagination(t *testing.T) {
	f := harnessFixture(t)
	input := harnessInput(t, "harness-a")
	status, body := projectRequest(t, f, "POST", "/api/harnesses", input)
	before := requireHarness(t, status, body, 201)
	for _, tc := range []struct {
		key           string
		value         any
		field, reason string
	}{
		{"name", "", "name", "required"}, {"toolPermissions", nil, "body", "invalid_json"},
		{"toolPermissions", []string{}, "body", "invalid_json"}, {"supportedModels", "model", "body", "invalid_json"},
		{"supportedModels", []any{nil}, "body", "invalid_json"}, {"availableTools", []string{" "}, "body", "invalid_json"},
		{"specialty", "reviewer", "body", "invalid_json"}, {"supportedTasks", []string{}, "body", "invalid_json"},
		{"contractVersion", "2", "body", "invalid_json"}, {"unknown", true, "body", "invalid_json"},
		{"id", "renamed", "body", "invalid_json"}, {"createdAt", "2026-10-26T00:00:00Z", "body", "invalid_json"},
	} {
		status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{tc.key: tc.value})
		requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
	}
	for _, query := range []string{"limit=201", "cursor=invalid!", "cursor=cHJvamVjdHMKYWJj", "limit=2&cursor=%zz"} {
		status, body = projectRequest(t, f, "GET", "/api/harnesses?"+query, nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/missing", map[string]any{})
	requireProjectError(t, status, body, 404, NotFound, "", "")
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/Invalid_ID", map[string]any{})
	requireProjectError(t, status, body, 400, ValidationFailed, "id", "invalid_format")
	if !reflect.DeepEqual(storedHarness(t, f, before.ID), before) || projectCount(t, f, "audit_logs") != 1 {
		t.Fatal("invalid input mutated data")
	}
}

// spec 03 3.3: repoPath must be an absolute directory whose Git top level is
// itself; failures are 422 and name the failed check. repoUrl is https://.
func TestHarnessesRejectRepoPathThatIsNotGitRepositoryRoot(t *testing.T) {
	f := harnessFixture(t)
	input := harnessInput(t, "harness-a")
	status, body := projectRequest(t, f, "POST", "/api/harnesses", input)
	before := requireHarness(t, status, body, 201)
	plain := t.TempDir()
	sub := filepath.Join(before.RepoPath, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(plain, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, reason string }{
		{plain, "not_git_repository"}, {sub, "not_git_repository"}, {filepath.Join(plain, "missing"), "not_found"},
		{file, "not_directory"}, {"relative/repo", "not_absolute"},
	} {
		request := harnessInput(t, "harness-b")
		request["repoPath"] = tc.path
		status, body = projectRequest(t, f, "POST", "/api/harnesses", request)
		requireProjectError(t, status, body, 422, PreconditionFailed, "repoPath", tc.reason)
		status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"repoPath": tc.path})
		requireProjectError(t, status, body, 422, PreconditionFailed, "repoPath", tc.reason)
	}
	for _, tc := range []struct {
		key, value, reason string
	}{
		{"repoUrl", "http://example.com/harness.git", "invalid_format"},
		{"repoUrl", "git@example.com:harness.git", "invalid_format"},
		{"name", strings.Repeat("あ", 201), "too_long"},
	} {
		status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{tc.key: tc.value})
		requireProjectError(t, status, body, 400, ValidationFailed, tc.key, tc.reason)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"name": strings.Repeat("あ", 200)})
	requireHarness(t, status, body, 200)
	if projectCount(t, f, "harnesses") != 1 || projectCount(t, f, "audit_logs") != 2 || storedHarness(t, f, "harness-a").RepoPath != before.RepoPath {
		t.Fatal("rejected repository changed persisted data")
	}
}

// spec 03 3.3: a PATCH that points repoPath at a symbolic link stores the
// resolved real path, and the change is audited with its before/after values.
func TestHarnessesPatchStoresRealRepoPathAndAuditsChange(t *testing.T) {
	f := harnessFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-a"))
	before := requireHarness(t, status, body, 201)
	repo, _ := gitRepository(t)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"repoPath": link})
	after := requireHarness(t, status, body, 200)
	real, err := filepath.EvalSymlinks(repo)
	if err != nil || after.RepoPath != real || storedHarness(t, f, "harness-a").RepoPath != real {
		t.Fatalf("repoPath=%q stored=%q want %q err=%v", after.RepoPath, storedHarness(t, f, "harness-a").RepoPath, real, err)
	}
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'harness.update' AND target_id = 'harness-a'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var history struct{ Before, After model.HarnessSettings }
	if err := json.Unmarshal([]byte(detail), &history); err != nil || history.Before.RepoPath != before.RepoPath || history.After.RepoPath != real {
		t.Fatalf("update audit=%s err=%v", detail, err)
	}
}

// spec 03 3.3: defaultProfileId must be a version of this Harness.
func TestHarnessesRejectDefaultProfileOfAnotherHarness(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
	before := requireHarness(t, status, body, 201)
	audits := projectCount(t, f, "audit_logs")
	for _, profileID := range []string{profiles[0].ID, "01JAAAAAAAAAAAAAAAAAAAAAAA"} {
		status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-b", map[string]any{"defaultProfileId": profileID})
		requireProjectError(t, status, body, 422, PreconditionFailed, "defaultProfileId", "invalid")
	}
	if !reflect.DeepEqual(storedHarness(t, f, "harness-b"), before) || projectCount(t, f, "audit_logs") != audits {
		t.Fatal("rejected defaultProfileId changed persisted data")
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profiles[0].ID})
	if got := requireHarness(t, status, body, 200); got.DefaultProfileID == nil || *got.DefaultProfileID != profiles[0].ID {
		t.Fatalf("own profile rejected: %s", body)
	}
}

// spec 03 3.3, 15 3.6.3: setting defaultProfileId marks the Issues of the
// tasks routed directly to this Harness and held for config_missing_profile
// (or missing_acceptance_criteria) in the same transaction as the write.
func TestHarnessesDefaultProfileRequestsRecheckOfHeldTasks(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
	requireHarness(t, status, body, 201)
	for i, task := range []struct{ project, harness, status, cause string }{
		{"", "harness-a", "blocked", "config_missing_profile"},      // 1: marked
		{"", "harness-a", "blocked", "missing_acceptance_criteria"}, // 2: marked
		{"", "harness-b", "blocked", "config_missing_profile"},      // 3: other Harness
		{"project-a", "", "blocked", "missing_acceptance_criteria"}, // 4: Project route
		{"", "harness-a", "queued", ""},                             // 5: not held
	} {
		id := "task-90000" + string(rune('1'+i))
		issue := "issue-" + string(rune('1'+i))
		if _, err := f.db.Exec(`INSERT INTO tasks (id, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at, source, intake_block_cause)
			VALUES (?, nullif(?, ''), nullif(?, ''), 't', 'b', '', 'solo', ?, ?, 't', 't', 'linear', nullif(?, ''))`,
			id, task.project, task.harness, task.status, "linear:"+id, task.cause); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO linear_issue_states (linear_issue_id, issue_identifier, team_id, ai_ready,
			attach_seq, issue_updated_at, first_seen_at, last_seen_at) VALUES (?, 'ENG-1', 'team-a', 1, 1, 't', 't', 't')`, issue); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url,
			team_id, task_id, attach_seq, created_at) VALUES (?, ?, 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', ?, 1, 't')`,
			"link-"+id, issue, id); err != nil {
			t.Fatal(err)
		}
	}
	marked := func() string {
		t.Helper()
		var got string
		rows, err := f.db.Query(`SELECT linear_issue_id FROM linear_issue_states WHERE recheck_requested_at IS NOT NULL ORDER BY linear_issue_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got += id + ","
		}
		return got
	}
	clear := func() {
		t.Helper()
		if _, err := f.db.Exec(`UPDATE linear_issue_states SET recheck_requested_at = NULL`); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(values map[string]any) {
		t.Helper()
		status, body := projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", values)
		requireHarness(t, status, body, 200)
	}

	patch(map[string]any{"name": "Renamed"})
	if got := marked(); got != "" {
		t.Fatalf("name-only PATCH marked %s", got)
	}
	// A rolled-back write leaves no mark.
	if _, err := f.db.Exec(`CREATE TRIGGER reject_harness_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profiles[0].ID})
	requireProjectError(t, status, body, 500, Internal, "", "")
	if _, err := f.db.Exec(`DROP TRIGGER reject_harness_audit`); err != nil {
		t.Fatal(err)
	}
	if got := marked(); got != "" || storedHarness(t, f, "harness-a").DefaultProfileID != nil {
		t.Fatalf("rolled-back PATCH marked %s", got)
	}

	patch(map[string]any{"defaultProfileId": profiles[0].ID})
	if got := marked(); got != "issue-1,issue-2," {
		t.Fatalf("marked = %s; want issue-1,issue-2,", got)
	}
	clear()
	patch(map[string]any{"defaultProfileId": profiles[0].ID})
	if got := marked(); got != "" {
		t.Fatalf("unchanged defaultProfileId marked %s", got)
	}
	patch(map[string]any{"defaultProfileId": profiles[1].ID})
	if got := marked(); got != "issue-1,issue-2," {
		t.Fatalf("changed defaultProfileId marked %s", got)
	}
}
