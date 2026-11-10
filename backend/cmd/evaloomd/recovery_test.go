package main

import (
	"context"
	"fmt"
	"github.com/shota-urano/evaloom/backend/internal/testregistry"
	"path/filepath"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func TestStartupRecoveryAllowsReconnectionAndRecoversSilentRunner(t *testing.T) {
	ctx := context.Background()
	db, err := testregistry.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := time.Now().UTC()
	for i, runner := range []string{"silent", "resumed", "never-heartbeat"} {
		if err := store.RegisterRunner(ctx, db, runner, runner, "unused-"+runner, "2020-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		if runner != "never-heartbeat" {
			if _, err := db.Exec(`UPDATE runners SET boot_id = 'same-boot', last_heartbeat_at = '2020-01-01T00:00:00Z', status = 'online' WHERE id = ?`, runner); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO projects (id, name, runner_id, created_at, updated_at, harness_id, profile_id) VALUES (?, ?, ?, '', '', 'fixture-harness', 'fixture-profile')`, runner, runner, runner); err != nil {
			t.Fatal(err)
		}
		taskID := fmt.Sprintf("task-%03d", i+1)
		if _, err := db.Exec(`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
			collaboration_mode, status, idempotency_key, created_at, updated_at)
			VALUES (?, ?, 'Recovery', '', 'recover abandoned work', 'recovery', 'development', 'solo', 'running', ?, '', '')`, taskID, runner, runner); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, runner_id, created_at)
			VALUES (?, ?, 1, 'continue', 'running', ?, '')`, runner, taskID, runner); err != nil {
			t.Fatal(err)
		}
	}
	if err := recoverOnStartup(ctx, db, started, started.Add(store.StartupHeartbeatGrace-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM executions WHERE status = 'running'`).Scan(&active); err != nil || active != 3 {
		t.Fatalf("recovered before grace: %d err=%v", active, err)
	}
	if _, err := store.Heartbeat(ctx, db, "resumed", "same-boot", []string{"resumed"}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := recoverOnStartup(ctx, db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
			t.Fatal(err)
		}
		for _, runner := range []string{"silent", "resumed", "never-heartbeat"} {
			var execution, task, runnerStatus string
			var requests, events, audits, executions int
			var classified, finished bool
			if err := db.QueryRow(`SELECT e.status, t.status, r.status,
				coalesce(e.error_class = 'interrupted', 0), e.finished_at IS NOT NULL,
				(SELECT count(*) FROM notification_requests WHERE execution_id = e.id AND status = 'interrupted'),
				(SELECT count(*) FROM execution_events WHERE execution_id = e.id AND type = 'status_changed' AND json_extract(payload, '$.reason') = 'runner_heartbeat_lost'),
				(SELECT count(*) FROM audit_logs WHERE target_id = e.id AND actor = 'system' AND action = 'execution.interrupt'),
				(SELECT count(*) FROM executions WHERE task_id = t.id)
				FROM executions e JOIN tasks t ON t.id = e.task_id JOIN runners r ON r.id = e.runner_id
				WHERE e.id = ?`, runner).Scan(&execution, &task, &runnerStatus, &classified, &finished, &requests, &events, &audits, &executions); err != nil {
				t.Fatal(err)
			}
			if runner == "resumed" {
				if execution != "running" || task != "running" || runnerStatus != "online" || classified || finished || requests != 0 || events != 0 || audits != 0 || executions != 1 {
					t.Fatalf("survivor changed: %s %s %s counts=%d/%d/%d/%d", execution, task, runnerStatus, requests, events, audits, executions)
				}
				continue
			}
			if execution != "interrupted" || task != "failed" || runnerStatus != "offline" || !classified || !finished || requests != 1 || events != 1 || audits != 1 || executions != 1 {
				t.Fatalf("recovery %s: %s %s %s classified=%v finished=%v counts=%d/%d/%d/%d", runner, execution, task, runnerStatus, classified, finished, requests, events, audits, executions)
			}
		}
	}
}
