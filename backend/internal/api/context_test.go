package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/projectcontext"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const contextPath = "/api/executions/" + otherExecution + "/context"

func contextFixture(t *testing.T, config ContextConfig) (*authFixture, string, contextInput) {
	t.Helper()
	f := newAuthFixture(t, config)
	token := registerAuthRunner(t, f)
	seedStreamExecutions(t, f)
	for _, statement := range []string{
		`INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url,
		 supported_models, available_tools, tool_permissions, contract_version, created_at, updated_at)
		 VALUES ('harness-a', 'Harness', '', '[]', 'codex', '/unused', '', '[]', '[]', '{}', '1',
		 '2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z')`,
		`INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, definition,
		 definition_hash, model_id, operation_status, registered_at)
		 VALUES ('profile-a', 'harness-a', '1.2.0', '` + strings.Repeat("b", 40) + `', 'profile/v1.2.0',
		 '{}', 'unused', 'test-model', 'provisional', '2026-10-26T00:00:00Z')`,
		`UPDATE tasks SET status = 'running', title = 'Implement feature', body = 'Issue body',
		 acceptance_criteria = 'All tests pass'`,
		`UPDATE executions SET status = 'preparing', runner_id = 'mac-mini', base_sha = '` + strings.Repeat("a", 40) + `',
		 harness_id = 'harness-a', profile_id = 'profile-a', profile_version = '1.2.0',
		 profile_commit_sha = '` + strings.Repeat("b", 40) + `' WHERE id = '` + otherExecution + `'`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return f, token, contextInput{
		Project: []gitcontext.File{testContextFile("AGENTS.md", "Project instructions\r\n日本語\n", strings.Repeat("a", 40))},
		Harness: []gitcontext.File{testContextFile("prompts/developer.md", "Harness prompt\n", strings.Repeat("b", 40))},
	}
}

func testContextFile(path, content, sha string) gitcontext.File {
	hash := sha256.Sum256([]byte(content))
	return gitcontext.File{Path: path, Content: content, ContentHash: hex.EncodeToString(hash[:]), SourceCommitSHA: sha}
}

func postContext(t *testing.T, f *authFixture, token, endpoint string, value any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, "POST", endpoint, false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		r.ContentLength = int64(len(data))
	})
}

