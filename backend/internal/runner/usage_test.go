package runner

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

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
	clock := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	handlers := stubStageHandlers(func(id StageID, _ context.Context, execution *Execution) error {
		if id != StageComplete {
			clock = clock.Add(100 * time.Millisecond)
		}
		if id == StageHarness {
			result := &adapter.HarnessResult{Usage: adapter.Usage{WallTimeMS: 71}}
			if tokensAvailable {
				result.Usage.InputTokens, result.Usage.OutputTokens = new(int64(0)), new(int64(17))
			}
			execution.HarnessResult = result
		}
		return nil
	})
	control := testExecutionControl()
	control.Reporter = process // Use the production authenticated result API and storage path.
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
		if wall != 1200 || status != "succeeded" {
			t.Fatalf("execution=%s wall_time_ms=%d status=%s; want 1200 succeeded", id, wall, status)
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

// A real storage failure must roll back completion. Runner retries the same
// HTTP body, without repeating publishing or measuring the retry delay.
func TestUsageStorageFailureRetriesResultWithoutRerunningStages(t *testing.T) {
	f := newLogControlPlane(t)
	if _, err := f.db.Exec(`UPDATE executions SET status = 'claimed' WHERE id = ?`, logExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_usage BEFORE INSERT ON usage_records
		BEGIN SELECT RAISE(ABORT, 'usage storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	process.pollInterval = time.Millisecond
	clock := time.Now()
	var attempts, publishes int
	var firstBody []byte
	process.client.Transport = usageRetryTransport(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		response, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if bytes.Contains(body, []byte(`"status":"succeeded"`)) {
			attempts++
			if attempts == 1 {
				firstBody = body
				var status string
				var rows int
				err := f.db.QueryRow(`SELECT status, (SELECT count(*) FROM usage_records) FROM executions WHERE id = ?`, logExecution).Scan(&status, &rows)
				if err != nil || status != "publishing" || rows != 0 || response.StatusCode != 500 {
					t.Errorf("failed usage report: status=%s rows=%d HTTP=%d err=%v", status, rows, response.StatusCode, err)
				}
				if _, err := f.db.Exec(`DROP TRIGGER reject_usage`); err != nil {
					t.Error(err)
				}
				clock = clock.Add(10 * time.Second)
			} else if !bytes.Equal(body, firstBody) {
				t.Error("retry changed usage measurements")
			}
		}
		return response, nil
	})
	control := testExecutionControl()
	control.Reporter = process
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	control.ReportContext = ctx
	executor, err := NewExecutorWithControl(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
		if id != StageComplete {
			clock = clock.Add(100 * time.Millisecond)
		}
		if id == StagePublish {
			publishes++
		}
		return nil
	}), control)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return clock }
	claim := Claim{}
	claim.Execution.ID = logExecution
	if err := executor.Execute(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var wall, rows int
	if err := f.db.QueryRow(`SELECT count(*), max(wall_time_ms) FROM usage_records WHERE execution_id = ?`, logExecution).Scan(&rows, &wall); err != nil ||
		rows != 1 || wall != 1200 || attempts != 2 || publishes != 1 {
		t.Fatalf("retry: rows=%d wall=%d attempts=%d publishes=%d err=%v", rows, wall, attempts, publishes, err)
	}
}

func TestUsageNotRecordedWhenExecutionStopsBeforeCompletion(t *testing.T) {
	for _, failure := range []error{errors.New("failed or blocked"), context.Canceled, context.DeadlineExceeded} {
		f := newLogControlPlane(t)
		if _, err := f.db.Exec(`UPDATE executions SET status = 'claimed' WHERE id = ?`, logExecution); err != nil {
			t.Fatal(err)
		}
		process, err := New(f.config)
		if err != nil {
			t.Fatal(err)
		}
		control := testExecutionControl()
		control.Reporter = process
		executor, err := NewExecutorWithControl(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
			if id == StageHarness {
				return failure
			}
			if id == StageComplete {
				t.Fatal("stopped execution reached completion")
			}
			return nil
		}), control)
		if err != nil {
			t.Fatal(err)
		}
		claim := Claim{}
		claim.Execution.ID = logExecution
		if err := executor.Execute(context.Background(), claim); !errors.Is(err, failure) {
			t.Fatalf("lost stop error: %v", err)
		}
		var rows int
		if err := f.db.QueryRow(`SELECT count(*) FROM usage_records`).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("stopped execution recorded usage: rows=%d err=%v", rows, err)
		}
	}
}

func TestResumedUsageDoesNotInventHarnessMeasurements(t *testing.T) {
	clock := time.Now()
	var reported bool
	control := testExecutionControl()
	control.Reporter = reportFunc(func(_ context.Context, _ string, result ExecutionResult) error {
		if result.Status == model.ExecutionSucceeded {
			reported = true
			usage := result.Usage
			if usage == nil || usage.WallTimeMS != 200 || usage.HarnessWallTimeMS != nil || usage.HarnessUsage != nil {
				t.Fatalf("resumed usage invented skipped harness measurements: %+v", usage)
			}
		}
		return nil
	})
	executor, err := NewExecutorWithControl(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
		if id != StageComplete {
			clock = clock.Add(100 * time.Millisecond)
		}
		return nil
	}), control)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return clock }
	claim := Claim{}
	claim.Execution.ResumePoint = new("gate3")
	if err := executor.Execute(context.Background(), claim); err != nil || !reported {
		t.Fatalf("resumed usage: reported=%v err=%v", reported, err)
	}
}

type usageRetryTransport func(*http.Request) (*http.Response, error)

func (transport usageRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}
