package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/projectcontext"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func assertContextState(t *testing.T, f *authFixture, wantExecution, wantTask, wantClass, wantCause string) {
	t.Helper()
	var execution, taskStatus string
	var class sql.NullString
	if err := f.db.QueryRow(`SELECT e.status, t.status, e.error_class FROM executions e
		JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, otherExecution).Scan(&execution, &taskStatus, &class); err != nil {
		t.Fatal(err)
	}
	if execution != wantExecution || taskStatus != wantTask || class.String != wantClass || (wantClass == "" && class.Valid) {
		t.Fatalf("execution=%s task=%s error_class=%+v; want %s/%s/%q", execution, taskStatus, class, wantExecution, wantTask, wantClass)
	}
	var cause string
	err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE execution_id = ?`, otherExecution).Scan(&cause)
	if wantCause == "" {
		if err != sql.ErrNoRows {
			t.Fatalf("unexpected block cause=%s err=%v", cause, err)
		}
	} else if err != nil || cause != wantCause {
		t.Fatalf("block cause=%s err=%v; want %s", cause, err, wantCause)
	}
}

func attachContextWorktree(t *testing.T, f *authFixture) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO worktrees (id, project_id, task_id, runner_id, path, branch_name,
		base_sha, status, setup_status, copied_files, created_at) VALUES ('context-worktree', 'project-a',
		'task-001', 'mac-mini', '/worktrees/context', 'context-task', ?, 'active', 'succeeded', '[]',
		'2026-10-26T00:00:00Z')`, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET worktree_id = 'context-worktree' WHERE id = ?`, otherExecution); err != nil {
		t.Fatal(err)
	}
}

