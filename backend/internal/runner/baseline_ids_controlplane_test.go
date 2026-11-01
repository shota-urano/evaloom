package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

const savedIDsXML = `<testsuite><testcase classname="Z" name="重複"/><testcase name="a"/><testcase classname="Z" name="重複"/><testcase name="disabled"><skipped/></testcase></testsuite>`

// Q40: no artifact reader is injected into the Runner. The only data crossing
// the restart boundary is persisted Control Plane data and the HTTP claim.
func TestBaselineIDsHTTPClaimAndGate2AfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name, baseline, verify, damagedJSON, damage string
		wantIDs                                     []string
		cause                                       model.BlockCause
		countOnly                                   bool
	}{
		{name: "OrderDuplicatesAndUnicode", wantIDs: []string{"Z.重複", "a", "Z.重複"}},
		{name: "MissingExistingIDDespiteEqualCounts", wantIDs: []string{"Z.重複", "a", "Z.重複"}, verify: `<testsuite><testcase classname="Z" name="重複"/><testcase name="new"/><testcase classname="Z" name="重複"/><testcase name="skip"><skipped/></testcase></testsuite>`, cause: model.BlockGate2MissingTests},
		{name: "MissingDuplicateDespiteEqualCounts", wantIDs: []string{"Z.重複", "a", "Z.重複"}, verify: `<testsuite><testcase classname="Z" name="重複"/><testcase name="a"/><testcase name="new"/><testcase name="skip"><skipped/></testcase></testsuite>`, cause: model.BlockGate2MissingTests},
		{name: "MissingArtifactRow", damage: "row", cause: model.BlockGate2Unverifiable},
		{name: "UnreadableArtifactFile", damage: "file", cause: model.BlockGate2Unverifiable},
		{name: "MissingIDsWithCountOnlyVerify", damage: "file", verify: `<testsuite tests="5" skipped="1"/>`, cause: model.BlockGate2Unverifiable},
		{name: "MalformedJSON", damagedJSON: `["a",`, cause: model.BlockGate2Unverifiable},
		{name: "JSONObject", damagedJSON: `{"testIds":["Z.重複","a","Z.重複"]}`, cause: model.BlockGate2Unverifiable},
		{name: "JSONString", damagedJSON: `"Z.重複"`, cause: model.BlockGate2Unverifiable},
		{name: "JSONNull", damagedJSON: `null`, cause: model.BlockGate2Unverifiable},
		{name: "JSONNullElement", damagedJSON: `["Z.重複",null,"Z.重複"]`, cause: model.BlockGate2Unverifiable},
		{name: "JSONNumberElement", damagedJSON: `["Z.重複",1,"Z.重複"]`, cause: model.BlockGate2Unverifiable},
		{name: "JSONBooleanElement", damagedJSON: `["Z.重複",true,"Z.重複"]`, cause: model.BlockGate2Unverifiable},
		{name: "JSONArrayElement", damagedJSON: `["Z.重複",[],"Z.重複"]`, cause: model.BlockGate2Unverifiable},
		{name: "TrailingJSON", damagedJSON: `["Z.重複","a","Z.重複"] []`, cause: model.BlockGate2Unverifiable},
		{name: "InvalidUTF8", damagedJSON: "[\"Z.重複\",\"\xff\",\"Z.重複\"]", cause: model.BlockGate2Unverifiable},
		{name: "TooFewIDs", damagedJSON: `["Z.重複","a"]`, cause: model.BlockGate2Unverifiable},
		{name: "TooManyIDs", damagedJSON: `["Z.重複","a","Z.重複","extra"]`, cause: model.BlockGate2Unverifiable},
		{name: "WrongArtifactKind", damage: "kind", cause: model.BlockGate2Unverifiable},
		{name: "OtherExecutionArtifact", damage: "execution", cause: model.BlockGate2Unverifiable},
		{name: "EmptyIDArray", baseline: `<testsuite tests="0"/>`, verify: `<testsuite><testcase name="new"/></testsuite>`, wantIDs: []string{}},
		{name: "CountOnlyBaselinePassesWithMoreTests", baseline: `<testsuite tests="4" skipped="1"/>`, countOnly: true},
		{name: "CountOnlyBaselineFewerExecuted", baseline: `<testsuite tests="4" skipped="1"/>`, verify: `<testsuite tests="3" skipped="1"/>`, countOnly: true, cause: model.BlockGate2MissingTests},
		{name: "CountOnlyBaselineMoreSkipped", baseline: `<testsuite tests="4" skipped="1"/>`, verify: `<testsuite tests="6" skipped="2"/>`, countOnly: true, cause: model.BlockGate2MissingTests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGate2Fixture(t)
			baselineXML := tc.baseline
			if baselineXML == "" {
				baselineXML = savedIDsXML
			}
			gate2Write(t, filepath.Join(f.path, "cases.xml"), baselineXML)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			firstRunner, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			first, err := firstRunner.claim(ctx)
			if err != nil || first == nil {
				t.Fatalf("first claim=%+v error=%v", first, err)
			}
			calls := 0
			// Persist the baseline through the product upload/result path. Gate 2
			// runs in the next Execution, after both processes are recreated.
			if err := f.executor(t, firstRunner, &calls).Execute(ctx, *first); err != nil {
				t.Fatal(err)
			}
			var artifactID, storagePath string
			var savedIDs []string
			if !tc.countOnly {
				if err := f.db.QueryRow(`SELECT a.id,a.storage_path FROM test_runs r JOIN artifacts a ON a.id=r.test_ids_artifact_id WHERE r.kind='baseline'`).Scan(&artifactID, &storagePath); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(f.dataDir, storagePath))
				if err != nil || json.Unmarshal(body, &savedIDs) != nil {
					t.Fatalf("saved IDs=%q error=%v", body, err)
				}
			}
			id := ulid.Make().String()
			if _, err := f.db.Exec(`INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES (?,'task-001',2,'continue','pending','now'); UPDATE tasks SET status='queued'`, id); err != nil {
				t.Fatal(err)
			}
			switch tc.damage {
			case "row":
				// Model a damaged/missing artifact row while retaining the saved
				// reference. Foreign keys normally prevent this state.
				if _, err := f.db.Exec(`PRAGMA foreign_keys=OFF; DELETE FROM artifacts WHERE id=?; PRAGMA foreign_keys=ON`, artifactID); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.Remove(filepath.Join(f.dataDir, storagePath)); err != nil {
					t.Fatal(err)
				}
			case "kind":
				if _, err := f.db.Exec(`UPDATE artifacts SET kind='harness_log' WHERE id=?`, artifactID); err != nil {
					t.Fatal(err)
				}
			case "execution":
				if _, err := f.db.Exec(`UPDATE artifacts SET execution_id=? WHERE id=?`, id, artifactID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.damagedJSON != "" {
				gate2Write(t, filepath.Join(f.dataDir, storagePath), tc.damagedJSON)
			}
			f.stop()
			f.start()
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			var rawIDs json.RawMessage
			// Observe the wire field and reject any attempt by the Runner to
			// read an artifact API. No storage reader is available to it.
			process.client.Transport = usageRetryTransport(func(r *http.Request) (*http.Response, error) {
				path := r.URL.Path
				if r.Method != http.MethodPost || path != "/api/runners/mac-mini/claim" && path != "/api/executions/"+id+"/result" && path != "/api/executions/"+id+"/artifacts" {
					return nil, fmt.Errorf("unexpected Runner API: %s %s", r.Method, path)
				}
				response, err := http.DefaultTransport.RoundTrip(r)
				if err != nil || path != "/api/runners/mac-mini/claim" {
					return response, err
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					return nil, err
				}
				var wire struct {
					Worktree struct {
						Baseline struct {
							TestIDs json.RawMessage `json:"testIds"`
						} `json:"baseline"`
					} `json:"worktree"`
				}
				if err := json.Unmarshal(body, &wire); err != nil {
					return nil, err
				}
				rawIDs = wire.Worktree.Baseline.TestIDs
				response.Body = io.NopCloser(bytes.NewReader(body))
				return response, nil
			})
			claim, err := process.claim(ctx)
			if err != nil || claim == nil || claim.Execution.ID != id || claim.Worktree == nil || claim.Worktree.Baseline == nil {
				t.Fatalf("continue claim=%+v error=%v", claim, err)
			}
			baseline := claim.Worktree.Baseline
			if baseline.ExecutionID != first.Execution.ID || (baseline.TestIDsArtifactID == nil) != tc.countOnly {
				t.Fatalf("original baseline was not preserved: %+v", baseline)
			}
			if !reflect.DeepEqual(baseline.TestIDs, tc.wantIDs) || len(rawIDs) == 0 || (string(rawIDs) == "null") != (tc.wantIDs == nil) {
				t.Fatalf("claim IDs=%v wire=%s want=%v", baseline.TestIDs, rawIDs, tc.wantIDs)
			}
			if tc.wantIDs != nil && !reflect.DeepEqual(baseline.TestIDs, savedIDs) {
				t.Fatalf("claim IDs=%v saved artifact IDs=%v", baseline.TestIDs, savedIDs)
			}
			verifyXML := tc.verify
			if verifyXML == "" {
				// An added test increases the count. Missing IDs must still be
				// checked rather than passing from counts alone.
				verifyXML = `<testsuite><testcase classname="Z" name="重複"/><testcase name="a"/><testcase classname="Z" name="重複"/><testcase name="new"/><testcase name="disabled"><skipped/></testcase></testsuite>`
			}
			err = f.gate2Executor(t, process, &calls, func(context.Context, *Execution) error {
				gate2Write(t, filepath.Join(f.path, "cases.xml"), verifyXML)
				return nil
			}, nil, nil).Execute(ctx, *claim)
			if tc.cause == "" && err != nil || tc.cause != "" && !errors.Is(err, ErrGate2Stopped) || calls != 2 {
				t.Fatalf("Gate 2 error=%v sandbox calls=%d", err, calls)
			}
			var status, result, findings, cause string
			if err := f.db.QueryRow(`SELECT e.status,g.result,g.findings,coalesce(b.cause,'') FROM executions e JOIN gate_results g ON g.execution_id=e.id AND g.gate=2 LEFT JOIN execution_blocks b ON b.gate_result_id=g.id WHERE e.id=?`, id).Scan(&status, &result, &findings, &cause); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantResult := "succeeded", "pass"
			var wantRules []string
			if tc.countOnly {
				wantRules = append(wantRules, "count_only_check")
			}
			if tc.cause != "" {
				wantStatus, wantResult = "blocked", "blocked"
				rule := "existing_test_not_run"
				if tc.cause == model.BlockGate2Unverifiable {
					rule = "test_report_missing"
				}
				wantRules = append(wantRules, rule)
			}
			var decoded []model.Finding
			if err := json.Unmarshal([]byte(findings), &decoded); err != nil {
				t.Fatal(err)
			}
			var rules []string
			for _, finding := range decoded {
				rules = append(rules, finding.Rule)
			}
			if status != wantStatus || result != wantResult || cause != string(tc.cause) || !reflect.DeepEqual(rules, wantRules) {
				t.Fatalf("status=%s result=%s cause=%s findings=%s", status, result, cause, findings)
			}
			var baselines, verifies int
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE kind='baseline'),(SELECT count(*) FROM test_runs WHERE kind='verify' AND execution_id=?)`, id).Scan(&baselines, &verifies); err != nil || baselines != 1 || verifies != 1 {
				t.Fatalf("baseline runs=%d verify runs=%d error=%v", baselines, verifies, err)
			}
		})
	}
}

func TestBaselineIDsClaimDoesNotGrantRunnerArtifactRead(t *testing.T) {
	f := newGate2Fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := process.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	calls := 0
	if err := f.executor(t, process, &calls).Execute(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	var artifactID string
	if err := f.db.QueryRow(`SELECT test_ids_artifact_id FROM test_runs WHERE kind='baseline'`).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/api/executions/"+claim.Execution.ID+"/artifacts/"+artifactID, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+process.token)
	response, err := process.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("Runner artifact GET returned HTTP %d", response.StatusCode)
	}
}

func TestClaimCarriesFullBaselineIDsBeyondFormerResponseCap(t *testing.T) {
	ids := []string{strings.Repeat("a", 1<<20), "duplicate", "duplicate"}
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		var claim Claim
		claim.Execution.ID, claim.Execution.TaskID = "execution", "task"
		claim.Worktree = &ClaimWorktree{Baseline: &ClaimBaseline{
			TestRun: model.TestRun{Total: new(3), Skipped: new(0), TestIDsArtifactID: new("artifact")},
			TestIDs: ids,
		}}
		if err := json.NewEncoder(w).Encode(claim); err != nil {
			t.Error(err)
		}
	})
	claim, err := process.claim(context.Background())
	if err != nil || claim == nil || claim.Worktree == nil || claim.Worktree.Baseline == nil || !reflect.DeepEqual(claim.Worktree.Baseline.TestIDs, ids) {
		t.Fatalf("full baseline IDs were not delivered: %v", err)
	}
}
