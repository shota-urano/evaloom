package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
)

type gateAcceptanceRuntime func(*testing.T, *securitytest.Fixture, string) (probe.Sandbox, sandbox.Sandbox)

// The ordinary suite uses process doubles ONLY for generated fixture code.
// TestDockerGateAcceptance uses the same assertions with real Sandboxes.
func TestGateAcceptance(t *testing.T) {
	runGateAcceptance(t, func(t *testing.T, f *securitytest.Fixture, _ string) (probe.Sandbox, sandbox.Sandbox) {
		return fixtureProcessDouble{home: f.Home}, baselineRuntimeFunc(func(ctx context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir, cmd.Env = spec.Worktree, []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return &baselineCommandProcess{cmd}, nil
		})
	})
}

func runGateAcceptance(t *testing.T, runtime gateAcceptanceRuntime) {
	t.Helper()
	for _, tc := range []struct {
		name, cause string
		gate        int
	}{
		{"delete_test", "gate2_missing_tests", 2},
		{"describe_only", "gate2_missing_tests", 2},
		{"exclude_test", "gate2_missing_tests", 2},
		{"increase_skip", "gate2_missing_tests", 2},
		{"commit_after_gate2", "gate3_mismatch", 3},
		{"untracked_after_gate2", "gate3_mismatch", 3},
		{"implementation_only", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) { gateAcceptanceExecution(t, tc.name, tc.cause, tc.gate, runtime) })
	}
}