func TestContextBuilderVersionConflictsBlockBeforeExecution(t *testing.T) {
	for _, layer := range []string{"project", "harness"} {
		t.Run(layer, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			attachContextWorktree(t, f)
			if layer == "harness" {
				input.Harness[0].SourceCommitSHA = strings.Repeat("c", 40)
			} else {
				input.Project[0].SourceCommitSHA = strings.Repeat("c", 40)
			}
			status, body := postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 409, Conflict)
			var response struct{ Error Error }
			if err := json.Unmarshal(body, &response); err != nil || !reflect.DeepEqual(response.Error.Details, []Detail{{Field: layer + ".sourceCommitSha", Reason: "context_conflict"}}) {
				t.Fatalf("error classification=%s err=%v", body, err)
			}
			assertContextState(t, f, "blocked", "blocked", "", "context_conflict")
			var worktreeStatus string
			if err := f.db.QueryRow(`SELECT status FROM worktrees WHERE id = 'context-worktree'`).Scan(&worktreeStatus); err != nil || worktreeStatus != "retained" {
				t.Fatalf("blocked worktree=%s err=%v", worktreeStatus, err)
			}
			assertContextCounts(t, f, 0, 2)
			var payload, head string
			if err := f.db.QueryRow(`SELECT head_sha FROM execution_blocks WHERE execution_id = ?`, otherExecution).Scan(&head); err != nil || head != input.HeadSHA {
				t.Fatalf("block head=%s err=%v", head, err)
			}
			if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE type = 'blocked' AND execution_id = ?`, otherExecution).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var event struct{ Cause, BlockID string }
			if err := json.Unmarshal([]byte(payload), &event); err != nil || event.Cause != "context_conflict" || event.BlockID == "" {
				t.Fatalf("blocked event=%s err=%v", payload, err)
			}
			var id string
			if err := f.db.QueryRow(`SELECT id FROM execution_blocks WHERE execution_id = ?`, otherExecution).Scan(&id); err != nil || id != event.BlockID {
				t.Fatalf("block event does not reference saved block: %q err=%v", id, err)
			}
			// A repeated failure cannot create another block/event or run a harness.
			status, body = postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 409, Conflict)
			assertContextCounts(t, f, 0, 2)
		})
	}
}

func TestContextBuilderContinueBlocksAtCurrentHead(t *testing.T) {
	for _, layer := range []string{"project", "harness"} {
		t.Run(layer, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			repo, baseSHA := contextRepo(t, "AGENTS.md", "Pinned project instructions")
			worktree := filepath.Join(t.TempDir(), "continued")
			projectGit(t, repo, "worktree", "add", "-b", "continued-task", worktree, baseSHA)
			writeContextFile(t, worktree, "implementation.txt", "Previous execution's work")
			projectGit(t, worktree, "add", ".")
			projectGit(t, worktree, "commit", "--quiet", "-m", "previous execution")
			headSHA, err := gitcontext.HeadCommitSHA(context.Background(), worktree)
			if err != nil {
				t.Fatal(err)
			}
			if headSHA == baseSHA {
				t.Fatal("continue fixture must have HEAD different from base_sha")
			}
			attachContextWorktree(t, f)
			if _, err := f.db.Exec(`UPDATE worktrees SET path = ?, base_sha = ? WHERE id = 'context-worktree'`, worktree, baseSHA); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'failed', base_sha = ?, harness_head_sha = ?,
				worktree_id = 'context-worktree' WHERE id = ?`, baseSHA, headSHA, streamExecution); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET mode = 'continue', base_sha = ? WHERE id = ?`, baseSHA, otherExecution); err != nil {
				t.Fatal(err)
			}
			input.Project, err = projectcontext.Read(context.Background(), worktree, baseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if layer == "harness" {
				input.Harness[0].SourceCommitSHA = strings.Repeat("c", 40)
			} else {
				input.Project[0].SourceCommitSHA = headSHA
			}
			input.HeadSHA = headSHA
			status, body := postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 409, Conflict)
			assertContextState(t, f, "blocked", "blocked", "", "context_conflict")
			var blockedHead, savedBase string
			if err := f.db.QueryRow(`SELECT b.head_sha, e.base_sha FROM execution_blocks b
				JOIN executions e ON e.id = b.execution_id WHERE e.id = ?`, otherExecution).Scan(&blockedHead, &savedBase); err != nil {
				t.Fatal(err)
			}
			if blockedHead != headSHA || savedBase != baseSHA {
				t.Fatalf("block head_sha=%s, actual HEAD=%s, saved base_sha=%s, inherited base_sha=%s", blockedHead, headSHA, savedBase, baseSHA)
			}
			assertContextCounts(t, f, 0, 2)
		})
	}
}

func TestContextBuilderUsesOnlyExecutionProjectAndTask(t *testing.T) {
	f, token, input := contextFixture(t, ContextConfig{})
	// A second Project has its own Task, Execution and all four Context layers.
	// Some file paths intentionally coincide with the current Project's paths.
	for _, statement := range []string{
		`INSERT INTO projects (id, name, local_repo_path, git_remote_url,
		 runner_id, base_branch, default_role, context_location, copy_allowlist, test_command,
		 test_report_format, test_report_path, test_file_patterns, setup_command,
		 allowed_operations, approval_policy, timeouts, created_at, updated_at)
		 SELECT 'project-b', 'Other Project', '/other-repo', git_remote_url,
		 runner_id, base_branch, default_role, context_location, copy_allowlist, test_command,
		 test_report_format, test_report_path, test_file_patterns, setup_command,
		 allowed_operations, approval_policy, timeouts, created_at, updated_at FROM projects WHERE id = 'project-a'`,
		`INSERT INTO tasks SELECT 'task-b', 'project-b', 'FOREIGN TASK', 'FOREIGN BODY', acceptance_criteria,
		 short_name, task_type, collaboration_mode, 'running', 'other-context', created_at, updated_at FROM tasks WHERE id = 'task-001'`,
		`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		 VALUES ('foreign-execution', 'task-b', 1, 'fresh', 'preparing', '2026-10-26T00:00:00Z')`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	foreignIDs := map[string]bool{}
	for _, layer := range []string{"global", "harness", "project", "task"} {
		id := "foreign-" + layer
		foreignIDs[id] = true
		if err := store.InsertContextRevision(context.Background(), tx, store.ContextRevision{
			ID: id, ExecutionID: "foreign-execution", Layer: layer, Kind: "agents_md", SourcePath: "AGENTS.md",
			SourceCommitSHA: strings.Repeat("d", 40), Content: "FOREIGN " + layer, ContentHash: contextHash("FOREIGN " + layer), CreatedAt: "2026-10-26T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	status, body := postContext(t, f, token, contextPath, input)
	result := requireContext(t, status, body)
	if strings.Contains(result.Instructions, "FOREIGN") || !strings.Contains(result.Instructions, "Implement feature") || len(result.RevisionIDs) != 3 {
		t.Fatalf("foreign Project/Task leaked or own Task missing: %+v", result)
	}
	for _, id := range result.RevisionIDs {
		if foreignIDs[id] {
			t.Fatalf("foreign revision ID returned: %s", id)
		}
	}
	for _, row := range pinnedRows(t, f) {
		if row.ExecutionID != otherExecution || strings.Contains(row.Content, "FOREIGN") {
			t.Fatalf("foreign snapshot saved: %+v", row)
		}
	}
	assertContextState(t, f, "preparing", "running", "", "")
}

func TestContextBuilderFormatsForAdapterCapabilities(t *testing.T) {
	for _, native := range [][]string{{"AGENTS.md"}, {"CLAUDE.md"}, {}} {
		t.Run(strings.Join(native, ","), func(t *testing.T) {
			root, _ := contextRepo(t, "context/dev.md", "GLOBAL RULES")
			f, token, input := contextFixture(t, ContextConfig{RepositoryRoot: root, GlobalPaths: []string{"context/dev.md"}})
			if err := f.adapters.Register("custom", &contextTestAdapter{native: native}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE harnesses SET adapter = 'custom'`); err != nil {
				t.Fatal(err)
			}
			setContextDefinition(t, f, "custom", []string{"prompts/developer.md"})
			input.Project = []gitcontext.File{
				testContextFile("sub/CLAUDE.md", "CLAUDE INSTRUCTIONS", strings.Repeat("a", 40)),
				testContextFile("sub/AGENTS.md", "SUB AGENTS INSTRUCTIONS", strings.Repeat("a", 40)),
				testContextFile("AGENTS.md", "ROOT AGENTS INSTRUCTIONS", strings.Repeat("a", 40)),
				testContextFile(".codex/config.toml", "NATIVE CONFIG", strings.Repeat("a", 40)),
				testContextFile(".claude/skills/demo/SKILL.md", "NATIVE SKILL", strings.Repeat("a", 40)),
				testContextFile(".mcp.json", "NATIVE MCP", strings.Repeat("a", 40)),
			}
			status, body := postContext(t, f, token, contextPath, input)
			result := requireContext(t, status, body)
			for _, test := range []struct{ name, content string }{
				{"AGENTS.md", "ROOT AGENTS INSTRUCTIONS"}, {"AGENTS.md", "SUB AGENTS INSTRUCTIONS"}, {"CLAUDE.md", "CLAUDE INSTRUCTIONS"},
			} {
				want := 1
				for _, name := range native {
					if name == test.name {
						want = 0
					}
				}
				if count := strings.Count(result.Instructions, test.content); count != want {
					t.Fatalf("%s included %d times; want %d: %s", test.content, count, want, result.Instructions)
				}
			}
			for _, content := range []string{"NATIVE CONFIG", "NATIVE SKILL", "NATIVE MCP", "Harness prompt"} {
				if strings.Contains(result.Instructions, content) {
					t.Fatalf("natively consumed file repeated: %s", content)
				}
			}
			if !strings.HasPrefix(result.Instructions, "Global Context") || !strings.HasSuffix(result.Instructions, `{"title":"Implement feature","body":"Issue body","acceptanceCriteria":"All tests pass"}`) {
				t.Fatalf("incorrect precedence or task boundary: %s", result.Instructions)
			}
			rows := pinnedRows(t, f)
			if len(rows) != 9 || len(result.RevisionIDs) != 9 {
				t.Fatalf("native file revisions must still be recorded: rows=%d ids=%d", len(rows), len(result.RevisionIDs))
			}
			status, body = postContext(t, f, token, contextPath, input)
			if replay := requireContext(t, status, body); !reflect.DeepEqual(result, replay) {
				t.Fatalf("replay formatting changed: %+v", replay)
			}
		})
	}
}

