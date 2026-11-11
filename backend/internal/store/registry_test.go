package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
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
		FROM projects p
		JOIN runners r ON r.id = p.runner_id
		JOIN harnesses h ON h.id = p.harness_id
		JOIN harness_profiles hp ON hp.id = p.profile_id WHERE p.id='project-a'`).Scan(&runner, &project, &harness, &version, &sha, &status, &definition)
	if err != nil {
		t.Fatal(err)
	}
	if runner != "mac-mini" || project != "project-a" || harness != "codex-developer" ||
		version != "1.2.0" || sha != strings.Repeat("a", 40) || status != "provisional" || definition != `{"profileVersion":"1.2.0","executionConfiguration":{"adapter":"codex"}}` {
		t.Fatalf("unexpected persisted assignment: %q %q %q %q %q %q %q", runner, project, harness, version, sha, status, definition)
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
	if err := db.QueryRow("SELECT profile_id FROM projects WHERE id = 'project-a'").Scan(&profile); err != nil || profile != "profile-1" {
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
	for _, statement := range []string{
		`UPDATE projects SET runner_id='missing'`,
		`UPDATE projects SET harness_id='missing'`,
		`UPDATE projects SET profile_id='missing'`,
		`UPDATE harness_profiles SET harness_id='missing'`,
		`DELETE FROM runners WHERE id='mac-mini'`,
		`DELETE FROM harnesses WHERE id='codex-developer'`,
		`DELETE FROM harness_profiles WHERE id='profile-1'`,
		`INSERT INTO routes VALUES ('route:missing','missing',NULL,'t','t')`,
		`INSERT INTO routes VALUES ('route:missing',NULL,'missing','t','t')`,
	} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
	insertRegistryHarness(t, db, "other", "codex")
	if _, err := insertRegistryProfile(db, "other-profile", "other", "1.0.0", strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`UPDATE harnesses SET default_profile_id='other-profile' WHERE id='codex-developer'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`UPDATE projects SET profile_id='other-profile'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
}

func TestRegistryRoutesHaveExactlyOneUniqueDestination(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	for _, query := range []string{
		`INSERT INTO routes VALUES ('route:both','project-a','codex-developer','t','t')`,
		`INSERT INTO routes VALUES ('route:none',NULL,NULL,'t','t')`,
	} {
		_, err := db.Exec(query)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	for _, query := range []string{
		`INSERT INTO routes VALUES ('route:project','project-a',NULL,'t','t')`,
		`INSERT INTO routes VALUES ('route:harness',NULL,'codex-developer','t','t')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`INSERT INTO routes VALUES ('route:second-project','project-a',NULL,'t','t')`,
		`INSERT INTO routes VALUES ('route:second-harness',NULL,'codex-developer','t','t')`,
	} {
		_, err := db.Exec(query)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	}
	_, err := db.Exec(`INSERT INTO routes VALUES ('route:project',NULL,'codex-developer','t','t')`)
	var constraint *sqlite.Error
	if !errors.As(err, &constraint) {
		t.Fatal("duplicate label accepted", err)
	}
}