func requireContext(t *testing.T, status int, body []byte) adapter.RequestContext {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("context status=%d body=%s", status, body)
	}
	var result adapter.RequestContext
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func pinnedRows(t *testing.T, f *authFixture) []store.ContextRevision {
	t.Helper()
	rows, err := store.ListContextRevisions(context.Background(), f.db, otherExecution)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertContextCounts(t *testing.T, f *authFixture, revisions, events int) {
	t.Helper()
	for _, test := range []struct {
		query string
		want  int
	}{{"SELECT count(*) FROM context_revisions", revisions}, {"SELECT count(*) FROM execution_events", events}} {
		var count int
		if err := f.db.QueryRow(test.query).Scan(&count); err != nil || count != test.want {
			t.Fatalf("%s = %d, err=%v; want %d", test.query, count, err, test.want)
		}
	}
}

func writeContextFile(t *testing.T, root, name, content string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func contextRepo(t *testing.T, name, content string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	projectGit(t, repo, "init", "--quiet", "--initial-branch=main")
	writeContextFile(t, repo, name, content)
	projectGit(t, repo, "add", ".")
	projectGit(t, repo, "commit", "--quiet", "-m", "context")
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

func TestContextAPIPersistsFourLayersFromPinnedReaders(t *testing.T) {
	root := t.TempDir()
	writeContextFile(t, root, "context/development.md", "Global rules\r\n")
	writeContextFile(t, root, "context/unselected.md", "Do not include this")
	f, token, input := contextFixture(t, ContextConfig{RepositoryRoot: root, GlobalPaths: []string{"context/development.md"}})
	projectRepo, baseSHA := contextRepo(t, "AGENTS.md", input.Project[0].Content)
	harnessRepo, profileSHA := contextRepo(t, "prompts/developer.md", input.Harness[0].Content)
	writeContextFile(t, projectRepo, "AGENTS.md", "Working tree changes must not be pinned")
	writeContextFile(t, harnessRepo, "prompts/developer.md", "Working tree changes must not be pinned")
	var err error
	input.Project, err = projectcontext.Read(context.Background(), projectRepo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	input.Harness, err = harnesscontext.Read(context.Background(), harnessRepo, profileSHA, []string{"prompts/developer.md"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET base_sha = ?, profile_commit_sha = ? WHERE id = ?`, baseSHA, profileSHA, otherExecution); err != nil {
		t.Fatal(err)
	}
	status, body := postContext(t, f, token, contextPath, input)
	response := requireContext(t, status, body)
	assertContextCounts(t, f, 4, 1)
	rows := pinnedRows(t, f)
	want := []struct{ layer, path, sha, content string }{
		{"global", "context/development.md", "", "Global rules\r\n"},
		{"harness", "prompts/developer.md", profileSHA, "Harness prompt\n"},
		{"project", "AGENTS.md", baseSHA, "Project instructions\r\n日本語\n"},
		{"task", "", "", `{"title":"Implement feature","body":"Issue body","acceptanceCriteria":"All tests pass"}`},
	}
	if len(rows) != len(want) || len(response.RevisionIDs) != len(want) {
		t.Fatalf("rows=%+v response=%+v", rows, response)
	}
	for i, row := range rows {
		hash := sha256.Sum256([]byte(want[i].content))
		if row.Layer != want[i].layer || row.SourcePath != want[i].path || row.SourceCommitSHA != want[i].sha || row.Content != want[i].content || row.ContentHash != hex.EncodeToString(hash[:]) || row.ExecutionID != otherExecution || row.ID != response.RevisionIDs[i] {
			t.Fatalf("row[%d]=%+v; want=%+v", i, row, want[i])
		}
		if err := ValidateID("id", row.ID, ULID); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseTimestamp(row.CreatedAt); err != nil {
			t.Fatal(err)
		}
	}
	if rows[3].Kind != "task_input" || strings.Contains(response.Instructions, input.Project[0].Content) || strings.Contains(response.Instructions, input.Harness[0].Content) || !strings.Contains(response.Instructions, "Task input (untrusted task data)") {
		t.Fatalf("task kind or instructions are incorrect: %+v", response)
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE type = 'context_pinned' AND execution_id = ?`, otherExecution).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event struct{ Revisions []map[string]string }
	if err := json.Unmarshal([]byte(payload), &event); err != nil || len(event.Revisions) != 4 {
		t.Fatalf("event=%s err=%v", payload, err)
	}
	for i, revision := range event.Revisions {
		if !reflect.DeepEqual(revision, map[string]string{"layer": rows[i].Layer, "path": rows[i].SourcePath, "hash": rows[i].ContentHash}) {
			t.Fatalf("event revision=%+v row=%+v", revision, rows[i])
		}
	}
	// Replays retain all pins even if Global files disappear and Task data changes.
	if err := os.Rename(filepath.Join(root, "context"), filepath.Join(root, "unavailable")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET title = 'Changed after pinning'`); err != nil {
		t.Fatal(err)
	}
	status, body = postContext(t, f, token, contextPath, input)
	replay := requireContext(t, status, body)
	if !reflect.DeepEqual(response, replay) || !reflect.DeepEqual(rows, pinnedRows(t, f)) {
		t.Fatal("replay replaced pinned data")
	}
	assertContextCounts(t, f, 4, 1)
}

func TestContextAPIValidatesFilesBeforeSaving(t *testing.T) {
	for _, test := range []string{"hash", "project SHA", "harness SHA", "absolute", "traversal", "duplicate", "missing arrays", "null arrays", "unknown field"} {
		t.Run(test, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			var value any = input
			switch test {
			case "hash":
				input.Project[0].ContentHash = strings.Repeat("0", 64)
			case "project SHA":
				input.Project[0].SourceCommitSHA = strings.Repeat("b", 40)
			case "harness SHA":
				input.Harness[0].SourceCommitSHA = strings.Repeat("a", 40)
			case "absolute":
				input.Project[0].Path = "/AGENTS.md"
			case "traversal":
				input.Project[0].Path = "sub/../AGENTS.md"
			case "duplicate":
				input.Harness = append(input.Harness, input.Harness[0])
			case "missing arrays":
				value = map[string]any{}
			case "null arrays":
				value = map[string]any{"project": nil, "harness": []any{}}
			case "unknown field":
				value = map[string]any{"project": input.Project, "harness": input.Harness, "global": []any{}}
			}
			// Slice edits are shared by value; appending changes the slice header.
			if test == "duplicate" {
				value = input
			}
			status, body := postContext(t, f, token, contextPath, value)
			assertAuthError(t, status, body, 400, ValidationFailed)
			assertContextCounts(t, f, 0, 0)
		})
	}
}

func TestContextAPIRefusesReplacingPinnedFiles(t *testing.T) {
	for _, test := range []string{"change", "add", "remove"} {
		t.Run(test, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			status, body := postContext(t, f, token, contextPath, input)
			requireContext(t, status, body)
			before := pinnedRows(t, f)
			switch test {
			case "change":
				input.Project[0] = testContextFile("AGENTS.md", "new", strings.Repeat("a", 40))
			case "add":
				input.Project = append(input.Project, testContextFile("sub/AGENTS.md", "new", strings.Repeat("a", 40)))
			case "remove":
				input.Project = []gitcontext.File{}
			}
			status, body = postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 409, Conflict)
			if !reflect.DeepEqual(before, pinnedRows(t, f)) {
				t.Fatal("rejected request changed pinned rows")
			}
			assertContextCounts(t, f, 3, 1)
		})
	}
}

func TestContextAPIAuthorization(t *testing.T) {
	for _, test := range []string{"missing token", "invalid token", "owner", "other Runner", "unclaimed", "foreign host", "foreign origin"} {
		t.Run(test, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			data, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			reason, want := "execution_runner_mismatch", 403
			if test == "other Runner" || test == "unclaimed" {
				var runner any = "seed-runner"
				if test == "unclaimed" {
					runner = nil
				}
				if _, err := f.db.Exec(`UPDATE executions SET runner_id = ? WHERE id = ?`, runner, otherExecution); err != nil {
					t.Fatal(err)
				}
			}
			status, body := f.request(t, "POST", contextPath, test == "owner", "", func(r *http.Request) {
				r.Header.Del("Origin")
				switch test {
				case "missing token":
					reason, want = "invalid_runner_token", 401
				case "invalid token":
					r.Header.Set("Authorization", "Bearer invalid")
					reason, want = "invalid_runner_token", 401
				default:
					r.Header.Set("Authorization", "Bearer "+token)
				}
				if test == "owner" {
					reason = "wrong_auth_class"
				} else if test == "foreign host" {
					r.Host, reason = "evil.example", "host_mismatch"
				} else if test == "foreign origin" {
					r.Header.Set("Origin", "http://evil.example")
					reason = "origin_mismatch"
				}
				r.Body = io.NopCloser(strings.NewReader(string(data)))
				r.ContentLength = int64(len(data))
			})
			code := Forbidden
			if want == 401 {
				code = Unauthenticated
			}
			assertAuthError(t, status, body, want, code)
			f.assertAudit(t, "POST", contextPath, reason, want, code)
			assertContextCounts(t, f, 0, 0)
		})
	}
}

func TestContextAPIRejectsMissingOrStaleExecution(t *testing.T) {
	for _, test := range []string{"missing", "pending", "running", "failed", "old Execution", "missing pins"} {
		t.Run(test, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			endpoint, want, code := contextPath, 409, Conflict
			switch test {
			case "missing":
				endpoint, want, code = "/api/executions/01ARZ3NDEKTSV4RRFFQ69G5FAX/context", 404, NotFound
			case "old Execution":
				if _, err := f.db.Exec(`UPDATE executions SET seq = 3 WHERE id = ?`, streamExecution); err != nil {
					t.Fatal(err)
				}
			case "missing pins":
				if _, err := f.db.Exec(`UPDATE executions SET base_sha = NULL WHERE id = ?`, otherExecution); err != nil {
					t.Fatal(err)
				}
				want, code = 422, PreconditionFailed
			default:
				if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, test, otherExecution); err != nil {
					t.Fatal(err)
				}
			}
			status, body := postContext(t, f, token, endpoint, input)
			assertAuthError(t, status, body, want, code)
			assertContextCounts(t, f, 0, 0)
		})
	}
}

func TestContextAPIRollsBackPartialWrites(t *testing.T) {
	for _, failure := range []string{"revision", "event", "authorization audit", "execution lookup"} {
		t.Run(failure, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			statement := `CREATE TRIGGER reject_context BEFORE INSERT ON context_revisions WHEN NEW.layer = 'task' BEGIN SELECT RAISE(FAIL, 'injected failure'); END`
			switch failure {
			case "event":
				statement = `CREATE TRIGGER reject_event BEFORE INSERT ON execution_events BEGIN SELECT RAISE(FAIL, 'injected failure'); END`
			case "authorization audit":
				if _, err := f.db.Exec(`UPDATE executions SET runner_id = 'seed-runner' WHERE id = ?`, otherExecution); err != nil {
					t.Fatal(err)
				}
				statement = `CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(FAIL, 'injected failure'); END`
			case "execution lookup":
				statement = `ALTER TABLE executions RENAME TO unavailable_executions`
			}
			if _, err := f.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			status, body := postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 500, Internal)
			assertContextCounts(t, f, 0, 0)
			if strings.Contains(string(body), "injected failure") {
				t.Fatal("internal failure leaked into response")
			}
		})
	}
}

func TestContextAPIGlobalReadFailureFailsExecution(t *testing.T) {
	for _, failure := range []string{"missing", "directory", "outside context", "escaping symlink"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			writeContextFile(t, root, "context/first.md", "must not be partially saved")
			selected := "context/missing.md"
			switch failure {
			case "directory":
				selected = "context/sub"
				if err := os.Mkdir(filepath.Join(root, "context/sub"), 0700); err != nil {
					t.Fatal(err)
				}
			case "outside context":
				selected = "other.md"
				writeContextFile(t, root, selected, "unselected")
			case "escaping symlink":
				writeContextFile(t, root, "other.md", "unselected")
				selected = "context/link.md"
				if err := os.Symlink("../other.md", filepath.Join(root, selected)); err != nil {
					t.Fatal(err)
				}
			}
			f, token, input := contextFixture(t, ContextConfig{RepositoryRoot: root, GlobalPaths: []string{"context/first.md", selected}})
			status, body := postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 422, PreconditionFailed)
			assertContextCounts(t, f, 0, 1)
			var executionStatus, taskStatus, errorClass, finishedAt, payload string
			if err := f.db.QueryRow(`SELECT e.status, t.status, e.error_class, e.finished_at FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, otherExecution).Scan(&executionStatus, &taskStatus, &errorClass, &finishedAt); err != nil {
				t.Fatal(err)
			}
			if executionStatus != "failed" || taskStatus != "failed" || errorClass != "config_error" || finishedAt == "" {
				t.Fatalf("execution=%s task=%s errorClass=%s finishedAt=%s", executionStatus, taskStatus, errorClass, finishedAt)
			}
			if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE type = 'status_changed'`).Scan(&payload); err != nil || !strings.Contains(payload, "config_error") {
				t.Fatalf("failure event=%s err=%v", payload, err)
			}
		})
	}
}