func gateAcceptanceExecution(t *testing.T, mode, cause string, stoppingGate int, runtime gateAcceptanceRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "evaloom-gate-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := securitytest.NewFixture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := f.Git(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Prepare trusted fixture inputs before acquiring the real baseline.
	gate2Write(t, filepath.Join(f.Worktree, "harness.py"), "exec(open('gate_harness.py').read())\n")
	git("add", "harness.py")
	git("commit", "-m", "Prepare Gate acceptance fixture")
	f.BaseSHA = git("rev-parse", "HEAD")
	git("push", "origin", "HEAD:refs/heads/main")
	common := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	provider, testSandbox := runtime(t, f, common)
	if err := (securitytest.Prerequisites{Sandbox: provider}).Check(ctx); err != nil {
		t.Fatal(err)
	}
	const branch = "evaloom/probe"
	remoteURL := gitDefenseRemote(t, f.Remote, f.Home)
	httpFixture := newBaselineHTTPFixture(t)
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE projects SET local_repo_path=?,git_remote_url=?,test_command='python3 gate_test.py',test_report_path='junit.xml'`, []any{f.Repository, remoteURL}},
		{`UPDATE worktrees SET path=?,base_sha=?,branch_name=?`, []any{f.Worktree, f.BaseSHA, branch}},
	} {
		if _, err := httpFixture.db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(httpFixture.config)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := r.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	refsBefore, err := f.Refs(ctx)
	if err != nil || len(refsBefore) != 1 || refsBefore["refs/heads/main"] != f.BaseSHA {
		t.Fatalf("initial refs=%v err=%v", refsBefore, err)
	}
	github := securitytest.NewGitHub("fixture/project", "fixture-token")
	defer github.Close()
	publication := &gateAcceptancePublication{db: httpFixture.db, executionID: claim.Execution.ID}
	publishCalls, testCalls := 0, 0
	var harnessHead, afterHead string
	var checkedPRCreates atomic.Int32
	// Read committed state at the receiving HTTP boundary, before the fake API
	// creates a PR and before the final result report can save publication data.
	githubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/repos/fixture/project/pulls" {
			var status, savedBranch string
			var verified, pushed sql.NullString
			err := httpFixture.db.QueryRowContext(req.Context(),
				`SELECT status,branch_name,verified_sha,pushed_sha FROM executions WHERE id=?`, claim.Execution.ID).
				Scan(&status, &savedBranch, &verified, &pushed)
			if err != nil || status != "publishing" || savedBranch != branch || !verified.Valid || verified.String == "" ||
				!pushed.Valid || pushed.String != verified.String {
				t.Errorf("PR creation before durable push record: status=%s branch=%s verified=%v pushed=%v err=%v", status, savedBranch, verified, pushed, err)
				http.Error(w, "push record not committed", http.StatusPreconditionFailed)
				return
			}
			checkedPRCreates.Add(1)
		}
		github.Server.Config.Handler.ServeHTTP(w, req)
	}))
	defer githubAPI.Close()
	handlers := stubStageHandlers(func(id StageID, ctx context.Context, e *Execution) error {
		switch id {
		case StagePrepareWorktree:
			e.WorktreeID, e.WorktreePath, e.BaseSHA = httpFixture.worktreeID, f.Worktree, f.BaseSHA
		case StageHarness:
			data, err := os.ReadFile("../adapter/testdata/harness_request.json")
			if err != nil {
				return err
			}
			var request adapter.HarnessRequest
			if err := json.Unmarshal(data, &request); err != nil {
				return err
			}
			request.ExecutionID, request.Project.Workdir, request.Project.BaseSHA = claim.Execution.ID, f.Worktree, f.BaseSHA
			h := probe.New(provider)
			session, err := h.Start(ctx, request, adapter.SandboxEnv{Worktree: f.Worktree, Env: map[string]string{"EVALOOM_GATE_CASE": mode}})
			if err != nil {
				return err
			}
			defer func() { _ = h.Cancel(session) }()
			logs, err := h.Logs(session)
			if err != nil {
				return err
			}
			var diagnostic strings.Builder
			for line := range logs {
				diagnostic.WriteString(line.Text)
			}
			result, err := h.Result(session)
			if err != nil || result.Status != adapter.ResultSucceeded {
				return fmt.Errorf("probe result=%+v err=%v logs=%s", result, err, diagnostic.String())
			}
			e.HarnessResult = &result
			harnessHead = git("rev-parse", "HEAD")
			if harnessHead == f.BaseSHA {
				return fmt.Errorf("probe made no commit")
			}
			data, err = os.ReadFile(filepath.Join(f.Worktree, "harness-results.json"))
			if err != nil {
				return err
			}
			observations, err := probe.DecodeReport(data)
			if err != nil || len(observations) != 1 || observations[0].ID != "fixture-file" || !*observations[0].Succeeded {
				return fmt.Errorf("probe observations=%s err=%v", data, err)
			}
		case StagePublish:
			publishCalls++
			refs, err := f.Refs(ctx)
			if err != nil || !reflect.DeepEqual(refs, refsBefore) {
				return fmt.Errorf("refs changed before Publisher: %v err=%v", refs, err)
			}
			_, err = publisher.Publish(ctx, publisher.Target{Worktree: f.Worktree, VerifiedSHA: e.VerifiedSHA, BranchName: branch}, publisher.PublishConfig{
				Push: publisher.PushConfig{RepositoryURL: remoteURL, CheckCancellation: r.Heartbeat,
					RecordRemoteMismatch: func(context.Context, string, string) error { return fmt.Errorf("unexpected remote mismatch") }},
				BaseBranch: "main", PullRequests: gateAcceptancePulls{githubAPI}, Recorder: publication, ReportContext: ctx,
				Report: publisher.Report{ExecutionID: claim.Execution.ID, TaskID: claim.Execution.TaskID, Title: claim.Task.Title,
					AcceptanceCriteria: claim.Task.AcceptanceCriteria, Summary: e.HarnessResult.Summary,
					HarnessID: claim.Profile.HarnessID, HarnessVersion: claim.Profile.Version,
					ProfileVersion: claim.Profile.Version, ProfileCommitSHA: claim.Profile.CommitSHA,
					Gate2Result: "pass", Gate3Result: "pass", VerifyRun: *e.VerifyRun},
			})
			return err
		}
		return nil
	})
	control := testExecutionControl()
	control.Timeouts = Timeouts{Setup: time.Minute, Harness: time.Minute, Test: time.Minute}
	control.CheckCancellation = r.Heartbeat
	// The existing result API accepts publication fields on a transition only.
	// Retain the real final report as well as the Recorder's earlier DB write;
	// the fake PR receiver independently checks that the final report is too late.
	control.Reporter = reportFunc(func(ctx context.Context, id string, result ExecutionResult) error {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return err
		}
		payload["branchName"] = branch
		if publication.sha != "" {
			payload["pushedSha"] = publication.sha
		}
		if publication.pr != nil {
			payload["prNumber"], payload["prUrl"] = publication.pr.Number, publication.pr.URL
		}
		data, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.controlPlaneURL+"/api/executions/"+id+"/result", bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+r.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := r.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("result HTTP %d", resp.StatusCode)
		}
		return nil
	})
	// Count actual Sandbox executions and retain the real baseline/Gate bodies.
	counted := baselineRuntimeFunc(func(ctx context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
		testCalls++
		return testSandbox.Run(ctx, spec, argv)
	})
	baselineConfig := BaselineConfig{Sandbox: counted, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 256 << 20, Time: time.Minute}, Shell: "/bin/sh", JUnitFormat: "junit"}
	executor, err := r.NewVerificationExecutor(handlers, control, Gate2Config{baselineConfig}, Gate3Config{})
	if err != nil {
		t.Fatal(err)
	}
	// A test-only stage hook injects changes strictly AFTER successful Gate 2
	// (including report parsing), BEFORE the production Gate 3 handler.
	for i := range executor.stages {
		if executor.stages[i].ID != StageGate2 {
			continue
		}
		verify := executor.stages[i].run
		executor.stages[i].run = func(ctx context.Context, e *Execution) error {
			if err := verify(ctx, e); err != nil {
				return err
			}
			if mode == "commit_after_gate2" {
				git("commit", "--allow-empty", "-m", "Commit after verification")
			}
			if mode == "untracked_after_gate2" {
				gate2Write(t, filepath.Join(f.Worktree, "after-verification.txt"), "untracked\n")
			}
			afterHead = git("rev-parse", "HEAD")
			return nil
		}
	}
	err = executor.Execute(ctx, *claim)
	if cause == "" && err != nil || stoppingGate == 2 && !errors.Is(err, ErrGate2Stopped) || stoppingGate == 3 && !errors.Is(err, ErrGate3Stopped) {
		t.Fatalf("execution: %v: %v", err, unwrapStage(err))
	}
	var status, taskStatus, savedHead string
	var verified, pushed, prURL sql.NullString
	var prNumber sql.NullInt64
	if err := httpFixture.db.QueryRow(`SELECT e.status,t.status,e.harness_head_sha,e.verified_sha,e.pushed_sha,e.pr_number,e.pr_url
		FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`, claim.Execution.ID).
		Scan(&status, &taskStatus, &savedHead, &verified, &pushed, &prNumber, &prURL); err != nil {
		t.Fatal(err)
	}
	if savedHead != harnessHead || testCalls != 2 {
		t.Fatalf("saved harness=%s actual=%s Sandbox test calls=%d", savedHead, harnessHead, testCalls)
	}
	if mode == "implementation_only" && git("diff", "--name-only", f.BaseSHA+".."+harnessHead) != "gate-impl.cjs" {
		t.Fatal("implementation-only probe changed another file")
	}
	if mode == "untracked_after_gate2" && (afterHead != harnessHead || git("status", "--porcelain") != "?? after-verification.txt") {
		t.Fatal("untracked-file case changed HEAD or did not leave exactly the injected file")
	}
	readIDs := func(kind string) []string {
		t.Helper()
		var path string
		if err := httpFixture.db.QueryRow(`SELECT a.storage_path FROM test_runs t JOIN artifacts a ON a.id=t.test_ids_artifact_id
			WHERE t.execution_id=? AND t.kind=? AND a.execution_id=t.execution_id AND a.kind='test_ids'`, claim.Execution.ID, kind).Scan(&path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(httpFixture.dataDir, path))
		var ids []string
		if err != nil || json.Unmarshal(data, &ids) != nil {
			t.Fatalf("%s test IDs=%s err=%v", kind, data, err)
		}
		return ids
	}
	baseIDs, verifyIDs := readIDs("baseline"), readIDs("verify")
	// Node 20 uses classname="test"; newer Node uses the suite's name.
	// Both obey spec 10's classname.name convention. Compare the actual saved
	// baseline IDs, while independently checking the two fixture test names.
	if len(baseIDs) != 2 || !strings.HasSuffix(baseIDs[0], ".adds positive numbers") || !strings.HasSuffix(baseIDs[1], ".adds negative numbers") {
		t.Fatalf("baseline IDs=%v", baseIDs)
	}
	firstID, secondID := baseIDs[0], baseIDs[1]
	if mode == "describe_only" {
		foundNew := false
		for _, id := range verifyIDs {
			if id == firstID || id == secondID {
				t.Fatalf("describe.only still executed existing test: %v", verifyIDs)
			}
			foundNew = foundNew || strings.HasSuffix(id, ".focused")
		}
		if !foundNew {
			t.Fatalf("focused test was not executed: %v", verifyIDs)
		}
	} else {
		wantIDs := baseIDs
		if stoppingGate == 2 {
			wantIDs = []string{firstID}
		}
		if !reflect.DeepEqual(verifyIDs, wantIDs) {
			t.Fatalf("verify IDs=%v want=%v", verifyIDs, wantIDs)
		}
	}
	// Confirm that the baseline and verify measurements were actually persisted,
	// and that the fixture change produced the intended execution/skip counts.
	var baseTotal, basePassed, baseSkipped, total, passed, skipped, exit int
	var target string
	if err := httpFixture.db.QueryRow(`SELECT total,passed,skipped FROM test_runs WHERE execution_id=? AND kind='baseline' AND target_sha=?`, claim.Execution.ID, f.BaseSHA).
		Scan(&baseTotal, &basePassed, &baseSkipped); err != nil {
		t.Fatal(err)
	}
	if err := httpFixture.db.QueryRow(`SELECT target_sha,total,passed,skipped,exit_code FROM test_runs WHERE execution_id=? AND kind='verify'`, claim.Execution.ID).
		Scan(&target, &total, &passed, &skipped, &exit); err != nil {
		t.Fatal(err)
	}
	if baseTotal != 2 || basePassed != 2 || baseSkipped != 0 || target != harnessHead || exit != 0 {
		t.Fatalf("baseline=%d/%d skip=%d verify SHA=%s exit=%d", basePassed, baseTotal, baseSkipped, target, exit)
	}
	wantPassed, wantSkipped := 2, 0
	if stoppingGate == 2 {
		wantPassed = 1
	}
	if mode == "increase_skip" {
		wantSkipped = 1
	}
	// Node versions differ in how non-focused files appear in JUnit. The exact
	// missing existing IDs and the executed new ID are asserted above instead.
	if mode != "describe_only" && (passed != wantPassed || skipped != wantSkipped) || passed <= 0 {
		t.Fatalf("verify total=%d passed=%d skipped=%d want passed=%d skipped=%d", total, passed, skipped, wantPassed, wantSkipped)
	}
	refsAfter, err := f.Refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gates, blocks, notifications int
	if cause != "" {
		var savedCause, blockHead, findings string
		if err := httpFixture.db.QueryRow(`SELECT b.cause,b.head_sha,g.findings FROM execution_blocks b JOIN gate_results g ON g.id=b.gate_result_id
			WHERE b.execution_id=? AND g.execution_id=b.execution_id AND g.gate=? AND g.result='blocked'`, claim.Execution.ID, stoppingGate).
			Scan(&savedCause, &blockHead, &findings); err != nil {
			t.Fatal(err)
		}
		wantHead, rule := harnessHead, "existing_test_not_run"
		if stoppingGate == 3 {
			wantHead, rule = afterHead, "changed_after_test"
		}
		if status != "blocked" || taskStatus != "blocked" || savedCause != cause || blockHead != wantHead || !strings.Contains(findings, `"rule":"`+rule+`"`) ||
			publishCalls != 0 || pushed.Valid || prNumber.Valid || prURL.Valid || !reflect.DeepEqual(refsBefore, refsAfter) || len(github.Accesses()) != 0 || checkedPRCreates.Load() != 0 {
			t.Fatalf("status=%s task=%s cause=%s block head=%s want=%s findings=%s publish=%d pushed=%v PR=%v/%v refs before=%v after=%v GitHub=%v",
				status, taskStatus, savedCause, blockHead, wantHead, findings, publishCalls, pushed, prNumber, prURL, refsBefore, refsAfter, github.Accesses())
		}
		if stoppingGate == 2 && verified.Valid || stoppingGate == 3 && verified.String != harnessHead || mode == "commit_after_gate2" && afterHead == harnessHead {
			t.Fatalf("verified=%v harness=%s after Gate 2=%s", verified, harnessHead, afterHead)
		}
		if err := httpFixture.db.QueryRow(`SELECT count(*) FROM notification_requests WHERE execution_id=? AND status='blocked'`, claim.Execution.ID).Scan(&notifications); err != nil || notifications != 1 {
			t.Fatalf("blocked notification requests=%d err=%v", notifications, err)
		}
	} else {
		wantRefs := map[string]string{"refs/heads/main": f.BaseSHA, "refs/heads/" + branch: verified.String}
		if status != "succeeded" || taskStatus != "completed" || verified.String != harnessHead || pushed.String != verified.String || !prNumber.Valid || prNumber.Int64 != 1 || prURL.String == "" ||
			publishCalls != 1 || !reflect.DeepEqual(refsAfter, wantRefs) {
			t.Fatalf("status=%s task=%s verified=%v harness=%s pushed=%v PR=%v/%v calls=%d refs=%v", status, taskStatus, verified, harnessHead, pushed, prNumber, prURL, publishCalls, refsAfter)
		}
		accesses := github.Accesses()
		if checkedPRCreates.Load() != 1 {
			t.Fatalf("PR creates with committed pushed_sha=verified_sha: %d want=1", checkedPRCreates.Load())
		}
		if len(accesses) != 2 || accesses[0].Method != "GET" || accesses[1].Method != "POST" || !accesses[0].Authorized || !accesses[1].Authorized {
			t.Fatalf("GitHub accesses=%+v", accesses)
		}
		var input struct{ Head, Base, Body string }
		if err := json.Unmarshal(accesses[1].Body, &input); err != nil || input.Head != branch || input.Base != "main" || !strings.Contains(input.Body, verified.String) {
			t.Fatalf("PR input=%+v err=%v", input, err)
		}
		if err := httpFixture.db.QueryRow(`SELECT count(*) FROM execution_blocks WHERE execution_id=?`, claim.Execution.ID).Scan(&blocks); err != nil || blocks != 0 {
			t.Fatalf("success blocks=%d err=%v", blocks, err)
		}
	}
	wantGates := 2
	if stoppingGate == 2 {
		wantGates = 1
	}
	if err := httpFixture.db.QueryRow(`SELECT count(*) FROM gate_results WHERE execution_id=?`, claim.Execution.ID).Scan(&gates); err != nil || gates != wantGates {
		t.Fatalf("gate results=%d want=%d err=%v", gates, wantGates, err)
	}
	if stoppingGate != 2 {
		var gate2Result string
		if err := httpFixture.db.QueryRow(`SELECT result FROM gate_results WHERE execution_id=? AND gate=2`, claim.Execution.ID).Scan(&gate2Result); err != nil || gate2Result != "pass" {
			t.Fatalf("Gate 2=%s err=%v", gate2Result, err)
		}
	}
	t.Logf("status=%s cause=%s baseline=%d verify passed=%d skip=%d; remote refs checked; verified=%s pushed=%s", status, cause, baseTotal, passed, skipped, verified.String, pushed.String)
}

// Spec 11's existing persistence boundary has no mid-publish result API. This
// test Recorder commits pushed_sha directly to the fixture's existing DB before
// acknowledging success, and retains metadata for the final result transition.
type gateAcceptancePublication struct {
	db          *sql.DB
	executionID string
	sha         string
	pr          *publisher.PullRequest
}

func (p *gateAcceptancePublication) RecordPushed(ctx context.Context, sha, branch string) error {
	// ExecContext outside a transaction returns only after SQLite autocommits.
	result, err := p.db.ExecContext(ctx, `UPDATE executions SET pushed_sha=?,branch_name=? WHERE id=?`, sha, branch, p.executionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("push record updated %d executions, want 1", rows)
	}
	p.sha = sha
	return nil
}
func (p *gateAcceptancePublication) RecordPR(_ context.Context, pr publisher.PullRequest, _ bool) error {
	p.pr = &pr
	return nil
}

// Fake GitHub's HTTP surface is deliberately local. Production Publish still
// owns push, lookup/create ordering and PR report construction.
type gateAcceptancePulls struct{ server *httptest.Server }

func (g gateAcceptancePulls) request(ctx context.Context, method, path string, input, output any) error {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, g.server.URL+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer fixture-token")
	resp, err := g.server.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("fake GitHub HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(output)
}
func (g gateAcceptancePulls) FindOpen(ctx context.Context, _, _ string) (*publisher.PullRequest, error) {
	var pulls []securitytest.PullRequest
	if err := g.request(ctx, "GET", "/repos/fixture/project/pulls?state=open", nil, &pulls); err != nil {
		return nil, err
	}
	if len(pulls) != 0 {
		return nil, fmt.Errorf("unexpected preexisting PR")
	}
	return nil, nil
}
func (g gateAcceptancePulls) Create(ctx context.Context, _, head, base, title, body string) (publisher.PullRequest, error) {
	var pull securitytest.PullRequest
	err := g.request(ctx, "POST", "/repos/fixture/project/pulls", map[string]string{"head": head, "base": base, "title": title, "body": body}, &pull)
	return publisher.PullRequest{Number: pull.Number, URL: pull.URL}, err
}
func (gateAcceptancePulls) Comment(context.Context, string, int, string) error {
	return fmt.Errorf("unexpected PR comment")
}

var _ publisher.PublicationRecorder = (*gateAcceptancePublication)(nil)
var _ publisher.PullRequests = gateAcceptancePulls{}