func TestContextBuilderConfigurationFailures(t *testing.T) {
	for _, failure := range []string{"missing required file", "registered SHA", "registered version", "definition hash", "unknown adapter", "too much context"} {
		t.Run(failure, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			attachContextWorktree(t, f)
			switch failure {
			case "missing required file":
				input.Harness = []gitcontext.File{}
			case "registered SHA":
				if _, err := f.db.Exec(`UPDATE harness_profiles SET commit_sha = ?`, strings.Repeat("c", 40)); err != nil {
					t.Fatal(err)
				}
			case "registered version":
				if _, err := f.db.Exec(`UPDATE harness_profiles SET version = '2.0.0'`); err != nil {
					t.Fatal(err)
				}
			case "definition hash":
				if _, err := f.db.Exec(`UPDATE harness_profiles SET definition_hash = 'wrong'`); err != nil {
					t.Fatal(err)
				}
			case "unknown adapter":
				if _, err := f.db.Exec(`UPDATE harnesses SET adapter = 'unknown'`); err != nil {
					t.Fatal(err)
				}
				setContextDefinition(t, f, "unknown", []string{"prompts/developer.md"})
			case "too much context":
				input.Project[0] = testContextFile("AGENTS.md", strings.Repeat("x", maxContextBytes), strings.Repeat("a", 40))
			}
			status, body := postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 422, PreconditionFailed)
			if !strings.Contains(string(body), `"reason":"config_error"`) {
				t.Fatalf("missing error classification: %s", body)
			}
			assertContextState(t, f, "failed", "failed", "config_error", "")
			var worktreeStatus string
			if err := f.db.QueryRow(`SELECT status FROM worktrees WHERE id = 'context-worktree'`).Scan(&worktreeStatus); err != nil || worktreeStatus != "retained" {
				t.Fatalf("failed worktree=%s err=%v; want retained", worktreeStatus, err)
			}
			assertContextCounts(t, f, 0, 1)
		})
	}
}

