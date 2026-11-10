package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRegistryMigrationPreservesPinsRoutesAndExecutionReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	db, err := open(context.Background(), path, migrationsBefore(t, 26))
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `UPDATE runners SET boot_id='boot'; UPDATE projects SET linear_team_id='team',linear_routing_label='route:project'`)
	mustTaskExec(t, db, `UPDATE executions SET runner_id='mac-mini',harness_id='codex-developer',profile_id='profile-1'`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db = openRegistry(t, path)
		var harness, profile, params, waivers, label, boot string
		if err := db.QueryRow(`SELECT p.harness_id,p.profile_id,p.params,p.approval_waivers,r.label,ru.boot_id
  FROM projects p JOIN routes r ON r.project_id=p.id JOIN runners ru ON ru.id=p.runner_id`).Scan(&harness, &profile, &params, &waivers, &label, &boot); err != nil {
			t.Fatal(err)
		}
		if harness != "codex-developer" || profile != "profile-1" || params != "{}" || waivers != "[]" || label != "route:project" || boot != "boot" {
			t.Fatal(harness, profile, params, waivers, label, boot)
		}
		var team, prefix string
		if err := db.QueryRow(`SELECT value FROM settings WHERE key='linear.teamId'`).Scan(&team); err != nil || team != `"team"` {
			t.Fatal(team, err)
		}
		if err := db.QueryRow(`SELECT value FROM settings WHERE key='linear.routingLabelPrefix'`).Scan(&prefix); err != nil || prefix != `"repo:"` {
			t.Fatal(prefix, err)
		}
		var contract string
		if err := db.QueryRow(`SELECT contract_version FROM harness_profiles WHERE id='profile-1'`).Scan(&contract); err != nil || contract != "1" {
			t.Fatal(contract, err)
		}
		if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
			t.Fatal(got)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegistryMigrationRejectsUnmappableLegacyDataAtomically(t *testing.T) {
	for _, cause := range []string{"missing assignment", "duplicate label", "multiple teams"} {
		t.Run(cause, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.db")
			db, err := open(context.Background(), path, migrationsBefore(t, 26))
			if err != nil {
				t.Fatal(err)
			}
			seedRegistry(t, db)
			if cause == "missing assignment" {
				mustTaskExec(t, db, `DELETE FROM project_harnesses`)
			} else {
				insertRegistryProject(t, db, "project-b", "/repos/b")
				mustTaskExec(t, db, `INSERT INTO project_harnesses VALUES ('project-b','developer','codex-developer','profile-1','t')`)
				if cause == "duplicate label" {
					mustTaskExec(t, db, `UPDATE projects SET linear_team_id=id,linear_routing_label='route:duplicate'`)
				} else {
					mustTaskExec(t, db, `UPDATE projects SET linear_team_id=id`)
				}
			}
			before := migrationRows(t, db, `SELECT * FROM projects ORDER BY id`)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			failed, err := Open(context.Background(), path)
			if failed != nil {
				failed.Close()
				t.Fatal("failed migration returned an open DB")
			}
			if err == nil || !strings.Contains(err.Error(), "0026_registry.sql") {
				t.Fatalf("expected migration failure: %v", err)
			}
			db, err = open(context.Background(), path, migrationsBefore(t, 26))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			after := migrationRows(t, db, `SELECT * FROM projects ORDER BY id`)
			if !reflect.DeepEqual(after, before) {
				t.Fatal("migration lost projects")
			}
			if tableExists(t, db, "routes") || !tableExists(t, db, "project_harnesses") {
				t.Fatal("partial migration survived")
			}
			if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
				t.Fatal(got)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version=26`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}
