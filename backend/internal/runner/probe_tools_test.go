package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// This test-only double runs exclusively generated fixture code. It tests tool
// plumbing, NOT isolation, and is not available as a production host fallback.
type fixtureProcessDouble struct{ home string }

func (fixtureProcessDouble) Check(context.Context) error { return nil }
func (s fixtureProcessDouble) Run(ctx context.Context, env adapter.SandboxEnv, command []string) (probe.Output, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = env.Worktree
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "PYTHONDONTWRITEBYTECODE=1",
		"GIT_AUTHOR_NAME=Evaloom Test", "GIT_AUTHOR_EMAIL=evaloom-test@example.invalid",
		"GIT_COMMITTER_NAME=Evaloom Test", "GIT_COMMITTER_EMAIL=evaloom-test@example.invalid"}
	for key, value := range env.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	output := probe.Output{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		output.ExitCode = exit.ExitCode()
		err = nil
	}
	return output, err
}
func (fixtureProcessDouble) ReadFile(_ context.Context, env adapter.SandboxEnv, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(env.Worktree, name))
}

// Spec 14.3.0 smoke test: actual scripts, Git and local HTTP, sequenced through
// the production Executor and persisted through the real claim/result API.
// Gate bodies below are fixture assertions, not the security tests in 3.2-3.5
// or production Gate/Publisher implementations (those tasks remain separate).
func TestProbeToolsExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := securitytest.NewFixture(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sandbox := fixtureProcessDouble{home: fixture.Home}
	var adapters adapter.Registry
	if err := adapters.Register("probe", probe.New(sandbox)); err != nil {
		t.Fatal(err)
	}
	db, process := extensionControlPlane(t, &adapters)
	// Only change seeded INPUTS, before the production claim is made.
	if _, err := db.Exec(`UPDATE harnesses SET adapter='probe'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE projects SET local_repo_path=?,git_remote_url=?,setup_command='python3 setup.py',test_command='python3 test.py',test_report_format='junit',test_report_path='junit.xml'`, fixture.Repository, fixture.Remote); err != nil {
		t.Fatal(err)
	}
	if err := process.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := process.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	github := securitytest.NewGitHub("fixture/project", "fake-publisher-token")
	defer github.Close()
	refsBefore, err := fixture.Refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"baseSha": fixture.BaseSHA, "branchName": "evaloom/probe"}
	var called []StageID
	var baseline []string
	var verified string
	env := adapter.SandboxEnv{Worktree: fixture.Worktree}
	runScript := func(ctx context.Context, name string) error {
		out, err := sandbox.Run(ctx, env, []string{"python3", name})
		if err != nil || out.ExitCode != 0 {
			return fmt.Errorf("%s: exit=%d err=%v stdout=%s stderr=%s", name, out.ExitCode, err, out.Stdout, out.Stderr)
		}
		return nil
	}
	readTests := func() ([]string, error) {
		data, err := os.ReadFile(filepath.Join(fixture.Worktree, "junit.xml"))
		if err != nil {
			return nil, err
		}
		var report struct {
			Tests    int `xml:"tests,attr"`
			Failures int `xml:"failures,attr"`
			Errors   int `xml:"errors,attr"`
			Skipped  int `xml:"skipped,attr"`
			Cases    []struct {
				Name  string `xml:"name,attr"`
				Class string `xml:"classname,attr"`
			} `xml:"testcase"`
		}
		if err := xml.Unmarshal(data, &report); err != nil {
			return nil, err
		}
		if report.Tests != 2 || report.Failures != 0 || report.Errors != 0 || report.Skipped != 0 || len(report.Cases) != 2 {
			return nil, fmt.Errorf("invalid fixture test report: %s", data)
		}
		var ids []string
		for _, test := range report.Cases {
			ids = append(ids, test.Class+"."+test.Name)
		}
		return ids, nil
	}
	handlers := stubStageHandlers(func(id StageID, ctx context.Context, execution *Execution) error {
		called = append(called, id)
		switch id {
		case StageValidateProject:
			return (securitytest.Prerequisites{Sandbox: sandbox}).Check(ctx)
		case StageSetup:
			return runScript(ctx, "setup.py")
		case StageTestBaseline:
			if err := runScript(ctx, "test.py"); err != nil {
				return err
			}
			baseline, err = readTests()
			return err
		case StageHarness:
			h, err := adapters.Lookup(execution.Claim.Profile.Adapter)
			if err != nil {
				return err
			}
			req := adapter.HarnessRequest{ContractVersion: adapter.ContractVersion, ExecutionID: claim.Execution.ID,
				Task:    adapter.RequestTask{ID: claim.Execution.TaskID, Title: claim.Task.Title, AcceptanceCriteria: claim.Task.AcceptanceCriteria},
				Project: adapter.RequestProject{ID: claim.Project.ID, Workdir: env.Worktree, Branch: "evaloom/probe", BaseSHA: fixture.BaseSHA},
				Profile: adapter.RequestProfile{HarnessID: claim.Profile.HarnessID, Version: claim.Profile.Version, CommitSHA: claim.Profile.CommitSHA, ModelID: "test-model", Definition: claim.Profile.Definition},
				Context: adapter.RequestContext{RevisionIDs: []string{}}}
			session, err := h.Start(ctx, req, env)
			if err != nil {
				return err
			}
			timer := time.NewTicker(time.Millisecond)
			defer timer.Stop()
			for {
				status, err := h.Status(session)
				if err != nil {
					return err
				}
				if status != adapter.SessionRunning {
					break
				}
				select {
				case <-ctx.Done():
					_ = h.Cancel(session)
					return ctx.Err()
				case <-timer.C:
				}
			}
			result, err := h.Result(session)
			if err != nil {
				return err
			}
			if err := result.Validate(); err != nil {
				return err
			}
			if result.Status != adapter.ResultSucceeded {
				return fmt.Errorf("probe failed: %+v", result)
			}
			execution.HarnessResult = &result
			metadata["harnessSessionId"] = string(session)
		case StageHarnessHead:
			head, err := fixture.Git(ctx, "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			if head == fixture.BaseSHA {
				return fmt.Errorf("probe made no commit")
			}
			metadata["harnessHeadSha"] = head
		case StageGate2:
			if err := runScript(ctx, "test.py"); err != nil {
				return err
			}
			ids, err := readTests()
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(ids, baseline) {
				return fmt.Errorf("fixture test IDs changed")
			}
			verified = metadata["harnessHeadSha"].(string)
			metadata["verifiedSha"] = verified
		case StageGate3:
			head, err := fixture.Git(ctx, "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			status, err := fixture.Git(ctx, "status", "--porcelain")
			if err != nil {
				return err
			}
			if head != verified || status != "" {
				return fmt.Errorf("fixture mismatch: %s %s", head, status)
			}
		case StagePublish:
			refs, err := fixture.Refs(ctx)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(refs, refsBefore) {
				return fmt.Errorf("harness unexpectedly pushed")
			}
			if _, err := fixture.Git(ctx, "push", "origin", verified+":refs/heads/evaloom/probe"); err != nil {
				return err
			}
			refs, err = fixture.Refs(ctx)
			if err != nil {
				return err
			}
			if refs["refs/heads/evaloom/probe"] != verified {
				return fmt.Errorf("remote SHA mismatch")
			}
			metadata["pushedSha"] = verified
			body := `{"title":"Fixture change","head":"evaloom/probe","base":"main","body":"Probe execution"}`
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, github.Server.URL+"/repos/fixture/project/pulls", strings.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer fake-publisher-token")
			resp, err := github.Server.Client().Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			var pull securitytest.PullRequest
			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("fake GitHub: HTTP %d", resp.StatusCode)
			}
			if err := json.NewDecoder(resp.Body).Decode(&pull); err != nil {
				return err
			}
			metadata["prNumber"], metadata["prUrl"] = pull.Number, pull.URL
		}
		return nil
	})
	control := testExecutionControl()
	control.Timeouts = Timeouts{Setup: 10 * time.Second, Harness: 10 * time.Second, Test: 10 * time.Second}
	control.CheckCancellation = process.Heartbeat
	// Include metadata using the existing result API fields. No DB state writes.
	control.Reporter = reportFunc(func(ctx context.Context, id string, result ExecutionResult) error {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return err
		}
		for key, value := range metadata {
			payload[key] = value
		}
		data, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, process.controlPlaneURL+"/api/executions/"+id+"/result", bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+process.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := process.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("result HTTP %d", resp.StatusCode)
		}
		return nil
	})
	executor, err := NewExecutorWithControl(handlers, control)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(ctx, *claim); err != nil {
		t.Fatalf("%v: %v", err, unwrapStage(err))
	}
	if !reflect.DeepEqual(called, expectedStageIDs) {
		t.Fatalf("stages=%v", called)
	}
	var status, taskStatus, verifiedSHA, pushedSHA, session, prURL string
	var prNumber int
	if err := db.QueryRow(`SELECT e.status,t.status,e.verified_sha,e.pushed_sha,e.harness_session_id,e.pr_number,e.pr_url FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`, claim.Execution.ID).
		Scan(&status, &taskStatus, &verifiedSHA, &pushedSHA, &session, &prNumber, &prURL); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || taskStatus != "completed" || verifiedSHA != verified || pushedSHA != verified || session == "" || prNumber != 1 || prURL == "" {
		t.Fatalf("status=%s task=%s verified=%s pushed=%s session=%s PR=%d %s", status, taskStatus, verifiedSHA, pushedSHA, session, prNumber, prURL)
	}
	for _, stage := range []string{"setup", "test", "harness"} {
		data, err := os.ReadFile(filepath.Join(fixture.Worktree, stage+"-results.json"))
		if err != nil {
			t.Fatal(err)
		}
		observations, err := probe.DecodeReport(data)
		if err != nil || len(observations) != 1 || !*observations[0].Succeeded {
			t.Fatalf("%s: observations=%v err=%v", stage, observations, err)
		}
	}
	if accesses := github.Accesses(); len(accesses) != 1 || accesses[0].Method != http.MethodPost || !accesses[0].Authorized {
		t.Fatalf("accesses=%+v", accesses)
	}
}

func unwrapStage(err error) error {
	if stage, ok := err.(*StageError); ok {
		return stage.Err
	}
	return err
}
