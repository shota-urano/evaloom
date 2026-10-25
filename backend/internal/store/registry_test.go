package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

const registryTime = "2026-10-25T00:00:00Z"

func TestRegistryPersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db := openRegistry(t, path)
	seedRegistry(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	var runner, project, harness, version, sha, status, definition string
	err := db.QueryRow(`SELECT r.id, p.id, h.id, hp.version, hp.commit_sha,
		hp.operation_status, hp.definition
		FROM project_harnesses ph
		JOIN projects p ON p.id = ph.project_id
		JOIN runners r ON r.id = p.runner_id
		JOIN harnesses h ON h.id = ph.harness_id
		JOIN harness_profiles hp ON hp.id = ph.profile_id
		WHERE ph.role = 'developer'`).Scan(&runner, &project, &harness, &version, &sha, &status, &definition)
	if err != nil {
		t.Fatal(err)
	}
	if runner != "mac-mini" || project != "project-a" || harness != "codex-developer" ||
		version != "1.2.0" || sha != strings.Repeat("a", 40) || status != "provisional" || definition != `{"profileVersion":"1.2.0"}` {
		t.Fatalf("unexpected persisted assignment: %q %q %q %q %q %q %q", runner, project, harness, version, sha, status, definition)
	}
	var review, command, format, report, setup sql.NullString
	if err := db.QueryRow(`SELECT review_role, test_command, test_report_format,
		test_report_path, setup_command FROM projects WHERE id = 'project-a'`).Scan(&review, &command, &format, &report, &setup); err != nil {
		t.Fatal(err)
	}
	if review.Valid || command.Valid || format.Valid || report.Valid || setup.Valid {
		t.Fatal("optional project settings must preserve NULL")
	}
}

