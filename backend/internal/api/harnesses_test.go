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
	repo, remote := gitRepository(t)
	return map[string]any{"id": id, "name": "Harness " + id,
		"repoPath": repo, "repoUrl": remote}
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
