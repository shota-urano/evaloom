package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestLinearSettingsColumnsMatchSpecification(t *testing.T) {
	db := openTaskTables(t)
	for table, want := range map[string]string{
		"linear_poll_teams":   "team_id,baseline_done_at,last_success_at,last_error,consecutive_failures",
		"linear_issue_states": "linear_issue_id,issue_identifier,team_id,ai_ready,attach_seq,issue_updated_at,last_decision,last_decision_at,first_seen_at,last_seen_at,removed_at,recheck_requested_at",
		"linear_issue_links":  "id,linear_issue_id,issue_identifier,issue_url,team_id,task_id,attach_seq,created_at",
		"linear_outbox":       "id,task_id,linear_issue_id,kind,action,state_name,dedupe_key,body,status,attempts,next_attempt_at,linear_comment_id,created_at,sent_at,outcome_unknown,execution_id,failure_reason,failed_at,label_name",
		"settings":            "key,value,updated_at",
	} {
		var got string
		if err := db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_table_info(?) ORDER BY cid)`, table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s columns=%s want=%s err=%v", table, got, want, err)
		}
	}
	// The existing unconstrained decision column already supports routing.
	mustTaskExec(t, db, `INSERT INTO linear_issue_states (linear_issue_id,issue_identifier,team_id,
		ai_ready,attach_seq,issue_updated_at,last_decision,first_seen_at,last_seen_at)
		VALUES ('issue','ENG-1','team',1,1,'t','routing','t','t')`)
}

func TestSettingsRejectsUnknownKeysOnInsertAndUpdate(t *testing.T) {
	db := openTaskTables(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, key := range []string{"linear.teamId", "linear.triggerLabel", "linear.routingLabelPrefix",
		"routing.proposer", "routing.defaultRunnerId", "limits.stageTimeoutMaxSec", "approval.ttlSec"} {
		if err := PutSetting(ctx, tx, key, `null`); err != nil {
			t.Fatalf("allowed key %s: %v", key, err)
		}
	}
	for _, key := range []string{"unknown", "linear.apiKey", "linear.teamIds", "Linear.teamId", ""} {
		assertRegistryConstraint(t, PutSetting(ctx, tx, key, `"value"`), sqlite3.SQLITE_CONSTRAINT_CHECK)
		_, err := tx.Exec(`UPDATE settings SET key=? WHERE key='linear.teamId'`, key)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	_, err = tx.Exec(`INSERT INTO settings VALUES (NULL,'null','t')`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_NOTNULL)
	if err := PutSetting(ctx, tx, "linear.triggerLabel", `invalid`); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	if rows, err := ReadSettings(ctx, tx); err != nil || len(rows) != 7 || rows["linear.triggerLabel"] != "null" {
		t.Fatalf("settings after rejected writes = %v, err=%v", rows, err)
	}
}

func insertLabelFeedback(db *sql.DB, id string, label any) error {
	_, err := db.Exec(`INSERT INTO linear_outbox (id,task_id,linear_issue_id,action,kind,label_name,
		dedupe_key,status,created_at) VALUES (?,'task-001','issue','add_label','route_approved',?,?,'pending','t')`,
		id, label, "task-001:label:"+id)
	return err
}

func TestLinearOutboxLabelMustBeRouteAndNotTrigger(t *testing.T) {
	db := openTaskTables(t)
	// Register even the trigger as a route, proving its independent exclusion.
	for _, label := range []string{"route:allowed", "ai-ready", "custom-trigger"} {
		mustTaskExec(t, db, `INSERT INTO harnesses (id,name,repo_url,repo_path,created_at,updated_at)
			VALUES (?,?,'url','path','t','t')`, label, label)
		mustTaskExec(t, db, `INSERT INTO routes VALUES (?,NULL,?,'t','t')`, label, label)
	}
	if err := insertLabelFeedback(db, "allowed", "route:allowed"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"route:missing", "ROUTE:allowed", "route:allowed ", "ai-ready"} {
		assertRegistryConstraint(t, insertLabelFeedback(db, "bad", label), sqlite3.SQLITE_CONSTRAINT_TRIGGER)
		_, err := db.Exec(`UPDATE linear_outbox SET label_name=? WHERE id='allowed'`, label)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_TRIGGER)
	}
	mustTaskExec(t, db, `INSERT INTO settings VALUES ('linear.triggerLabel','"custom-trigger"','t')`)
	assertRegistryConstraint(t, insertLabelFeedback(db, "custom", "custom-trigger"), sqlite3.SQLITE_CONSTRAINT_TRIGGER)
	_, err := db.Exec(`UPDATE linear_outbox SET label_name='custom-trigger' WHERE id='allowed'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_TRIGGER)
	// The configured value replaces the default, rather than banning both.
	if err := insertLabelFeedback(db, "old-trigger", "ai-ready"); err != nil {
		t.Fatal(err)
	}
	// Trigger validation also applies when converting another action to add_label.
	mustTaskExec(t, db, `INSERT INTO linear_outbox (id,task_id,linear_issue_id,kind,dedupe_key,body,status,created_at)
		VALUES ('comment','task-001','issue','decision_open','comment','body','pending','t')`)
	_, err = db.Exec(`UPDATE linear_outbox SET action='add_label',kind='route_approved',body=NULL,
		label_name='custom-trigger' WHERE id='comment'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_TRIGGER)
	// Route edits must not prevent recording delivery failures for earlier rows.
	mustTaskExec(t, db, `DELETE FROM routes WHERE label='route:allowed'`)
	mustTaskExec(t, db, `UPDATE linear_outbox SET status='failed',failure_reason='label_not_found',failed_at='t' WHERE id='allowed'`)
}

func TestLinearOutboxLabelPayloadConstraints(t *testing.T) {
	db := openTaskTables(t)
	mustTaskExec(t, db, `INSERT INTO routes VALUES ('route:allowed','project-a',NULL,'t','t')`)
	if err := insertLabelFeedback(db, "label", "route:allowed"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"outcome_unknown=1", "body='body'", "state_name='Done'",
		"linear_comment_id='comment'", "kind='started'", "label_name=NULL", "action='comment'"} {
		_, err := db.Exec(`UPDATE linear_outbox SET ` + change + ` WHERE id='label'`)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	for _, action := range []string{"comment", "set_state"} {
		_, err := db.Exec(`INSERT INTO linear_outbox (id,task_id,linear_issue_id,action,kind,label_name,
			state_name,body,dedupe_key,status,created_at) VALUES (?,'task-001','issue',?,'cancelled','route:allowed',
			CASE WHEN ?='set_state' THEN 'Canceled' END,CASE WHEN ?='comment' THEN 'body' END,?,'pending','t')`,
			action, action, action, action, action)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}
	_, err := db.Exec(`UPDATE linear_outbox SET task_id='missing' WHERE id='label'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`UPDATE linear_outbox SET execution_id='missing' WHERE id='label'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	_, err = db.Exec(`INSERT INTO linear_outbox (id,task_id,linear_issue_id,action,kind,label_name,dedupe_key,status,created_at)
		SELECT 'duplicate',task_id,linear_issue_id,action,kind,label_name,dedupe_key,status,created_at FROM linear_outbox`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}

func TestLinearSettingsMigrationPreservesHistoryAndRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linear.db")
	db := openBefore(t, path, 30)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO linear_outbox (id,task_id,linear_issue_id,kind,dedupe_key,body,
		status,attempts,next_attempt_at,linear_comment_id,created_at,sent_at,outcome_unknown,
		execution_id,failure_reason,failed_at)
		VALUES ('old','task-001','issue','succeeded','key','body','failed',3,'next','comment','created',
		'sent',1,'execution-1','auth','failed')`)
	mustTaskExec(t, db, `INSERT INTO settings VALUES ('linear.triggerLabel','"custom"','updated')`)
	mustTaskExec(t, db, `INSERT INTO linear_poll_teams VALUES ('team','base','success','network',2)`)
	mustTaskExec(t, db, `INSERT INTO linear_issue_states VALUES ('issue','ENG-1','team',1,2,'updated',
		'held','decision','first','last','removed','recheck')`)
	mustTaskExec(t, db, `INSERT INTO linear_issue_links VALUES ('link','issue','ENG-1','url','team','task-001',2,'created')`)
	queries := []string{
		`SELECT id,task_id,linear_issue_id,kind,action,state_name,dedupe_key,body,status,attempts,
		 next_attempt_at,linear_comment_id,created_at,sent_at,outcome_unknown,execution_id,failure_reason,failed_at FROM linear_outbox`,
		`SELECT * FROM settings`, `SELECT * FROM linear_poll_teams`,
		`SELECT * FROM linear_issue_states`, `SELECT * FROM linear_issue_links`,
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = migrationRows(t, db, query)
	}
	db.Close()
	for range 2 {
		db = openRegistry(t, path)
		for i, query := range queries {
			if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before[i]) {
				t.Fatalf("changed %s: got=%v want=%v", query, got, before[i])
			}
		}
		if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
			t.Fatalf("foreign key violations: %v", got)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinearSettingsMigrationRejectsUnknownLegacyKeyAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.db")
	db := openBefore(t, path, 30)
	mustTaskExec(t, db, `INSERT INTO settings VALUES ('unknown','"preserve"','t')`)
	db.Close()
	db, err := Open(context.Background(), path)
	if err == nil || db != nil || !strings.Contains(err.Error(), "0030_linear_settings.sql") {
		t.Fatalf("Open=%v err=%v; want migration failure", db, err)
	}
	db = openBefore(t, path, 30)
	defer db.Close()
	if got := migrationRows(t, db, `SELECT * FROM settings`); !reflect.DeepEqual(got, [][]any{{"unknown", `"preserve"`, "t"}}) {
		t.Fatalf("legacy settings changed: %v", got)
	}
	if got := migrationRows(t, db, `SELECT name FROM pragma_table_info('linear_outbox') WHERE name='label_name'`); len(got) != 0 {
		t.Fatalf("partial outbox rebuild: %v", got)
	}
	if got := migrationRows(t, db, `SELECT version FROM schema_migrations WHERE version=30`); len(got) != 0 {
		t.Fatalf("failed migration recorded: %v", got)
	}
}
