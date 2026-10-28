package runner

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type usageRecorderFunc func(context.Context, string, ExecutionUsage) error

func (record usageRecorderFunc) RecordUsage(ctx context.Context, id string, usage ExecutionUsage) error {
	return record(ctx, id, usage)
}

// Test-only bridge across the unspecified usage transport. Production Runner
// must not write the management DB. This records total duration in one row;
// no second row/column convention for harness duration is invented here.
func testUsageRecorder(t *testing.T, db *sql.DB) UsageRecorder {
	t.Helper()
	return usageRecorderFunc(func(ctx context.Context, id string, usage ExecutionUsage) error {
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM executions WHERE id = ?`, id).Scan(&status); err != nil {
			return err
		}
		if status != "publishing" {
			t.Fatalf("usage must be recorded before succeeded: status=%s", status)
		}
		if usage.HarnessWallTimeMS == nil || *usage.HarnessWallTimeMS != 100 || usage.HarnessUsage == nil || usage.HarnessUsage.WallTimeMS != 71 {
			t.Fatalf("lost measured harness time or adapter usage: %+v", usage)
		}
		return store.RecordUsage(ctx, db, store.UsageRecord{
			ID: ulid.Make().String(), ExecutionID: id, WallTimeMS: usage.WallTimeMS,
			InputTokens: usage.HarnessUsage.InputTokens, OutputTokens: usage.HarnessUsage.OutputTokens,
		})
	})
}

func TestCompletedExecutionsRecordWallTime(t *testing.T) {
	testCompletedExecutionUsage(t, true)
}

func TestAdapterWithoutTokenUsageRecordsNullTokens(t *testing.T) {
	testCompletedExecutionUsage(t, false)
}

func testCompletedExecutionUsage(t *testing.T, tokensAvailable bool) {
	t.Helper()
	f := newLogControlPlane(t)
	if _, err := f.db.Exec(`UPDATE executions SET status = 'claimed' WHERE id = ?`, logExecution); err != nil {
		t.Fatal(err)
	}
	second := ulid.Make().String()
	if _, err := f.db.Exec(`INSERT INTO tasks
		(id, project_id, title, body, acceptance_criteria, short_name, task_type, collaboration_mode, status, idempotency_key, created_at, updated_at)
		SELECT 'task-002', project_id, title, body, acceptance_criteria, short_name, task_type, collaboration_mode, status, 'usage-2', created_at, updated_at
		FROM tasks WHERE id = 'task-001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, runner_id, created_at)
		VALUES (?, 'task-002', 1, 'fresh', 'claimed', 'mac-mini', 'now')`, second); err != nil {
		t.Fatal(err)
	}
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := NewUsageStage(testUsageRecorder(t, f.db))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	handlers := stubStageHandlers(func(id StageID, _ context.Context, execution *Execution) error {
		clock = clock.Add(100 * time.Millisecond)
		if id == StageHarness {
			result := &adapter.HarnessResult{Usage: adapter.Usage{WallTimeMS: 71}}
			if tokensAvailable {
				result.Usage.InputTokens, result.Usage.OutputTokens = new(int64(0)), new(int64(17))
			}
			execution.HarnessResult = result
		}
		return nil
	})
	handlers[StageComplete] = complete
	control := testExecutionControl()
	control.Reporter = process // Exercise the unchanged authenticated result API.
	control.CheckCancellation = process.Heartbeat
	executor, err := NewExecutorWithControl(handlers, control)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return clock }
	for _, id := range []string{logExecution, second} {
		claim := Claim{}
		claim.Execution.ID = id
		if err := executor.Execute(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		var wall int64
		var input, output sql.NullInt64
		var cost sql.NullFloat64
		var status, created string
		if err := f.db.QueryRow(`SELECT u.wall_time_ms, u.input_tokens, u.output_tokens, u.cost_amount, e.status, u.created_at
			FROM usage_records u JOIN executions e ON e.id = u.execution_id WHERE e.id = ?`, id).
			Scan(&wall, &input, &output, &cost, &status, &created); err != nil {
			t.Fatal(err)
		}
		if wall != 1300 || status != "succeeded" {
			t.Fatalf("execution=%s wall_time_ms=%d status=%s; want 1300 succeeded", id, wall, status)
		}
		if tokensAvailable {
			if !input.Valid || input.Int64 != 0 || !output.Valid || output.Int64 != 17 {
				t.Fatalf("available tokens lost (zero must not become NULL): input=%+v output=%+v", input, output)
			}
		} else if input.Valid || output.Valid {
			t.Fatalf("unavailable adapter tokens must be SQL NULL: input=%+v output=%+v", input, output)
		}
		if cost.Valid {
			t.Fatalf("unspecified cost must remain NULL: %+v", cost)
		}
		if _, err := time.Parse(time.RFC3339, created); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM usage_records WHERE execution_id = ?`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("execution=%s usage rows=%d err=%v; want 1", id, count, err)
		}
	}
}