func TestRegistryProfileVersionIsUniqueWithinHarness(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	for _, sha := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		_, err := insertRegistryProfile(db, "duplicate", "codex-developer", "1.2.0", sha)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	}
	// A new version of the same harness and the same version of another
	// harness must both be registrable.
	if _, err := insertRegistryProfile(db, "next", "codex-developer", "1.3.0", strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	insertRegistryHarness(t, db, "claude-developer", "claude_code")
	if _, err := insertRegistryProfile(db, "other", "claude-developer", "1.2.0", strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	var profile string
	if err := db.QueryRow("SELECT profile_id FROM project_harnesses WHERE project_id = 'project-a' AND role = 'developer'").Scan(&profile); err != nil || profile != "profile-1" {
		t.Fatalf("registering a version changed the assignment: profile = %q, err = %v", profile, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM harness_profiles").Scan(&count); err != nil || count != 3 {
		t.Fatalf("profiles = %d, err = %v; want 3", count, err)
	}
}

func TestRegistryRejectsInvalidForeignKeys(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	for _, test := range []struct {
		name, insert, update, remove string
	}{
		{
			"project runner",
			`INSERT INTO projects SELECT 'bad', name, '/bad', git_remote_url, 'missing',
			base_branch, default_role, review_role, context_location, test_command,
			test_report_format, test_report_path, setup_command, copy_allowlist,
			test_file_patterns, allowed_operations, approval_policy, timeouts,
			created_at, updated_at FROM projects WHERE id = 'project-a'`,
			`UPDATE projects SET runner_id = 'missing' WHERE id = 'project-a'`,
			`DELETE FROM runners WHERE id = 'mac-mini'`,
		},
		{
			"profile harness",
			`INSERT INTO harness_profiles SELECT 'bad', 'missing', version, commit_sha,
			tag_name, definition, definition_hash, model_id, operation_status,
			registered_at FROM harness_profiles WHERE id = 'profile-1'`,
			`UPDATE harness_profiles SET harness_id = 'missing' WHERE id = 'profile-1'`,
			`DELETE FROM harnesses WHERE id = 'codex-developer'`,
		},
		{
			"assignment project",
			`INSERT INTO project_harnesses VALUES ('missing', 'developer', 'codex-developer', 'profile-1', '` + registryTime + `')`,
			`UPDATE project_harnesses SET project_id = 'missing' WHERE project_id = 'project-a'`,
			`DELETE FROM projects WHERE id = 'project-a'`,
		},
		{
			"assignment harness",
			`INSERT INTO project_harnesses VALUES ('project-a', 'other', 'missing', 'profile-1', '` + registryTime + `')`,
			`UPDATE project_harnesses SET harness_id = 'missing' WHERE project_id = 'project-a'`,
			`DELETE FROM harnesses WHERE id = 'codex-developer'`,
		},
		{
			"assignment profile",
			`INSERT INTO project_harnesses VALUES ('project-a', 'other', 'codex-developer', 'missing', '` + registryTime + `')`,
			`UPDATE project_harnesses SET profile_id = 'missing' WHERE project_id = 'project-a'`,
			`DELETE FROM harness_profiles WHERE id = 'profile-1'`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Exercise the connection hook as well as the initial connection.
			db.SetMaxIdleConns(0)
			for _, statement := range []string{test.insert, test.update, test.remove} {
				_, err := db.Exec(statement)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
			}
		})
	}
}

func TestRegistryAssignmentKeyAndSharedHarness(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	_, err := db.Exec(`INSERT INTO project_harnesses VALUES ('project-a', 'developer', 'codex-developer', 'profile-1', ?)`, registryTime)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
	insertRegistryProject(t, db, "project-b", "/repos/project-b")
	if _, err := insertRegistryProfile(db, "profile-2", "codex-developer", "2.0.0", strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project_harnesses VALUES ('project-b', 'developer', 'codex-developer', 'profile-2', ?)`, registryTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project_harnesses VALUES ('project-a', 'other-role', 'codex-developer', 'profile-2', ?)`, registryTime); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE projects SET local_repo_path = '/repos/project-a' WHERE id = 'project-b'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}

func TestRegistryRejectsUnknownEnumValues(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	for _, statement := range []string{
		`UPDATE runners SET status = 'unknown'`,
		`UPDATE harnesses SET adapter = 'unknown'`,
		`UPDATE harness_profiles SET operation_status = 'official'`,
		`UPDATE harness_profiles SET operation_status = 'unknown'`,
	} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
}

func openRegistry(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedRegistry(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO runners
		(id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('mac-mini', 'Mac mini', 'hashed-token', '/worktrees', 'online', ?, ?)`, registryTime, registryTime); err != nil {
		t.Fatal(err)
	}
	insertRegistryProject(t, db, "project-a", "/repos/project-a")
	insertRegistryHarness(t, db, "codex-developer", "codex")
	if _, err := insertRegistryProfile(db, "profile-1", "codex-developer", "1.2.0", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project_harnesses
		(project_id, role, harness_id, profile_id, assigned_at)
		VALUES ('project-a', 'developer', 'codex-developer', 'profile-1', ?)`, registryTime); err != nil {
		t.Fatal(err)
	}
}

func insertRegistryProject(t *testing.T, db *sql.DB, id, path string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects
		(id, name, local_repo_path, git_remote_url, runner_id, base_branch,
		default_role, review_role, context_location, test_command, test_report_format,
		test_report_path, setup_command, copy_allowlist, test_file_patterns,
		allowed_operations, approval_policy, timeouts, created_at, updated_at)
		VALUES (?, 'Project', ?, 'https://example.invalid/project.git', 'mac-mini',
		'main', 'developer', NULL, 'AGENTS.md', NULL, NULL, NULL, NULL,
		'[]', '[]', '{}', '{}', '{"setupSec":60,"harnessSec":600,"testSec":120}', ?, ?)`,
		id, path, registryTime, registryTime)
	if err != nil {
		t.Fatal(err)
	}
}

func insertRegistryHarness(t *testing.T, db *sql.DB, id, adapter string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO harnesses
		(id, name, specialty, supported_tasks, adapter, repo_path, repo_url,
		supported_models, available_tools, tool_permissions, contract_version, created_at, updated_at)
		VALUES (?, 'Developer', 'developer', '["development"]', ?, '/repos/harness',
		'https://example.invalid/harness.git', '["test-model"]', '[]', '{}', '1', ?, ?)`,
		id, adapter, registryTime, registryTime)
	if err != nil {
		t.Fatal(err)
	}
}

func insertRegistryProfile(db *sql.DB, id, harness, version, sha string) (sql.Result, error) {
	return db.Exec(`INSERT INTO harness_profiles
		(id, harness_id, version, commit_sha, tag_name, definition, definition_hash,
		model_id, operation_status, registered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'test-model', 'provisional', ?)`,
		id, harness, version, sha, "profile/v"+version,
		`{"profileVersion":"`+version+`"}`, strings.Repeat("d", 64), registryTime)
}

func assertRegistryConstraint(t *testing.T, err error, want int) {
	t.Helper()
	var constraint *sqlite.Error
	if !errors.As(err, &constraint) || constraint.Code() != want {
		t.Fatalf("error = %v; want SQLite constraint code %d", err, want)
	}
}