func TestRegistryColumnsMatchSpecification(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	for table, want := range map[string]string{
		"projects":  "id,name,harness_id,profile_id,params,approval_waivers,runner_id,created_at,updated_at",
		"runners":   "id,name,token_hash,status,last_heartbeat_at,boot_id,created_at",
		"harnesses": "id,name,repo_url,repo_path,default_profile_id,approval_waivers,created_at,updated_at",
		"routes":    "label,project_id,harness_id,created_at,updated_at",
	} {
		var got string
		if err := db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_table_info(?) ORDER BY cid)`, table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s columns=%s want=%s err=%v", table, got, want, err)
		}
	}
	if tableExists(t, db, "project_harnesses") {
		t.Fatal("obsolete project_harnesses remains")
	}
}

func TestRegistryRejectsUnknownEnumValues(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "evaloom.db"))
	seedRegistry(t, db)
	for _, statement := range []string{
		`UPDATE runners SET status = 'unknown'`,

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
	legacy := len(migrationRows(t, db, `SELECT name FROM pragma_table_info('runners') WHERE name='worktree_root'`)) != 0
	rootColumn, rootValue := "", ""
	if legacy {
		rootColumn = ", worktree_root"
		rootValue = ", '/worktrees'"
	}
	if _, err := db.Exec(`INSERT INTO runners (id,name,token_hash,status,last_heartbeat_at,created_at`+rootColumn+`)
 VALUES ('mac-mini','Mac mini','hashed-token','online',?,?`+rootValue+`)`, registryTime, registryTime); err != nil {
		t.Fatal(err)
	}
	insertRegistryHarness(t, db, "codex-developer", "codex")
	if _, err := insertRegistryProfile(db, "profile-1", "codex-developer", "1.2.0", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	insertRegistryProject(t, db, "project-a", "/repos/project-a")
	if legacy {
		if _, err := db.Exec(`INSERT INTO project_harnesses VALUES ('project-a','developer','codex-developer','profile-1',?)`, registryTime); err != nil {
			t.Fatal(err)
		}
	}
}

// insertRegistryProject also seeds databases migrated only partway, for the
// upgrade tests. It fills the columns the projects table has at that point,
// including development columns that a later migration removed.
func insertRegistryProject(t *testing.T, db *sql.DB, id, path string) {
	t.Helper()
	values := map[string]any{
		"id": id, "name": "Project", "harness_id": "codex-developer", "profile_id": "profile-1", "local_repo_path": path,
		"git_remote_url": "https://example.invalid/project.git", "runner_id": "mac-mini",
		"base_branch": "main", "default_role": "developer", "context_location": "AGENTS.md",
		"copy_allowlist": "[]", "test_file_patterns": "[]", "setup_allowed_hosts": "[]",
		"allowed_operations": "{}", "approval_policy": `{"mergePullRequest":"require_approval","version":1}`,
		"timeouts": `{"harnessSec":600}`, "created_at": registryTime, "updated_at": registryTime,
	}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('projects') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	var args []any
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		if value, ok := values[column]; ok {
			columns = append(columns, column)
			args = append(args, value)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO projects (`+strings.Join(columns, ", ")+`) VALUES (?`+
		strings.Repeat(", ?", len(columns)-1)+`)`, args...)
	if err != nil {
		t.Fatal(err)
	}
}

func insertRegistryHarness(t *testing.T, db *sql.DB, id, adapter string) {
	t.Helper()
	var legacy bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('harnesses') WHERE name='adapter')`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	var err error
	if legacy {
		_, err = db.Exec(`INSERT INTO harnesses (id,name,specialty,supported_tasks,adapter,repo_path,repo_url,supported_models,available_tools,tool_permissions,contract_version,created_at,updated_at)
 VALUES (?,'Developer','developer','["development"]',?,'/repos/harness','https://example.invalid/harness.git','["test-model"]','[]','{}','1',?,?)`, id, adapter, registryTime, registryTime)
	} else {
		_, err = db.Exec(`INSERT INTO harnesses (id,name,repo_path,repo_url,created_at,updated_at) VALUES (?,'Harness','/repos/harness','https://example.invalid/harness.git',?,?)`, id, registryTime, registryTime)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func insertRegistryProfile(db *sql.DB, id, harness, version, sha string) (sql.Result, error) {
	var legacy bool
	if err := db.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM pragma_table_info('harness_profiles') WHERE name='contract_version')`).Scan(&legacy); err != nil {
		return nil, err
	}
	contractColumn, contractValue := "", ""
	if !legacy {
		contractColumn = ", contract_version"
		contractValue = ", '1'"
	}
	return db.Exec(`INSERT INTO harness_profiles (id,harness_id,version,commit_sha,tag_name,definition,definition_hash,model_id,operation_status,registered_at`+contractColumn+`)
 VALUES (?,?,?,?,?,?,?,'test-model','provisional',?`+contractValue+`)`, id, harness, version, sha, "profile/v"+version,
		`{"profileVersion":"`+version+`","executionConfiguration":{"adapter":"codex"}}`, strings.Repeat("d", 64), registryTime)
}

func assertRegistryConstraint(t *testing.T, err error, want int) {
	t.Helper()
	var constraint *sqlite.Error
	if !errors.As(err, &constraint) || constraint.Code() != want {
		t.Fatalf("error = %v; want SQLite constraint code %d", err, want)
	}
}