func TestUsageRecordFailurePreventsSuccessWithoutRerunningStages(t *testing.T) {
	failure := errors.New("usage storage unavailable")
	var records, publishes, successes int
	complete, err := NewUsageStage(usageRecorderFunc(func(context.Context, string, ExecutionUsage) error {
		records++
		return failure
	}))
	if err != nil {
		t.Fatal(err)
	}
	handlers := stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
		if id == StagePublish {
			publishes++
		}
		return nil
	})
	handlers[StageComplete] = complete
	control := testExecutionControl()
	control.Reporter = reportFunc(func(_ context.Context, _ string, result ExecutionResult) error {
		if result.Status == model.ExecutionSucceeded {
			successes++
		}
		return nil
	})
	executor, err := NewExecutorWithControl(handlers, control)
	if err != nil {
		t.Fatal(err)
	}
	err = executor.Execute(context.Background(), Claim{})
	var stageError *StageError
	if !errors.Is(err, failure) || !errors.As(err, &stageError) || stageError.Stage.ID != StageComplete || records != 1 || publishes != 1 || successes != 0 {
		t.Fatalf("usage failure: error=%v records=%d publishes=%d successes=%d", err, records, publishes, successes)
	}
}

func TestUsageNotRecordedWhenExecutionStopsBeforeCompletion(t *testing.T) {
	for _, failure := range []error{errors.New("failed or blocked"), context.Canceled, context.DeadlineExceeded} {
		complete, err := NewUsageStage(usageRecorderFunc(func(context.Context, string, ExecutionUsage) error {
			t.Fatal("stopped execution reached completion usage recorder")
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		handlers := stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
			if id == StageHarness {
				return failure
			}
			return nil
		})
		handlers[StageComplete] = complete
		executor, err := NewExecutorWithControl(handlers, testExecutionControl())
		if err != nil {
			t.Fatal(err)
		}
		if err := executor.Execute(context.Background(), Claim{}); !errors.Is(err, failure) {
			t.Fatalf("lost stop error: %v", err)
		}
	}
}

func TestResumedUsageDoesNotInventHarnessMeasurements(t *testing.T) {
	clock := time.Now()
	var recorded bool
	complete, err := NewUsageStage(usageRecorderFunc(func(_ context.Context, _ string, usage ExecutionUsage) error {
		recorded = true
		if usage.WallTimeMS != 300 || usage.HarnessWallTimeMS != nil || usage.HarnessUsage != nil {
			t.Fatalf("resumed usage invented skipped harness measurements: %+v", usage)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	handlers := stubStageHandlers(func(StageID, context.Context, *Execution) error {
		clock = clock.Add(100 * time.Millisecond)
		return nil
	})
	handlers[StageComplete] = complete
	executor, err := NewExecutor(handlers)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return clock }
	claim := Claim{}
	claim.Execution.ResumePoint = new("gate2")
	if err := executor.Execute(context.Background(), claim); err != nil || !recorded {
		t.Fatalf("resumed usage: recorded=%v err=%v", recorded, err)
	}
	if _, err := NewUsageStage(nil); err == nil {
		t.Fatal("missing usage recorder accepted")
	}
}
