package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Only the unspecified persistence transport is bridged here. No production
// endpoint or JSON shape is added for worktree/publication registration.
type pipelineRecords struct {
	mu       sync.Mutex
	prepared model.PreparedWorktree
	pushed   string
	pr       *publisher.PullRequest
}

func (r *pipelineRecords) RecordCopiedWorktree(_ context.Context, _ string, w model.PreparedWorktree) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prepared = w
	return ulid.Make().String(), nil
}
func (*pipelineRecords) WarnMissingCopy(context.Context, string, string) error     { return nil }
func (*pipelineRecords) RecordWorktreePathRejection(context.Context, string) error { return nil }
func (r *pipelineRecords) RecordPushed(_ context.Context, sha, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushed = sha
	return nil
}
func (r *pipelineRecords) RecordPR(_ context.Context, pr publisher.PullRequest, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pr = &pr
	return nil
}

type pipelineGit struct{ remote string }

func (g pipelineGit) PrepareGit(context.Context, string) (publisher.GitCommand, error) {
	return func(ctx context.Context, path, op, ref string) ([]byte, error) {
		return safegit.Output(ctx, path, op, "--", g.remote, ref)
	}, nil
}

type pipelinePRs struct{ endpoint string }

func (p pipelinePRs) FindOpen(context.Context, string, string) (*publisher.PullRequest, error) {
	return nil, nil
}
func (p pipelinePRs) Create(ctx context.Context, _, head, base, title, body string) (publisher.PullRequest, error) {
	data, _ := json.Marshal(map[string]string{"head": head, "base": base, "title": title, "body": body})
	req, _ := http.NewRequestWithContext(ctx, "POST", p.endpoint, strings.NewReader(string(data)))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return publisher.PullRequest{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 201 {
		return publisher.PullRequest{}, fmt.Errorf("PR refused")
	}
	var pr publisher.PullRequest
	err = json.NewDecoder(response.Body).Decode(&pr)
	return pr, err
}
func (pipelinePRs) Comment(context.Context, string, int, string) error {
	return errors.New("unexpected comment")
}

// The in-memory dummy remains the harness implementation. A test-only wrapper
// simulates the sandbox's committed edit and can observe the stage deadline.
type pipelineHarness struct {
	adapter.Adapter
	start func(context.Context, adapter.HarnessRequest, adapter.SandboxEnv) error
	logs  []adapter.LogLine
}

func (h pipelineHarness) Start(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := h.start(ctx, req, env); err != nil {
		return "", err
	}
	return h.Adapter.Start(ctx, req, env)
}

func (h pipelineHarness) Logs(id adapter.SessionID) (<-chan adapter.LogLine, error) {
	lines, err := h.Adapter.Logs(id)
	if err != nil {
		return nil, err
	}
	out := make(chan adapter.LogLine)
	go func() {
		defer close(out)
		for line := range lines {
			out <- line
		}
		for _, line := range h.logs {
			out <- line
		}
	}()
	return out, nil
}

type pipelineSandbox struct {
	config sandbox.ContainerConfig
	calls  *int
}

func (s pipelineSandbox) Run(ctx context.Context, spec sandbox.SandboxSpec, args []string) (sandbox.Process, error) {
	*s.calls++
	if spec.Kind != sandbox.TestSetup && spec.Kind != sandbox.TestRun || spec.ConfigDir != "" || spec.GitDir != "" || spec.Network != sandbox.NetworkNone {
		return nil, errors.New("unexpected test sandbox scope")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = spec.Worktree, []string{"PATH=/usr/bin:/bin"}, s.config.Stdout, s.config.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &baselineCommandProcess{command: cmd}, nil
}

func TestPipelineClaimThroughPushAndSucceeded(t *testing.T) {
	for _, outcome := range []string{"success", "setup_failure", "gate_failure", "missing_auth", "missing_contract", "timeout", "default_timeout"} {
		t.Run(outcome, func(t *testing.T) { runPipelineFixture(t, outcome) })
	}
}

func runPipelineFixture(t *testing.T, outcome string) {
	t.Helper()
	// Reuse the existing local bare remote HTTP fixture for clone/fetch only.
	f, _, _, _, source, remote := newCreationExecution(t)
	pushURL := gitDefenseRemote(t, remote, t.TempDir())
	project, err := safegit.Output(context.Background(), source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	profileSHA := strings.TrimSpace(string(project))
	gate2Write(t, filepath.Join(source, ".gitignore"), "report.xml\n")
	gate2Write(t, filepath.Join(source, "AGENTS.md"), "fixture instructions\n")
	gate2Git(t, source, "add", ".")
	gate2Git(t, source, "commit", "-m", "test configuration")
	gate2Git(t, remote, "fetch", source, "HEAD:refs/heads/main")
	var projectURL string
	if err := f.db.QueryRow("SELECT git_remote_url FROM projects").Scan(&projectURL); err != nil {
		t.Fatal(err)
	}
	gate2Git(t, source, "remote", "add", "origin", projectURL)
	claim := Claim{}
	claim.Execution.ID, claim.Execution.TaskID, claim.Execution.Mode, claim.Execution.Seq = logExecution, "task-002", "continue", 1
	claim.Task.Title, claim.Task.ShortName, claim.Task.AcceptanceCriteria = "Pipeline", "pipeline", "push verified commit"
	claim.Project = model.Project{ID: "project", LocalRepoPath: source, GitRemoteURL: projectURL, BaseBranch: "main", CopyAllowlist: []string{}}
	setup, command, format, report := "printf 'setup complete\\n'", `printf '%s' '<testsuite><testcase name="existing"/></testsuite>' > report.xml`, "junit", "report.xml"
	if outcome == "setup_failure" {
		setup = "exit 7"
	}
	if outcome == "gate_failure" {
		command = `if test -f changed; then exit 9; fi; ` + command
	}
	claim.Project.SetupCommand, claim.Project.TestCommand, claim.Project.TestReportFormat, claim.Project.TestReportPath = &setup, &command, &format, &report
	claim.Profile.Adapter, claim.Profile.HarnessID, claim.Profile.Version, claim.Profile.CommitSHA = "dummy", "harness", "1.0.0", profileSHA
	claim.Profile.Definition = json.RawMessage(`{"harnessVersion":"fixture-1","modelId":"fixture","contextFiles":[]}`)
	var mu sync.Mutex
	var results []ExecutionResult
	var logs []string
	var claimed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(req.URL.Path, "/heartbeat"):
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
		case strings.HasSuffix(req.URL.Path, "/claim"):
			if claimed {
				w.WriteHeader(204)
				return
			}
			claimed = true
			if err := json.NewEncoder(w).Encode(claim); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(req.URL.Path, "/result"):
			var result ExecutionResult
			if err := json.NewDecoder(req.Body).Decode(&result); err != nil {
				t.Error(err)
			}
			previous := model.ExecutionClaimed
			if len(results) > 0 {
				previous = results[len(results)-1].Status
			}
			if result.FromStatus != previous {
				t.Errorf("transition %s -> %s after %s", result.FromStatus, result.Status, previous)
			}
			results = append(results, result)
			w.WriteHeader(204)
		case strings.HasSuffix(req.URL.Path, "/context"):
			var input struct{ Project, Harness []any }
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			ids := []string{"task-revision"}
			for range len(input.Project) + len(input.Harness) {
				ids = append(ids, ulid.Make().String())
			}
			_ = json.NewEncoder(w).Encode(adapter.RequestContext{Instructions: "pinned fixture", RevisionIDs: ids})
		case strings.HasSuffix(req.URL.Path, "/events"):
			data, _ := io.ReadAll(req.Body)
			logs = append(logs, string(data))
			w.WriteHeader(204)
		case strings.HasSuffix(req.URL.Path, "/artifacts"):
			_, _ = io.Copy(io.Discard, req.Body)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request: %s", req.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	config := testConfig(t, server.URL)
	config.HarnessAuthFiles = map[string]string{"harness": filepath.Join(config.DataDir, "auth.json")}
	if outcome == "missing_auth" {
		config.HarnessAuthFiles = map[string]string{}
	}
	config.DefaultTimeouts = DefaultTimeouts{SetupSec: 10, HarnessSec: 60, TestSec: 10}
	if outcome == "timeout" {
		claim.Project.Timeouts = map[string]int{"harnessSec": 1}
	}
	if outcome == "default_timeout" {
		config.DefaultTimeouts.HarnessSec = 1
	}
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	process.pollInterval = time.Millisecond
	// Keep the fixture's parent deadline beyond the configured harness limit,
	// so the assertion observes the stage deadline rather than the test parent.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var harnessCalls, sandboxCalls int
	var prCalls atomic.Int32
	prs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		prCalls.Add(1)
		var input map[string]string
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if req.Method != "POST" || !strings.HasPrefix(input["head"], "evaloom/") || input["base"] != "main" || !strings.Contains(input["body"], "Gate 2: pass") {
			t.Errorf("invalid PR: %v", input)
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(publisher.PullRequest{Number: 1, URL: "https://github.com/fixture/repo/pull/1"})
	}))
	defer prs.Close()
	records := &pipelineRecords{}
	var adapters adapter.Registry
	if err := adapters.Register("dummy", pipelineHarness{Adapter: dummy.New(), logs: []adapter.LogLine{
		{Stream: "stdout", Text: testToken}, {Stream: "stderr", Text: "-----BEGIN PRIVATE KEY-----"},
		{Stream: "stderr", Text: "fixture-private-key-material"}, {Stream: "stderr", Text: "-----END PRIVATE KEY-----"},
	}, start: func(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) error {
		harnessCalls++
		deadline, ok := ctx.Deadline()
		wantTimeout := time.Duration(config.DefaultTimeouts.HarnessSec) * time.Second
		if override := claim.Project.Timeouts["harnessSec"]; override > 0 {
			wantTimeout = time.Duration(override) * time.Second
		}
		if !ok || time.Until(deadline) > wantTimeout || time.Until(deadline) < wantTimeout-time.Second {
			t.Error("configured harness deadline lost")
		}
		if outcome == "timeout" || outcome == "default_timeout" {
			<-ctx.Done()
			return ctx.Err()
		}
		if env.Env["GIT_CONFIG_VALUE_0"] != config.CommitAuthor.Name || env.Env["GIT_CONFIG_VALUE_1"] != config.CommitAuthor.Email {
			t.Error("configured author not passed")
		}
		gate2Write(t, filepath.Join(req.Project.Workdir, "changed"), "harness edit\n")
		gate2Git(t, req.Project.Workdir, "add", ".")
		gate2Git(t, req.Project.Workdir, "commit", "-m", "harness edit")
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	deps := PipelineDependencies{Copies: records, Audit: records, Adapters: &adapters,
		Policy: func(context.Context, Claim) (PipelinePolicy, error) {
			return PipelinePolicy{CPU: 1, MemoryBytes: 128 << 20,
				Shell: "/bin/sh", JUnitFormat: "junit", HarnessRepository: source, ConfigVariable: "CODEX_HOME"}, nil
		},
		Sandbox: func(cfg sandbox.ContainerConfig) (sandbox.Sandbox, error) {
			if cfg.RuntimePath != config.Container.RuntimePath || cfg.Image != config.Container.Image || cfg.ProxyBinary != config.Container.ProxyBinary {
				t.Error("container configuration lost")
			}
			return pipelineSandbox{config: cfg, calls: &sandboxCalls}, nil
		},
		Publication: func(context.Context, Claim) (publisher.PublishConfig, error) {
			return publisher.PublishConfig{Recorder: records, PullRequests: pipelinePRs{prs.URL}, Push: publisher.PushConfig{
				Git: pipelineGit{pushURL}, RecordRemoteMismatch: func(context.Context, string, string) error { return errors.New("unexpected remote mismatch") },
			}}, nil
		},
	}
	if outcome == "missing_contract" {
		deps.Copies = nil
	}
	var executeErr error
	err = process.Run(ctx, func(jobCtx context.Context, job Claim) {
		executeErr = process.ExecuteClaim(jobCtx, ctx, config, job, deps)
		cancel()
	}, func(err error) { t.Error(err) })
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Fatal("no result reports")
	}
	last := results[len(results)-1]
	if outcome == "success" {
		if executeErr != nil || last.Status != model.ExecutionSucceeded || last.Usage == nil || last.Usage.HarnessWallTimeMS == nil || last.Usage.HarnessUsage == nil || last.Usage.HarnessUsage.InputTokens != nil || last.Usage.HarnessUsage.OutputTokens != nil {
			var stage *StageError
			if errors.As(executeErr, &stage) {
				t.Logf("fixture stage cause: %v", stage.Err)
			}
			t.Fatalf("result=%+v err=%v", last, executeErr)
		}
		if records.pushed == "" || records.pr == nil || prCalls.Load() != 1 || harnessCalls != 1 || sandboxCalls != 3 {
			t.Fatalf("push=%s PR=%v calls=%d/%d/%d", records.pushed, records.pr, prCalls.Load(), harnessCalls, sandboxCalls)
		}
		if got := gate2Git(t, remote, "rev-parse", "refs/heads/"+records.prepared.BranchName); got != records.pushed || got != last.VerifiedSHA {
			t.Fatalf("remote=%s pushed=%s verified=%s", got, records.pushed, last.VerifiedSHA)
		}
		var statuses []model.ExecutionStatus
		for _, result := range results {
			if result.FromStatus != result.Status {
				statuses = append(statuses, result.Status)
			}
		}
		want := []model.ExecutionStatus{model.ExecutionPreparing, model.ExecutionRunning, model.ExecutionVerifying, model.ExecutionPublishing, model.ExecutionSucceeded}
		if !reflect.DeepEqual(statuses, want) {
			t.Fatalf("states=%v", statuses)
		}
		if len(logs) == 0 {
			t.Fatal("logs were not delivered")
		}
		for _, line := range logs {
			if strings.Contains(line, testToken) || strings.Contains(line, "fixture-private-key-material") {
				t.Fatal("secret reached event delivery")
			}
		}
		paths, err := filepath.Glob(filepath.Join(config.DataDir, "logs", "*.log"))
		if err != nil || len(paths) != 4 {
			t.Fatalf("log artifacts=%v err=%v", paths, err)
		}
		for _, path := range paths {
			body, err := os.ReadFile(path)
			if err != nil || strings.Contains(string(body), testToken) || strings.Contains(string(body), "fixture-private-key-material") {
				t.Fatalf("unsafe saved log: %s err=%v", path, err)
			}
		}
		return
	}
	if executeErr == nil || last.Status == model.ExecutionSucceeded || records.pushed != "" || prCalls.Load() != 0 {
		t.Fatalf("unexpected success: %+v %v", last, executeErr)
	}
	if outcome == "missing_auth" || outcome == "missing_contract" {
		if last.ErrorClass != model.ErrorConfig || harnessCalls != 0 || sandboxCalls != 0 {
			t.Fatalf("configuration failure started work: %+v", last)
		}
		return
	}
	if _, err := os.Stat(records.prepared.Path); err != nil {
		t.Fatalf("failed worktree not retained: %v", err)
	}
	if outcome == "setup_failure" && (sandboxCalls != 1 || harnessCalls != 0 || last.ErrorClass != model.ErrorConfig) {
		t.Fatalf("setup continued: %+v calls=%d/%d", last, sandboxCalls, harnessCalls)
	}
	if outcome == "gate_failure" && (sandboxCalls != 3 || harnessCalls != 1 || last.ErrorClass != model.ErrorTestFailed) {
		t.Fatalf("gate did not stop: %+v", last)
	}
	if (outcome == "timeout" || outcome == "default_timeout") && (last.Status != model.ExecutionTimedOut || last.ErrorClass != model.ErrorTimeout || sandboxCalls != 2) {
		t.Fatalf("timeout not applied: %+v", last)
	}
}
