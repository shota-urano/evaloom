package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"modernc.org/sqlite/lib"
)

// 0025 removes the development tables and the development columns of
// projects and executions (docs/specs/19-routing-and-stages.md 3.11). The
// remaining rows and the references to projects and executions survive.
func TestDevelopmentRemovalMigrationKeepsRemainingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := open(context.Background(), path, migrationsBefore(t, 25))
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	seedVerificationRecords(t, db)
	mustTaskExec(t, db, `UPDATE projects SET linear_team_id = 'team-a', linear_routing_label = 'repo:a'`)
	mustTaskExec(t, db, `UPDATE executions SET status = 'failed', runner_id = 'mac-mini', role = 'developer',
		harness_id = 'codex-developer', profile_id = 'profile-1', profile_version = '1.2.0', profile_commit_sha = ?,
		worktree_id = 'worktree-1', branch_name = 'evaloom/task-001-change', base_sha = ?, pr_number = 12,
		harness_session_id = 'session-1', error_class = 'config_error', error_message = 'stopped'`,
		strings.Repeat("a", 40), strings.Repeat("a", 40))
	keptProject := `SELECT id, name, runner_id, default_role,
		created_at, updated_at, linear_team_id, linear_routing_label FROM projects ORDER BY id`
	keptExecution := `SELECT id, task_id, seq, mode, status, runner_id, role, harness_id, profile_id,
		profile_version, profile_commit_sha, harness_session_id, error_class, error_message, created_at
		FROM executions ORDER BY id`
	projects, executions := migrationRows(t, db, keptProject), migrationRows(t, db, keptExecution)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = open(context.Background(), path, migrationsBefore(t, 26))
	if err != nil {
		t.Fatal(err)
	}
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
	if got := migrationRows(t, db, keptProject); !reflect.DeepEqual(got, projects) {
		t.Fatalf("projects = %#v; want %#v", got, projects)
	}
	if got := migrationRows(t, db, keptExecution); !reflect.DeepEqual(got, executions) {
		t.Fatalf("executions = %#v; want %#v", got, executions)
	}
	for _, table := range []string{"worktrees", "test_runs", "gate_results", "execution_blocks"} {
		if tableExists(t, db, table) {
			t.Errorf("development table %s remains", table)
		}
	}
	for table, columns := range map[string][]string{
		"projects": {"local_repo_path", "git_remote_url", "base_branch", "context_location", "test_command",
			"test_report_format", "test_report_path", "setup_command", "setup_allowed_hosts", "copy_allowlist", "approval_policy",
			"allowed_operations", "timeouts", "review_role"},
		"executions": {"worktree_id", "branch_name", "base_sha", "resume_point", "harness_head_sha",
			"verified_sha", "pushed_sha", "pr_number", "pr_url"},
	} {
		for _, column := range columns {
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil || count != 0 {
				t.Errorf("development column %s.%s remains (count %d, err %v)", table, column, count, err)
			}
		}
	}
	if violations := migrationRows(t, db, "PRAGMA foreign_key_check"); len(violations) != 0 {
		t.Fatalf("foreign key violations = %#v", violations)
	}
	// References to the rebuilt tables are still enforced.
	_, err = db.Exec(`DELETE FROM projects WHERE id = 'project-a'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`DELETE FROM executions WHERE id = 'execution-1'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`UPDATE projects SET runner_id = 'missing'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	insertRegistryProject(t, db, "project-b", "/repos/project-b")
	_, err = db.Exec(`UPDATE projects SET linear_team_id = 'team-a', linear_routing_label = 'repo:a' WHERE id = 'project-b'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}
