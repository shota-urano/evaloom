package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func insertPrefixRoute(t *testing.T, db *sql.DB, label string) {
	t.Helper()
	mustTaskExec(t, db, `INSERT INTO harnesses (id,name,repo_url,repo_path,created_at,updated_at)
		VALUES (?,?,'url','path','t','t')`, label, label)
	mustTaskExec(t, db, `INSERT INTO routes VALUES (?,NULL,?,'t','t')`, label, label)
}

func TestLinearOutboxLabelRequiresLiteralRoutingPrefix(t *testing.T) {
	for _, prefix := range []string{`"route:"`, `"custom_%:"`, `"振分:"`, `null`, `"\u3000"`, `12`, `{}`, `invalid`} {
		t.Run(prefix, func(t *testing.T) {
			db := openTaskTables(t)
			mustTaskExec(t, db, `INSERT INTO settings VALUES ('linear.routingLabelPrefix',?,'t')
				ON CONFLICT(key) DO UPDATE SET value=excluded.value`, prefix)
			allowed := "route:allowed"
			if prefix == `"custom_%:"` {
				allowed = "custom_%:allowed"
			}
			if prefix == `"振分:"` {
				allowed = "振分:allowed"
			}
			for _, label := range []string{allowed, "other:label", "ROUTE:allowed", "customAA:allowed"} {
				insertPrefixRoute(t, db, label)
			}
			if err := insertLabelFeedback(db, "allowed", allowed); err != nil {
				t.Fatal(err)
			}
			for _, label := range []string{"other:label", "ROUTE:allowed", "customAA:allowed"} {
				assertRegistryConstraint(t, insertLabelFeedback(db, "bad", label), sqlite3.SQLITE_CONSTRAINT_TRIGGER)
				_, err := db.Exec(`UPDATE linear_outbox SET label_name=? WHERE id='allowed'`, label)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_TRIGGER)
			}
			mustTaskExec(t, db, `INSERT INTO linear_outbox (id,task_id,linear_issue_id,kind,dedupe_key,body,status,created_at)
				VALUES ('comment','task-001','issue','decision_open','comment','body','pending','t')`)
			_, err := db.Exec(`UPDATE linear_outbox SET action='add_label',kind='route_approved',body=NULL,
				label_name='other:label' WHERE id='comment'`)
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_TRIGGER)
			// Changing settings invalidates pending delivery, while outcome
			// recording for old rows must continue to work.
			mustTaskExec(t, db, `UPDATE settings SET value='"changed:"' WHERE key='linear.routingLabelPrefix'`)
			mustTaskExec(t, db, `UPDATE linear_outbox SET status='failed',failure_reason='other',failed_at='t' WHERE id='allowed'`)
		})
	}
}

func TestLinearLabelPrefixMigrationPreservesPendingHistoryOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.db")
	db := openBefore(t, path, 33)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO routes VALUES ('other:label','project-a',NULL,'t','t')`)
	if err := insertLabelFeedback(db, "old", "other:label"); err != nil {
		t.Fatal(err)
	}
	query := `SELECT * FROM linear_outbox WHERE id='old'`
	before := migrationRows(t, db, query)
	db.Close()
	for range 2 {
		db = openRegistry(t, path)
		if got := migrationRows(t, db, query); !reflect.DeepEqual(got, before) {
			t.Fatalf("pending history changed: got=%v want=%v", got, before)
		}
		assertRegistryConstraint(t, insertLabelFeedback(db, "new", "other:label"), sqlite3.SQLITE_CONSTRAINT_TRIGGER)
		db.Close()
	}
}

func TestLinearOutboxRejectsTriggerEvenWithinRoutingPrefix(t *testing.T) {
	for _, trigger := range []string{"ai-ready", "ai-custom"} {
		t.Run(trigger, func(t *testing.T) {
			db := openTaskTables(t)
			mustTaskExec(t, db, `INSERT INTO settings VALUES ('linear.routingLabelPrefix','"ai-"','t')`)
			if trigger != "ai-ready" {
				mustTaskExec(t, db, `INSERT INTO settings VALUES ('linear.triggerLabel','"ai-custom"','t')`)
			}
			insertPrefixRoute(t, db, trigger)
			insertPrefixRoute(t, db, "ai-allowed")
			if err := insertLabelFeedback(db, "allowed", "ai-allowed"); err != nil {
				t.Fatal(err)
			}
			assertRegistryConstraint(t, insertLabelFeedback(db, "trigger", trigger), sqlite3.SQLITE_CONSTRAINT_TRIGGER)
			_, err := db.Exec(`UPDATE linear_outbox SET label_name=? WHERE id='allowed'`, trigger)
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_TRIGGER)
		})
	}
}
