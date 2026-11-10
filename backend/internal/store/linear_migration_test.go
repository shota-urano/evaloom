package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"modernc.org/sqlite/lib"
)

const legacyTaskColumns = `id, project_id, title, body, acceptance_criteria, short_name,
	task_type, collaboration_mode, status, idempotency_key, created_at, updated_at`

// registryProjectColumns are the 0001 projects columns that remained until
// migration 0025 removed the development columns.
const registryProjectColumns = `id, name, local_repo_path, git_remote_url, runner_id, base_branch,
	default_role, review_role, context_location, test_command, test_report_format,
	test_report_path, setup_command, copy_allowlist,
	allowed_operations, approval_policy, timeouts, created_at, updated_at`

func TestLinearMigrationPreservesPhase1DataAndReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := open(context.Background(), path, beforeLinearMigration(t))
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	seedVerificationRecords(t, db)
	if _, err := insertTask(db, "task-002", "key-2", "completed"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `UPDATE executions SET worktree_id = 'worktree-1'`)
	mustTaskExec(t, db, `INSERT INTO execution_events (execution_id, type, payload, created_at)
		VALUES ('execution-1', 'log', '{}', ?)`, registryTime)
	mustTaskExec(t, db, `INSERT INTO notification_requests (execution_id, status, created_at)
		VALUES ('execution-1', 'blocked', ?)`, registryTime)
	queries := []string{
		"SELECT " + legacyTaskColumns + " FROM tasks ORDER BY id",
		"SELECT " + registryProjectColumns + " FROM projects ORDER BY id",
	}
	for _, table := range []string{"worktrees", "executions", "execution_events", "artifacts",
		"test_runs", "gate_results", "execution_blocks", "audit_logs", "usage_records", "notification_requests"} {
		queries = append(queries, "SELECT * FROM "+table+" ORDER BY 1")
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = migrationRows(t, db, query)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A second open also checks that the migration is not applied twice.
	// Stop before 0025, which removes the development tables and columns.
	for range 2 {
		db, err = open(context.Background(), path, migrationsBefore(t, 25))
		if err != nil {
			t.Fatal(err)
		}
		for i, query := range queries {
			if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before[i]) {
				t.Fatalf("migration changed %s: got %#v, want %#v", query, got, before[i])
			}
		}
		if got := migrationRows(t, db, `SELECT source, intake_block_cause FROM tasks ORDER BY id`); !reflect.DeepEqual(got, [][]any{{"manual", nil}, {"manual", nil}}) {
			t.Fatalf("migrated task origins = %#v", got)
		}
		if got := migrationRows(t, db, `SELECT linear_team_id, linear_routing_label FROM projects`); !reflect.DeepEqual(got, [][]any{{nil, nil}}) {
			t.Fatalf("migrated routing = %#v", got)
		}
		assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24})
		if violations := migrationRows(t, db, "PRAGMA foreign_key_check"); len(violations) != 0 {
			t.Fatalf("foreign key violations = %#v", violations)
		}
		_, err := db.Exec(`DELETE FROM tasks WHERE id = 'task-001'`)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinearMigrationProjectRoutingUnique(t *testing.T) {
	db := openTaskTables(t)
	insertRegistryProject(t, db, "project-b", "/repos/project-b")
	insertRegistryProject(t, db, "project-c", "/repos/project-c")
	mustTaskExec(t, db, `UPDATE projects SET linear_team_id = 'team-a',
		linear_routing_label = 'repo:a' WHERE id = 'project-a'`)
	_, err := db.Exec(`UPDATE projects SET linear_team_id = 'team-a',
		linear_routing_label = 'repo:a' WHERE id = 'project-b'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	// Both components take part in the key; incomplete routing stays nullable.
	for _, routing := range [][2]any{{"team-b", "repo:a"}, {"team-a", "repo:b"},
		{nil, "repo:a"}, {"team-a", nil}, {nil, nil}} {
		mustTaskExec(t, db, `UPDATE projects SET linear_team_id = ?,
			linear_routing_label = ? WHERE id = 'project-b'`, routing[0], routing[1])
	}
	mustTaskExec(t, db, `UPDATE projects SET linear_team_id = 'team-a' WHERE id = 'project-c'`)
	// INSERT, as well as UPDATE, must reject a duplicate complete pair.
	_, err = db.Exec(`INSERT INTO projects (id, name, runner_id, default_role,
		created_at, updated_at, linear_team_id, linear_routing_label)
		SELECT 'duplicate', name, runner_id, default_role,
		created_at, updated_at,
		linear_team_id, linear_routing_label FROM projects WHERE id = 'project-a'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}

func TestLinearMigrationTaskProjectNullability(t *testing.T) {
	db := openTaskTables(t)
	_, err := db.Exec(`UPDATE tasks SET project_id = NULL WHERE id = 'task-001'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	for _, test := range []struct {
		name           string
		source, cause  any
		project        any
		constraintCode int
	}{
		{"manual without project", "manual", nil, nil, sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"manual with cause without project", "manual", "routing_no_label", nil, sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"linear without cause or project", "linear", nil, nil, sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"unknown source", "unknown", nil, "project-a", sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"null source", nil, nil, "project-a", sqlite3.SQLITE_CONSTRAINT_NOTNULL},
		{"unknown cause", "linear", "unknown", nil, sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"missing project", "linear", nil, "missing", sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := insertLinearMigrationTask(db, test.name, test.project, test.source, test.cause)
			assertRegistryConstraint(t, err, test.constraintCode)
		})
	}
	for i, cause := range []string{"routing_multiple_labels", "routing_unknown_label", "routing_team_mismatch",
		"routing_no_label", "missing_acceptance_criteria", "config_missing_assignment"} {
		if _, err := insertLinearMigrationTask(db, fmt.Sprint(i), nil, "linear", cause); err != nil {
			t.Fatalf("valid held task with cause %s: %v", cause, err)
		}
	}
	_, err = db.Exec(`UPDATE tasks SET source = 'manual' WHERE id = '0'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = db.Exec(`UPDATE tasks SET intake_block_cause = NULL WHERE id = '0'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	mustTaskExec(t, db, `UPDATE tasks SET project_id = 'project-a', intake_block_cause = NULL,
		status = 'queued' WHERE id = '0'`)
	_, err = insertLinearMigrationTask(db, "routed", "project-a", "linear", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Existing manual writers omit the new columns and retain their behavior.
	if _, err := insertTask(db, "manual-default", "default", "queued"); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := db.QueryRow(`SELECT source FROM tasks WHERE id = 'manual-default'`).Scan(&source); err != nil || source != "manual" {
		t.Fatalf("default source = %q, err = %v", source, err)
	}
}

func TestLinearMigrationIssueLinksConstraints(t *testing.T) {
	db := openTaskTables(t)
	for _, id := range []string{"task-002", "task-003", "task-004"} {
		if _, err := insertTask(db, id, id, "queued"); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(id, issue, task string, seq int) error {
		_, err := db.Exec(`INSERT INTO linear_issue_links
			(id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
			VALUES (?, ?, 'ENG-123', 'https://example.invalid/issue/ENG-123', 'team-a', ?, ?, ?)`,
			id, issue, task, seq, registryTime)
		return err
	}
	if err := insert("link-1", "issue-a", "task-001", 1); err != nil {
		t.Fatal(err)
	}
	assertRegistryConstraint(t, insert("duplicate-attach", "issue-a", "task-002", 1), sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	assertRegistryConstraint(t, insert("duplicate-task", "issue-b", "task-001", 1), sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	assertRegistryConstraint(t, insert("missing-task", "issue-b", "missing", 1), sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	// The same issue may link to a later task; another issue may use seq=1.
	for _, args := range [][3]string{{"link-2", "issue-a", "task-002"}, {"link-3", "issue-b", "task-003"}} {
		seq := 1
		if args[1] == "issue-a" {
			seq = 2
		}
		if err := insert(args[0], args[1], args[2], seq); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`UPDATE linear_issue_links SET attach_seq = 1 WHERE id = 'link-2'`,
		`UPDATE linear_issue_links SET task_id = 'task-001' WHERE id = 'link-2'`,
	} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	}
	_, err := db.Exec(`DELETE FROM tasks WHERE id = 'task-002'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
}

func TestLinearMigrationPollStateTables(t *testing.T) {
	db := openTaskTables(t)
	mustTaskExec(t, db, `INSERT INTO linear_issue_states
		(linear_issue_id, issue_identifier, team_id, ai_ready, attach_seq, issue_updated_at, first_seen_at, last_seen_at)
		VALUES ('issue-a', 'ENG-123', 'team-a', 1, 0, ?, ?, ?)`, registryTime, registryTime, registryTime)
	mustTaskExec(t, db, `INSERT INTO linear_poll_teams (team_id, consecutive_failures) VALUES ('team-a', 0)`)
	if got := migrationRows(t, db, `SELECT last_decision, last_decision_at FROM linear_issue_states`); !reflect.DeepEqual(got, [][]any{{nil, nil}}) {
		t.Fatalf("initial decision = %#v", got)
	}
	if got := migrationRows(t, db, `SELECT baseline_done_at, last_success_at, last_error, consecutive_failures FROM linear_poll_teams`); !reflect.DeepEqual(got, [][]any{{nil, nil, nil, int64(0)}}) {
		t.Fatalf("initial poll state = %#v", got)
	}
	for _, decision := range []string{"started", "held", "cancelled", "skipped_active_task", "skipped_initial", "skipped_cancelled"} {
		mustTaskExec(t, db, `UPDATE linear_issue_states SET last_decision = ?, last_decision_at = ?`, decision, registryTime)
	}
	for _, class := range []any{"auth", "rate_limited", "network", "other", nil} {
		mustTaskExec(t, db, `UPDATE linear_poll_teams SET last_error = ?`, class)
	}
	mustTaskExec(t, db, `UPDATE linear_issue_states SET ai_ready = 0, attach_seq = 1`)
	for _, statement := range []string{`UPDATE linear_issue_states SET ai_ready = 2`,
		`UPDATE linear_poll_teams SET last_error = 'unknown'`} {
		_, err := db.Exec(statement)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	for _, table := range []string{"linear_issue_states", "linear_poll_teams"} {
		_, err := db.Exec("INSERT INTO " + table + " SELECT * FROM " + table)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
	}
	if got := migrationRows(t, db, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name IN ('event_inbox', 'project_integrations', 'tasks_linear_intake')`); len(got) != 0 {
		t.Fatalf("out-of-scope or temporary tables = %#v", got)
	}
}

func beforeLinearMigration(t *testing.T) fstest.MapFS {
	t.Helper()
	return migrationsBefore(t, 10)
}

// migrationsBefore returns the migrations older than version.
func migrationsBefore(t *testing.T, version int64) fstest.MapFS {
	t.Helper()
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < version {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	return previous
}

func insertLinearMigrationTask(db *sql.DB, id string, project, source, cause any) (sql.Result, error) {
	return db.Exec(`INSERT INTO tasks (`+legacyTaskColumns+`, source, intake_block_cause)
		VALUES (?, ?, 'Title', 'Body', 'tests pass', 'change', 'development', 'solo', 'blocked', ?, ?, ?, ?, ?)`,
		id, project, id, registryTime, registryTime, source, cause)
}

func migrationRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLinearMigrationRollsBackRebuildOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	previous := beforeLinearMigration(t)
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sql, err := migrationFiles.ReadFile("migrations/0010_linear_intake.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Break an existing reference after the rebuild. The pre-commit check must
	// reject it and roll back the schema, the data, and the migration history.
	previous["0010_linear_intake.sql"] = &fstest.MapFile{Data: append(sql, []byte(`
		UPDATE executions SET task_id = 'missing';`)...)}
	db, err = open(context.Background(), path, previous)
	if err == nil || db != nil || !strings.Contains(err.Error(), "foreign key check failed") {
		t.Fatalf("failed rebuild = %v, %v", db, err)
	}
	db, err = open(context.Background(), path, beforeLinearMigration(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := migrationRows(t, db, `SELECT name FROM pragma_table_info('tasks') WHERE name = 'source'`); len(got) != 0 {
		t.Fatalf("failed migration changed task schema: %#v", got)
	}
	if got := migrationRows(t, db, `SELECT task_id FROM executions`); !reflect.DeepEqual(got, [][]any{{"task-001"}}) {
		t.Fatalf("failed migration changed references: %#v", got)
	}
	if got := migrationRows(t, db, `SELECT name FROM pragma_table_info('projects') WHERE name = 'linear_team_id'`); len(got) != 0 {
		t.Fatalf("failed migration changed project schema: %#v", got)
	}
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
}

// TestEmptyAcceptanceCriteriaOnlyForLinearMissingCriteria checks 0011: an
// empty acceptance_criteria is allowed only for source=linear held as
// missing_acceptance_criteria (spec 15 3.8, decided 2026-11-08).
func TestEmptyAcceptanceCriteriaOnlyForLinearMissingCriteria(t *testing.T) {
	db := openTaskTables(t)
	insert := func(id string, project, source, cause any, criteria string) error {
		_, err := db.Exec(`INSERT INTO tasks (`+legacyTaskColumns+`, source, intake_block_cause)
			VALUES (?, ?, 'Title', 'Body', ?, 'change', 'development', 'solo', 'blocked', ?, ?, ?, ?, ?)`,
			id, project, criteria, id, registryTime, registryTime, source, cause)
		return err
	}
	for _, test := range []struct {
		name           string
		project        any
		source, cause  any
		constraintCode int
	}{
		{"manual", "project-a", "manual", nil, sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"manual by default", "project-a", nil, nil, sqlite3.SQLITE_CONSTRAINT_NOTNULL},
		{"manual with missing criteria cause", "project-a", "manual", "missing_acceptance_criteria", sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"linear not held", "project-a", "linear", nil, sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"linear held for routing", nil, "linear", "routing_no_label", sqlite3.SQLITE_CONSTRAINT_CHECK},
		{"linear held for assignment", "project-a", "linear", "config_missing_assignment", sqlite3.SQLITE_CONSTRAINT_CHECK},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRegistryConstraint(t, insert(test.name, test.project, test.source, test.cause, ""), test.constraintCode)
		})
	}
	// Existing manual writers that omit source keep rejecting an empty value.
	_, err := db.Exec(`INSERT INTO tasks (` + legacyTaskColumns + `)
		VALUES ('omitted', 'project-a', 'Title', 'Body', '', 'change', 'development', 'solo', 'queued', 'omitted', 't', 't')`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = db.Exec(`UPDATE tasks SET acceptance_criteria = '' WHERE id = 'task-001'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)

	for _, project := range []any{nil, "project-a"} {
		id := fmt.Sprint("held-", project)
		if err := insert(id, project, "linear", "missing_acceptance_criteria", ""); err != nil {
			t.Fatalf("held linear task without criteria (project %v): %v", project, err)
		}
	}
	var criteria string
	if err := db.QueryRow(`SELECT acceptance_criteria FROM tasks WHERE id = 'held-<nil>'`).Scan(&criteria); err != nil || criteria != "" {
		t.Fatalf("stored criteria = %q, err = %v", criteria, err)
	}
	// Releasing the hold requires criteria; the empty value cannot outlive it.
	_, err = db.Exec(`UPDATE tasks SET intake_block_cause = NULL, status = 'queued' WHERE id = 'held-project-a'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	mustTaskExec(t, db, `UPDATE tasks SET intake_block_cause = NULL, status = 'queued',
		acceptance_criteria = 'tests pass' WHERE id = 'held-project-a'`)
	if got := migrationRows(t, db, `SELECT name FROM sqlite_master WHERE name = 'tasks_acceptance_criteria'`); len(got) != 0 {
		t.Fatalf("temporary table left = %#v", got)
	}
}

// TestEmptyAcceptanceCriteriaMigrationPreservesLinearTasks applies 0011 to a
// database that already holds Linear tasks and links from 0010.
func TestEmptyAcceptanceCriteriaMigrationPreservesLinearTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 11 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	if _, err := insertLinearMigrationTask(db, "task-002", nil, "linear", "routing_unknown_label"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `INSERT INTO linear_issue_links
		(id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
		VALUES ('link-1', 'issue-a', 'ENG-1', 'https://example.invalid/ENG-1', 'team-a', 'task-002', 1, ?)`, registryTime)
	query := "SELECT " + legacyTaskColumns + ", source, intake_block_cause FROM tasks ORDER BY id"
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
		t.Fatalf("migration changed tasks: got %#v, want %#v", got, before)
	}
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
	if violations := migrationRows(t, db, "PRAGMA foreign_key_check"); len(violations) != 0 {
		t.Fatalf("foreign key violations = %#v", violations)
	}
	for _, id := range []string{"task-001", "task-002"} {
		_, err := db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
}

// 受け入れ基準: removed_at の列を足すマイグレーションで既存の行が NULL になる
// (spec 15 3.8, Q78). 0012 is applied to a database that already holds
// linear_issue_states rows from 0010.
func TestLinearIssueRemovedAtMigrationLeavesExistingRowsNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 12 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id      string
		aiReady int
		seq     int
	}{{"issue-a", 1, 2}, {"issue-b", 0, 3}} {
		mustTaskExec(t, db, `INSERT INTO linear_issue_states (linear_issue_id, issue_identifier, team_id,
			ai_ready, attach_seq, issue_updated_at, last_decision, last_decision_at, first_seen_at, last_seen_at)
			VALUES (?, 'ENG-1', 'team-a', ?, ?, ?, 'started', ?, ?, ?)`,
			row.id, row.aiReady, row.seq, registryTime, registryTime, registryTime, registryTime)
	}
	query := `SELECT linear_issue_id, issue_identifier, team_id, ai_ready, attach_seq, issue_updated_at,
		last_decision, last_decision_at, first_seen_at, last_seen_at FROM linear_issue_states ORDER BY 1`
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
	if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
		t.Fatalf("migration changed rows: got %#v, want %#v", got, before)
	}
	if got := migrationRows(t, db, `SELECT removed_at FROM linear_issue_states ORDER BY 1`); !reflect.DeepEqual(got, [][]any{{nil}, {nil}}) {
		t.Fatalf("removed_at of existing rows = %#v, want NULL", got)
	}
	mustTaskExec(t, db, `UPDATE linear_issue_states SET removed_at = ? WHERE linear_issue_id = 'issue-a'`, registryTime)
	mustTaskExec(t, db, `UPDATE linear_issue_states SET removed_at = NULL WHERE linear_issue_id = 'issue-a'`)
	mustTaskExec(t, db, `UPDATE linear_issue_states SET last_decision = 'skipped_restored'`)
}

// 受け入れ基準: recheck_requested_at の列を足すマイグレーションで既存の行が
// NULL になる (spec 15 3.6.3, 3.8, Q81). 0016 is applied to a database that
// already holds linear_issue_states rows.
func TestLinearIssueRecheckRequestedAtMigrationLeavesExistingRowsNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 16 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id      string
		removed any
	}{{"issue-a", nil}, {"issue-b", registryTime}} {
		mustTaskExec(t, db, `INSERT INTO linear_issue_states (linear_issue_id, issue_identifier, team_id,
			ai_ready, attach_seq, issue_updated_at, last_decision, last_decision_at, first_seen_at, last_seen_at,
			removed_at) VALUES (?, 'ENG-1', 'team-a', 1, 1, ?, 'held', ?, ?, ?, ?)`,
			row.id, registryTime, registryTime, registryTime, registryTime, row.removed)
	}
	query := `SELECT linear_issue_id, issue_identifier, team_id, ai_ready, attach_seq, issue_updated_at,
		last_decision, last_decision_at, first_seen_at, last_seen_at, removed_at FROM linear_issue_states ORDER BY 1`
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
	if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
		t.Fatalf("migration changed rows: got %#v, want %#v", got, before)
	}
	if got := migrationRows(t, db, `SELECT recheck_requested_at FROM linear_issue_states ORDER BY 1`); !reflect.DeepEqual(got, [][]any{{nil}, {nil}}) {
		t.Fatalf("recheck_requested_at of existing rows = %#v, want NULL", got)
	}
}

// 受け入れ基準: outcome_unknown の列を足すマイグレーションで、pending かつ
// attempts が1以上の行が 1、それ以外の行が 0 になる (spec 16 3.4.1, 3.7, Q83).
// 0017 is applied to a database that already holds linear_outbox rows.
func TestLinearOutboxOutcomeUnknownMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 17 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	for _, row := range []struct {
		id, status string
		attempts   int
	}{
		{"o1-pending-0", "pending", 0},
		{"o2-pending-1", "pending", 1},
		{"o3-pending-4", "pending", 4},
		{"o4-sent-0", "sent", 0},
		{"o5-sent-2", "sent", 2},
		{"o6-failed-5", "failed", 5},
	} {
		mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body,
			status, attempts, created_at) VALUES (?, 'task-001', 'issue-a', 'succeeded', ?, 'body', ?, ?, ?)`,
			row.id, "key-"+row.id, row.status, row.attempts, registryTime)
	}
	query := `SELECT id, task_id, linear_issue_id, kind, dedupe_key, body, status, attempts, next_attempt_at,
		linear_comment_id, created_at, sent_at FROM linear_outbox ORDER BY id`
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
	if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
		t.Fatalf("migration changed rows: got %#v, want %#v", got, before)
	}
	want := [][]any{{"o1-pending-0", int64(0)}, {"o2-pending-1", int64(1)}, {"o3-pending-4", int64(1)},
		{"o4-sent-0", int64(0)}, {"o5-sent-2", int64(0)}, {"o6-failed-5", int64(0)}}
	if got := migrationRows(t, db, `SELECT id, outcome_unknown FROM linear_outbox ORDER BY id`); !reflect.DeepEqual(got, want) {
		t.Fatalf("outcome_unknown after migration = %#v, want %#v", got, want)
	}
	// New rows default to 0 and only 0 / 1 are accepted.
	mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body,
		status, created_at) VALUES ('o7', 'task-001', 'issue-a', 'failed', 'key-o7', 'body', 'pending', ?)`, registryTime)
	if got := migrationRows(t, db, `SELECT outcome_unknown FROM linear_outbox WHERE id = 'o7'`); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
		t.Fatalf("default outcome_unknown = %#v, want 0", got)
	}
	_, err = db.Exec(`UPDATE linear_outbox SET outcome_unknown = 2 WHERE id = 'o7'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
}

// 受け入れ基準 (spec 16 3.4.2, 3.7, Q82): execution_id・failure_reason・
// failed_at を足すマイグレーションで、既存の行の execution_id が dedupe_key
// の対象の Execution で埋まり、Execution の無い行は NULL、すでに failed の行
// は failure_reason が other で failed_at が created_at になる。
// 0018 is applied to a database that already holds linear_outbox rows.
func TestLinearOutboxFailureRecordMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 18 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db) // task-001 with execution-1
	if _, err := insertTask(db, "task-002", "key-2", "queued"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertExecution(db, "execution-2", "task-002", 1, "pending"); err != nil {
		t.Fatal(err)
	}
	const created1, created2 = "2026-11-01T01:02:03.123456789Z", "2026-11-02T00:00:00Z"
	for _, row := range []struct {
		id, kind, dedupeKey, status, created string
	}{
		{"o1-exec-pending", "succeeded", "task-001:succeeded:execution-1", "pending", registryTime},
		{"o2-exec-failed", "failed", "task-001:failed:execution-1", "failed", created1},
		{"o3-hold-failed", "intake_blocked", "task-001:intake_blocked:routing_no_label", "failed", created2},
		{"o4-cancel-sent", "cancelled", "task-001:cancelled:task-001", "sent", registryTime},
		{"o5-other-task", "succeeded", "task-001:succeeded:execution-2", "pending", registryTime},
		{"o6-missing", "blocked", "task-001:blocked:execution-x", "pending", registryTime},
		{"o7-criteria", "missing_acceptance_criteria", "task-001:missing_acceptance_criteria:missing_acceptance_criteria", "pending", registryTime},
	} {
		mustTaskExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body,
			status, attempts, created_at) VALUES (?, 'task-001', 'issue-a', ?, ?, 'body', ?, 1, ?)`,
			row.id, row.kind, row.dedupeKey, row.status, row.created)
	}
	query := `SELECT id, task_id, linear_issue_id, kind, dedupe_key, body, status, attempts, next_attempt_at,
		linear_comment_id, created_at, sent_at, outcome_unknown FROM linear_outbox ORDER BY id`
	before := migrationRows(t, db, query)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25})
	if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
		t.Fatalf("migration changed rows: got %#v, want %#v", got, before)
	}
	want := [][]any{
		{"o1-exec-pending", "execution-1", nil, nil},
		{"o2-exec-failed", "execution-1", "other", created1},
		{"o3-hold-failed", nil, "other", created2},
		{"o4-cancel-sent", nil, nil, nil},
		{"o5-other-task", nil, nil, nil},
		{"o6-missing", nil, nil, nil},
		{"o7-criteria", nil, nil, nil},
	}
	if got := migrationRows(t, db, `SELECT id, execution_id, failure_reason, failed_at FROM linear_outbox ORDER BY id`); !reflect.DeepEqual(got, want) {
		t.Fatalf("after migration = %#v, want %#v", got, want)
	}
	// Only the Q85 reasons are accepted, and execution_id references an Execution.
	_, err = db.Exec(`UPDATE linear_outbox SET failure_reason = 'permission_denied' WHERE id = 'o3-hold-failed'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	for _, reason := range []string{"auth", "issue_removed", "retries_exhausted", "other"} {
		mustTaskExec(t, db, `UPDATE linear_outbox SET failure_reason = ? WHERE id = 'o3-hold-failed'`, reason)
	}
	_, err = db.Exec(`UPDATE linear_outbox SET execution_id = 'execution-x' WHERE id = 'o6-missing'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
}
