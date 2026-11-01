package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Exercise Q38 through the actual parser, artifact upload, result endpoint and
// SQLite persistence for both stages. Preserve duplicate IDs when comparing.
func TestJUnitIDsPersistIdenticallyForBaselineAndGate2(t *testing.T) {
	for _, missingDuplicate := range []bool{false, true} {
		name := "SameIDs"
		if missingDuplicate {
			name = "MissingDuplicateBlocks"
		}
		t.Run(name, func(t *testing.T) {
			f := newGate2Fixture(t)
			baseline := `<testsuite><testcase id="ignored" classname="FooTest" name="testBar"/><testcase name="plain"/><testcase classname="" name="emptyClass"/><testcase classname="FooTest" name="testBar"/><testcase name="skip"><skipped/></testcase></testsuite>`
			gate2Write(t, filepath.Join(f.path, "cases.xml"), baseline)
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			claim, err := p.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			calls := 0
			err = f.gate2Executor(t, p, &calls, func(context.Context, *Execution) error {
				if missingDuplicate {
					gate2Write(t, filepath.Join(f.path, "cases.xml"), `<testsuite><testcase classname="FooTest" name="testBar"/><testcase name="plain"/><testcase classname="" name="emptyClass"/><testcase name="new"/><testcase name="skip"><skipped/></testcase></testsuite>`)
				}
				return nil
			}, nil, nil).Execute(ctx, *claim)
			if missingDuplicate && !errors.Is(err, ErrGate2Stopped) || !missingDuplicate && err != nil || calls != 2 {
				t.Fatalf("execution=%v calls=%d", err, calls)
			}
			want := []string{"FooTest.testBar", "plain", "emptyClass", "FooTest.testBar"}
			for _, kind := range []string{"baseline", "verify"} {
				var path string
				if err := f.db.QueryRow(`SELECT a.storage_path FROM test_runs r JOIN artifacts a ON a.id=r.test_ids_artifact_id WHERE r.execution_id=? AND r.kind=? AND a.kind='test_ids'`, claim.Execution.ID, kind).Scan(&path); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(f.dataDir, path))
				var ids []string
				if err != nil || json.Unmarshal(body, &ids) != nil {
					t.Fatalf("artifact=%q err=%v", body, err)
				}
				if missingDuplicate && kind == "verify" {
					want = []string{"FooTest.testBar", "plain", "emptyClass", "new"}
				}
				if !reflect.DeepEqual(ids, want) {
					t.Fatalf("%s IDs=%v want=%v", kind, ids, want)
				}
			}
			var status, findings string
			if err := f.db.QueryRow(`SELECT e.status,g.findings FROM executions e JOIN gate_results g ON g.execution_id=e.id AND g.gate=2 WHERE e.id=?`, claim.Execution.ID).Scan(&status, &findings); err != nil {
				t.Fatal(err)
			}
			wantStatus := "succeeded"
			if missingDuplicate {
				wantStatus = "blocked"
				var saved []model.Finding
				if err := json.Unmarshal([]byte(findings), &saved); err != nil || len(saved) != 1 || saved[0].Rule != "existing_test_not_run" {
					t.Fatalf("missing duplicate finding=%s err=%v", findings, err)
				}
			}
			if status != wantStatus {
				t.Fatalf("status=%s want=%s", status, wantStatus)
			}
		})
	}
}