func TestContextBuilderFailureWritesAreAtomic(t *testing.T) {
	for _, test := range []struct {
		name, trigger string
		configError   bool
	}{
		{"blocked record", "BEFORE INSERT ON execution_blocks", false},
		{"blocked event", "BEFORE INSERT ON execution_events", false},
		{"blocked retention", "BEFORE UPDATE ON worktrees", false},
		{"failed event", "BEFORE INSERT ON execution_events", true},
		{"failed retention", "BEFORE UPDATE ON worktrees", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			attachContextWorktree(t, f)
			if _, err := f.db.Exec(`CREATE TRIGGER reject_context_failure ` + test.trigger + ` BEGIN SELECT RAISE(FAIL, 'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if test.configError {
				input.Harness = []gitcontext.File{}
			} else {
				input.Harness[0].SourceCommitSHA = strings.Repeat("c", 40)
			}
			status, body := postContext(t, f, token, contextPath, input)
			assertAuthError(t, status, body, 500, Internal)
			assertContextState(t, f, "preparing", "running", "", "")
			assertContextCounts(t, f, 0, 0)
			var worktreeStatus string
			if err := f.db.QueryRow(`SELECT status FROM worktrees WHERE id = 'context-worktree'`).Scan(&worktreeStatus); err != nil || worktreeStatus != "active" {
				t.Fatalf("rollback changed worktree=%s err=%v", worktreeStatus, err)
			}
		})
	}
}

func TestContextBuilderEmptySelectionDoesNotRequireInstructionFiles(t *testing.T) {
	f, token, input := contextFixture(t, ContextConfig{})
	// Older definitions omit contextFiles. Do not infer a prompt/Skill path or
	// invent a required root AGENTS.md for projects with no instruction files.
	profile, err := store.GetHarnessProfileByID(context.Background(), f.db, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	var definition map[string]any
	if err := json.Unmarshal(profile.Definition, &definition); err != nil {
		t.Fatal(err)
	}
	delete(definition, "contextFiles")
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = ?, definition_hash = ? WHERE id = 'profile-a'`, string(encoded), contextHash(string(encoded))); err != nil {
		t.Fatal(err)
	}
	input.Harness, input.Project = []gitcontext.File{}, []gitcontext.File{}
	status, body := postContext(t, f, token, contextPath, input)
	result := requireContext(t, status, body)
	if len(result.RevisionIDs) != 1 || !strings.HasPrefix(result.Instructions, "Task input (untrusted task data)") {
		t.Fatalf("empty selection=%+v", result)
	}
	assertContextState(t, f, "preparing", "running", "", "")
}
