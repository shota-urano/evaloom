package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestTaskExecutionColumnsMatchSpecification(t *testing.T) {
	db := openTaskTables(t)
	for table, want := range map[string]string{
		"tasks":             "id,source,project_id,harness_id,title,body,acceptance_criteria,collaboration_mode,status,intake_block_cause,idempotency_key,created_at,updated_at",
		"executions":        "id,task_id,seq,mode,status,resume_stage,runner_id,route_kind,project_id,harness_id,profile_id,profile_version,profile_commit_sha,contract_version,params,workspace_id,work_session_id,content_id,outputs,error_class,error_message,cancel_requested_at,claimed_at,started_at,finished_at,last_heartbeat_at,created_at",
		"task_workspaces":   "id,task_id,runner_id,path,status,created_by_execution_id,created_at,cleaned_at",
		"stage_runs":        "id,execution_id,stage,attempt,status,exit_code,error_class,request_artifact_id,result_artifact_id,started_at,finished_at",
		"check_results":     "id,execution_id,stage_run_id,result,content_id,summary,findings,created_at",
		"execution_events":  "id,execution_id,type,payload,created_at",
		"context_revisions": "id,execution_id,layer,kind,source_path,source_revision,content_hash,content,created_at",
	} {
		var got string
		if err := db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_table_info(?) ORDER BY cid)`, table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s columns=%s want=%s err=%v", table, got, want, err)
		}
	}
}

func TestTaskDestinationConstraint(t *testing.T) {
	db := openTaskTables(t)
	_, err := db.Exec(`UPDATE tasks SET harness_id='codex-developer'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	_, err = db.Exec(`INSERT INTO tasks (id,source,project_id,harness_id,title,body,acceptance_criteria,collaboration_mode,status,idempotency_key,created_at,updated_at)
		VALUES ('task-002','manual','project-a','codex-developer','t','b','','solo','received','key-2','t','t')`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
	mustTaskExec(t, db, `UPDATE tasks SET project_id=NULL,harness_id='codex-developer'`)
	_, err = db.Exec(`UPDATE tasks SET harness_id='missing'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	mustTaskExec(t, db, `UPDATE tasks SET harness_id=NULL,status='routing'`)
}

func TestStageWorkspaceAndCheckConstraints(t *testing.T) {
	db := openTaskTables(t)
	mustTaskExec(t, db, `INSERT INTO task_workspaces VALUES ('workspace-1','task-001','mac-mini','/workspaces/task-001','active','execution-1','t',NULL)`)
	mustTaskExec(t, db, `UPDATE executions SET workspace_id='workspace-1'`)
	mustTaskExec(t, db, `INSERT INTO stage_runs (id,execution_id,stage,attempt,status,started_at) VALUES ('stage-1','execution-1','check',1,'running','t')`)
	mustTaskExec(t, db, `INSERT INTO check_results VALUES ('check-1','execution-1','stage-1','pass','opaque',NULL,'[]','t')`)
	for _, column := range []string{"request_artifact_id", "result_artifact_id"} {
		_, err := db.Exec("UPDATE stage_runs SET " + column + "='missing'")
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
	for _, tc := range []struct{ table, column string }{
		{"task_workspaces", "task_id"}, {"task_workspaces", "runner_id"}, {"task_workspaces", "created_by_execution_id"},
		{"stage_runs", "execution_id"}, {"check_results", "execution_id"}, {"check_results", "stage_run_id"}, {"executions", "workspace_id"},
	} {
		_, err := db.Exec("UPDATE " + tc.table + " SET " + tc.column + "='missing'")
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
	for _, tc := range []struct {
		table, column string
		allowed       []string
	}{
		{"task_workspaces", "status", []string{"active", "retained", "cleaned"}},
		{"stage_runs", "stage", []string{"prepare", "work", "check", "publish"}},
		{"stage_runs", "status", []string{"running", "succeeded", "failed", "blocked", "needs_approval", "unknown", "question", "cancelled", "timed_out"}},
		{"stage_runs", "error_class", []string{"config_error", "prepare_failed", "work_failed", "check_failed", "publish_failed", "publish_unknown", "approval_rejected", "contract_error", "sandbox_error", "timeout", "cancelled", "interrupted"}},
		{"check_results", "result", []string{"pass", "fail", "blocked"}},
		{"executions", "resume_stage", []string{"work", "publish"}},
		{"executions", "route_kind", []string{"project", "harness"}},
		{"tasks", "intake_block_cause", []string{"missing_acceptance_criteria", "config_missing_profile"}},
	} {
		t.Run(tc.table+"/"+tc.column, func(t *testing.T) {
			for _, value := range tc.allowed {
				mustTaskExec(t, db, "UPDATE "+tc.table+" SET "+tc.column+"=?", value)
			}
			_, err := db.Exec("UPDATE "+tc.table+" SET "+tc.column+"=?", "obsolete")
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
		})
	}
	_, err := db.Exec(`INSERT INTO stage_runs (id,execution_id,stage,attempt,status,started_at) SELECT 'duplicate',execution_id,stage,attempt,status,started_at FROM stage_runs`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	mustTaskExec(t, db, `INSERT INTO stage_runs (id,execution_id,stage,attempt,status,started_at) SELECT 'retry',execution_id,stage,attempt+1,status,started_at FROM stage_runs`)
	_, err = db.Exec(`UPDATE stage_runs SET attempt=1 WHERE id='retry'`)
	assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestTaskExecutionMigrationPreservesDataAndSSESequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	db := openBefore(t, path, 27)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	seedVerificationRecords(t, db)
	mustTaskExec(t, db, `UPDATE tasks SET status='verifying'`)
	mustTaskExec(t, db, `UPDATE executions SET status='verifying',runner_id='mac-mini',harness_id='codex-developer',profile_id='profile-1',profile_version='1.2.0',profile_commit_sha='sha',harness_session_id='session',error_class='test_failed'`)
	mustTaskExec(t, db, `INSERT INTO context_revisions VALUES ('context-1','execution-1','harness','record','record.json','revision','hash','contents','t')`)
	mustTaskExec(t, db, `INSERT INTO execution_events VALUES (50,'execution-1','context_pinned','{}','t')`)
	mustTaskExec(t, db, `INSERT INTO execution_events VALUES (100,'execution-1','log','{}','t')`)
	mustTaskExec(t, db, `DELETE FROM execution_events WHERE id=100`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db = openRegistry(t, path)
		var taskStatus, executionStatus, session, classification, route, project, profile string
		if err := db.QueryRow(`SELECT t.status,e.status,e.work_session_id,e.error_class,e.route_kind,e.project_id,e.profile_id FROM executions e JOIN tasks t ON t.id=e.task_id`).Scan(&taskStatus, &executionStatus, &session, &classification, &route, &project, &profile); err != nil {
			t.Fatal(err)
		}
		if taskStatus != "running" || executionStatus != "checking" || session != "session" || classification != "check_failed" || route != "project" || project != "project-a" || profile != "profile-1" {
			t.Fatal(taskStatus, executionStatus, session, classification, route, project, profile)
		}
		var revision, content string
		if err := db.QueryRow(`SELECT source_revision,content FROM context_revisions`).Scan(&revision, &content); err != nil || revision != "revision" || content != "contents" {
			t.Fatal(revision, content, err)
		}
		result := mustTaskExec(t, db, `INSERT INTO execution_events (execution_id,type,payload,created_at) VALUES ('execution-1','stage_started','{}','t')`)
		if id, err := result.LastInsertId(); err != nil || id <= 100 {
			t.Fatal(id, err)
		}
		if got := migrationRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
			t.Fatal(got)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTaskExecutionMigrationRejectsAmbiguousLegacyRowsAtomically(t *testing.T) {
	for _, query := range []string{
		`UPDATE tasks SET status='planning'`,
		`UPDATE tasks SET source='linear',intake_block_cause='routing_no_label'`,
		`UPDATE executions SET status='merging'`,
		`INSERT INTO execution_events (execution_id,type,payload,created_at) VALUES ('execution-1','pushed','{}','t')`,
		`INSERT INTO context_revisions VALUES ('context-1','execution-1','project','agents_md','AGENTS.md','sha','hash','contents','t')`,
	} {
		t.Run(query, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.db")
			db := openBefore(t, path, 27)
			seedRegistry(t, db)
			seedTaskTables(t, db)
			mustTaskExec(t, db, query)
			before := migrationRows(t, db, `SELECT * FROM tasks`)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			failed, err := Open(context.Background(), path)
			if failed != nil {
				failed.Close()
				t.Fatal("migration returned a DB after failure")
			}
			if err == nil || !strings.Contains(err.Error(), "0027_task_executions.sql") {
				t.Fatal(err)
			}
			db = openBefore(t, path, 27)
			if after := migrationRows(t, db, `SELECT * FROM tasks`); !reflect.DeepEqual(before, after) {
				t.Fatal("task data changed")
			}
			if tableExists(t, db, "stage_runs") || tableExists(t, db, "task_workspaces") {
				t.Fatal("partial schema survived")
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version=27`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestTaskExecutionMigrationPreservesSSESequenceWithNoLiveEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	db := openBefore(t, path, 27)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO execution_events VALUES (100,'execution-1','log','{}','t')`)
	mustTaskExec(t, db, `DELETE FROM execution_events`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	result := mustTaskExec(t, db, `INSERT INTO execution_events (execution_id,type,payload,created_at) VALUES ('execution-1','log','{}','t')`)
	if id, err := result.LastInsertId(); err != nil || id <= 100 {
		t.Fatal(id, err)
	}
}