func TestBaselineQ39RecordsFailuresAndStopsOnlyWithoutCounts(t *testing.T) {
	for _, tc := range []struct {
		name, command          string
		failed, exit           int
		countOnly, unavailable bool
	}{
		{name: "FailingTestWithZeroExit", command: `printf '%s' '<testsuite><testcase classname="FooTest" name="failed"><failure/></testcase></testsuite>' > report.xml`, failed: 1},
		{name: "FailingTestWithNonzeroExit", command: `printf '%s' '<testsuite><testcase classname="FooTest" name="failed"><error/></testcase></testsuite>' > report.xml; exit 2`, failed: 1, exit: 2},
		{name: "NonzeroExitWithPassingReport", command: `printf '%s' '<testsuite><testcase classname="FooTest" name="failed"/></testsuite>' > report.xml; exit 1`, exit: 1},
		{name: "CountsOnlyFailure", command: `printf '%s' '<testsuite tests="1" failures="1"/>' > report.xml; exit 1`, failed: 1, exit: 1, countOnly: true},
		{name: "MissingTestName", command: `printf '%s' '<testsuite><testcase id="ignored"/></testsuite>' > report.xml`, countOnly: true},
		{name: "NoReport", command: `true`, unavailable: true},
		{name: "NoReportWithNonzeroExit", command: `exit 1`, exit: 1, unavailable: true},
		{name: "MalformedReport", command: `printf '%s' '<testsuite>' > report.xml`, unavailable: true},
		{name: "UnreadableReport", command: `ln -s missing report.xml`, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBaselineHTTPFixture(t)
			f.command = tc.command
			if _, err := f.db.Exec(`UPDATE projects SET test_command=?`, f.command); err != nil {
				t.Fatal(err)
			}
			// A stale report must not rescue an unavailable measurement.
			gate2Write(t, filepath.Join(f.path, "report.xml"), `<testsuite tests="5"/>`)
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			claim, err := p.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			calls := 0
			executor := f.executor(t, p, &calls)
			harnessCalls := 0
			for i := range executor.stages {
				if executor.stages[i].ID == StageHarness {
					original := executor.stages[i].run
					executor.stages[i].run = func(ctx context.Context, e *Execution) error {
						harnessCalls++
						return original(ctx, e) // verifies durable baseline before harness
					}
				}
			}
			err = executor.Execute(ctx, *claim)
			if tc.unavailable && !errors.Is(err, ErrBaselineUnverifiable) || !tc.unavailable && err != nil || calls != 1 {
				t.Fatalf("execution=%v sandbox calls=%d", err, calls)
			}
			run, err := (store.BaselineStore{DB: f.db}).FindBaseline(ctx, claim.Execution.ID, f.worktreeID)
			if err != nil || run == nil || run.ExitCode == nil || *run.ExitCode != tc.exit || run.TargetSHA != f.sha || run.Kind != "baseline" {
				t.Fatalf("saved baseline=%+v err=%v", run, err)
			}
			var status, class, reason string
			var notifications int
			if err := f.db.QueryRow(`SELECT status,coalesce(error_class,''),coalesce(error_message,''),(SELECT count(*) FROM notification_requests WHERE execution_id=e.id AND status='failed') FROM executions e WHERE id=?`, claim.Execution.ID).Scan(&status, &class, &reason, &notifications); err != nil {
				t.Fatal(err)
			}
			if tc.unavailable {
				if status != "failed" || class != "config_error" || notifications != 1 || reason != "開始点のテストレポートから基準値の件数を取得できない" || harnessCalls != 0 || run.Total != nil || run.Skipped != nil || run.TestIDsArtifactID != nil {
					t.Fatalf("status=%s class=%s notifications=%d harness=%d run=%+v", status, class, notifications, harnessCalls, run)
				}
				return
			}
			if status != "succeeded" || class != "" || harnessCalls != 1 || run.Total == nil || *run.Total != 1 || run.Failed == nil || *run.Failed != tc.failed || run.Skipped == nil || *run.Skipped != 0 || (run.TestIDsArtifactID == nil) != tc.countOnly {
				t.Fatalf("status=%s class=%s harness=%d baseline=%+v", status, class, harnessCalls, run)
			}
			if !tc.countOnly {
				var path string
				if err := f.db.QueryRow(`SELECT storage_path FROM artifacts WHERE id=?`, *run.TestIDsArtifactID).Scan(&path); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(f.dataDir, path))
				if err != nil || string(body) != `["FooTest.failed"]` {
					t.Fatalf("failed test ID missing: %s err=%v", body, err)
				}
			}
		})
	}
}
