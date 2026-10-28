package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Condition 31: the only concrete adapter reference is the registration line.
// The production Executor, claim API, result API and state persistence are used.
// Other stage bodies are stubs at Executor's existing StageFunc boundary: this
// tests extension, not worktrees, sandbox isolation, Gates or Git Publisher.
func TestDummyAdapterExecutionExtension(t *testing.T) {
	gateFailure := errors.New("gate rejected change")
	for _, tc := range []struct {
		name       string
		registered bool
		failGate   bool
	}{
		{name: "succeeds", registered: true},
		{name: "unregistered"},
		{name: "gate_failure", registered: true, failGate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var adapters adapter.Registry
			if tc.registered {
				if err := adapters.Register("dummy", dummy.New()); err != nil {
					t.Fatal(err)
				}
			}
			db, process := extensionControlPlane(t, &adapters)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Reconcile this boot before claiming, as the production pull loop does.
			if err := process.Heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			claim, err := process.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			if claim.Profile.Adapter != "dummy" || claim.Execution.ResumePoint != nil {
				t.Fatalf("claim must select dummy and start at stage 1: %+v", claim)
			}
			var called []StageID
			var harnessResult *adapter.HarnessResult
			var harnessLogs []adapter.LogLine
			handlers := stubStageHandlers(func(id StageID, ctx context.Context, execution *Execution) error {
				called = append(called, id)
				if id == StageHarness {
					result, logs, err := runExtensionHarness(ctx, &adapters, execution.Claim, t.TempDir())
					if err != nil {
						return err
					}
					execution.HarnessResult, harnessResult, harnessLogs = result, result, logs
				}
				if id == StageGate2 && tc.failGate {
					return gateFailure
				}
				return nil
			})
			control := testExecutionControl()
			control.Reporter, control.ReportContext = process, ctx
			control.CheckCancellation = process.Heartbeat
			executor, err := NewExecutorWithControl(handlers, control)
			if err != nil {
				t.Fatal(err)
			}
			err = executor.Execute(ctx, *claim)
			wantCalls := expectedStageIDs
			wantStatuses := []string{"claimed", "preparing", "running", "verifying", "publishing", "succeeded"}
			wantExecution, wantTask, wantUsage := "succeeded", "completed", 1
			switch {
			case !tc.registered:
				assertExtensionStageError(t, err, StageHarness, adapter.ErrNotRegistered)
				wantCalls, wantStatuses = expectedStageIDs[:8], wantStatuses[:3]
				wantExecution, wantTask, wantUsage = "running", "running", 0
			case tc.failGate:
				assertExtensionStageError(t, err, StageGate2, gateFailure)
				wantCalls, wantStatuses = expectedStageIDs[:11], wantStatuses[:4]
				wantExecution, wantTask, wantUsage = "verifying", "verifying", 0
			case err != nil:
				t.Fatal(err)
			}
			if !reflect.DeepEqual(called, wantCalls) {
				t.Fatalf("stages=%v; want %v", called, wantCalls)
			}
			if tc.registered {
				if harnessResult == nil || harnessResult.SessionID == "" ||
					harnessResult.Summary != "dummy completed "+claim.Execution.ID || len(harnessLogs) != 1 || harnessLogs[0].Stream != "stdout" {
					t.Fatalf("adapter was not exercised: result=%+v logs=%+v", harnessResult, harnessLogs)
				}
			}
			var executionStatus, taskStatus string
			var finished sql.NullString
			var usage int
			err = db.QueryRow(`SELECT e.status, t.status, e.finished_at,
				(SELECT count(*) FROM usage_records WHERE execution_id = e.id)
				FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, claim.Execution.ID).
				Scan(&executionStatus, &taskStatus, &finished, &usage)
			if err != nil || executionStatus != wantExecution || taskStatus != wantTask || usage != wantUsage || finished.Valid != (wantUsage == 1) {
				t.Fatalf("execution=%s task=%s finished=%+v usage=%d err=%v", executionStatus, taskStatus, finished, usage, err)
			}
			events, err := store.ListExecutionEvents(ctx, db, claim.Execution.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var statuses []string
			for _, event := range events {
				if event.Type == model.EventStatusChanged {
					var payload struct{ To string }
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					statuses = append(statuses, payload.To)
				}
			}
			if !reflect.DeepEqual(statuses, wantStatuses) {
				t.Fatalf("persisted transitions=%v; want %v", statuses, wantStatuses)
			}
		})
	}
}

// This harness stage uses only the name from the real claim and adapter.Adapter.
// No concrete adapter type or name-specific branch is needed in the executor.
func runExtensionHarness(ctx context.Context, adapters *adapter.Registry, claim Claim, workdir string) (*adapter.HarnessResult, []adapter.LogLine, error) {
	harness, err := adapters.Lookup(claim.Profile.Adapter)
	if err != nil {
		return nil, nil, err
	}
	request := adapter.HarnessRequest{
		ContractVersion: adapter.ContractVersion, ExecutionID: claim.Execution.ID,
		Task: adapter.RequestTask{ID: claim.Execution.TaskID, Title: claim.Task.Title,
			Body: claim.Task.Body, AcceptanceCriteria: claim.Task.AcceptanceCriteria},
		Project: adapter.RequestProject{ID: claim.Project.ID, Workdir: workdir,
			Branch: "evaloom/extension", BaseSHA: strings.Repeat("a", 40)},
		Profile: adapter.RequestProfile{HarnessID: claim.Profile.HarnessID, Version: claim.Profile.Version,
			CommitSHA: claim.Profile.CommitSHA, ModelID: "test-model", Definition: claim.Profile.Definition},
		Context: adapter.RequestContext{RevisionIDs: []string{}},
	}
	id, err := harness.Start(ctx, request, adapter.SandboxEnv{Worktree: workdir})
	if err != nil {
		return nil, nil, err
	}
	status, err := harness.Status(id)
	if err != nil {
		return nil, nil, err
	}
	if status != adapter.SessionSucceeded {
		return nil, nil, fmt.Errorf("harness did not succeed: %s", status)
	}
	lines, err := harness.Logs(id)
	if err != nil {
		return nil, nil, err
	}
	var logs []adapter.LogLine
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case line, ok := <-lines:
			if ok {
				logs = append(logs, line)
				continue
			}
			result, err := harness.Result(id)
			if err == nil {
				err = result.Validate()
			}
			if err == nil && (result.Status != adapter.ResultSucceeded || result.SessionID != id) {
				err = fmt.Errorf("harness returned an unsuccessful or mismatched result")
			}
			return &result, logs, err
		}
	}
}

func assertExtensionStageError(t *testing.T, err error, id StageID, cause error) {
	t.Helper()
	var stageErr *StageError
	if !errors.Is(err, cause) || !errors.As(err, &stageErr) || stageErr.Stage.ID != id {
		t.Fatalf("error=%v; want %s wrapping %v", err, id, cause)
	}
}

func extensionControlPlane(t *testing.T, adapters *adapter.Registry) (*sql.DB, *Runner) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := httptest.NewUnstartedServer(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := api.NewOwnerAuth(db, logger, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = api.NewHandler(db, logger, auth, adapters)
	server.Start()
	t.Cleanup(server.Close)
	config := testConfig(t, server.URL)
	token, err := runnerauth.Register(ctx, db, config.RunnerID, "Extension Runner", config.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	// Seed only inputs. Claim and every subsequent state transition must come
	// from production HTTP handlers; never write a successful execution here.
	for _, query := range []string{
		`INSERT INTO projects (id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,test_file_patterns,allowed_operations,approval_policy,timeouts,created_at,updated_at)
		 VALUES ('project','Project','/repo','','mac-mini','main','developer','AGENTS.md','[]','[]','{}','{}','{}','now','now')`,
		`INSERT INTO harnesses (id,name,specialty,supported_tasks,adapter,repo_path,repo_url,supported_models,available_tools,tool_permissions,contract_version,created_at,updated_at)
		 VALUES ('harness','Harness','developer','["development"]','dummy','/harness','test://harness','[]','[]','{}','1','now','now')`,
		`INSERT INTO harness_profiles (id,harness_id,version,commit_sha,tag_name,definition,definition_hash,model_id,operation_status,registered_at)
		 VALUES ('profile','harness','1.0.0','` + strings.Repeat("a", 40) + `','v1.0.0','{}','test-hash','test-model','provisional','now')`,
		`INSERT INTO project_harnesses (project_id,role,harness_id,profile_id,assigned_at)
		 VALUES ('project','developer','harness','profile','now')`,
		`INSERT INTO tasks (id,project_id,title,body,acceptance_criteria,short_name,task_type,collaboration_mode,status,idempotency_key,created_at,updated_at)
		 VALUES ('task','project','Extension test','','Adapter succeeds','extension','development','solo','queued','extension','now','now')`,
		`INSERT INTO executions (id,task_id,seq,mode,status,created_at)
		 VALUES ('` + logExecution + `','task',1,'fresh','pending','now')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return db, process
}
