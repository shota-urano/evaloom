package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type stageRuntimeCall struct {
	Args []string
	At   time.Time
}

// A Docker-compatible fake CLI. Only the runtime is replaced: the stage frame,
// Profile loader, workspace, channel, real Container and its supervisor run.
// Forced removal terminates the fake attach client; no daemon/VM is needed.
func TestStageRuntimeHelper(t *testing.T) {
	root := os.Getenv("STAGE_FAKE_ROOT")
	if root == "" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		os.Exit(90)
	}
	args := os.Args[index+1:]
	write := func(name string) {
		if os.WriteFile(filepath.Join(root, name), nil, 0600) != nil {
			os.Exit(91)
		}
	}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }
	file, err := os.OpenFile(filepath.Join(root, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(92)
	}
	_ = json.NewEncoder(file).Encode(stageRuntimeCall{args, time.Now()})
	_ = file.Close()
	switch args[0] + " " + args[1] {
	case "image inspect":
		fmt.Print(`{"Os":"linux","Config":{}}`)
	case "container create":
	case "container start":
		write("started")
		for !exists("removed") && !exists("exited") {
			time.Sleep(5 * time.Millisecond)
		}
	case "container kill":
		if !slices.Contains(args, "TERM") {
			os.Exit(93)
		}
		write("term")
		if os.Getenv("STAGE_FAKE_IGNORE_TERM") != "true" {
			write("exited")
		}
	case "container inspect":
		if exists("exited") {
			fmt.Print(`{"Status":"exited","Running":false,"ExitCode":143}`)
		} else {
			fmt.Print(`{"Status":"running","Running":true}`)
		}
	case "container rm":
		write("removed")
	default:
		os.Exit(94)
	}
	os.Exit(0)
}

type liveStageReporter struct{ *stageReportStub }

func (s liveStageReporter) FinishStage(ctx context.Context, id string, stage model.Stage, attempt int, outcome StageOutcome) (StageNext, error) {
	if err := ctx.Err(); err != nil {
		return StageNext{}, fmt.Errorf("cancelled final-report context: %w", err)
	}
	return s.stageReportStub.FinishStage(ctx, id, stage, attempt, outcome)
}

func TestStageTimeoutAndHeartbeatCancellationWithRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		stage      model.Stage
		ignoreTerm bool
		ownResult  bool
	}{
		{"timeout-kills-after-grace", "timeout", model.StagePrepare, true, false},
		{"heartbeat-cancels", "cancelled", model.StagePrepare, false, false},
		{"publish-reports-own-cancellation", "", model.StagePublish, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, claim, reports, services := stageFixture(t, true)
			services.Reporter = liveStageReporter{reports}
			reports.next = func(model.Stage, StageOutcome) StageNext { return StageNext{Next: "stop"} }
			if tc.want == "timeout" {
				claim.Limits.TimeoutSec["prepare"] = 1 // Same value as the short pinned Profile.
			}
			if tc.stage == model.StagePublish {
				claim.Execution.ResumeStage = stagePtr("publish")
				claim.Resume = json.RawMessage(`{"stage":"publish","completedStages":[],"checkedContentId":"checked","approvals":[]}`)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.executions = map[string]context.CancelFunc{claim.Execution.ID: cancel}
			r.cancelledExecutions = map[string]bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/heartbeat" {
					_ = json.NewEncoder(w).Encode(map[string]any{"cancelExecutionIds": []string{claim.Execution.ID}})
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			r.heartbeatURL, r.controlPlaneURL, r.client = server.URL+"/heartbeat", server.URL, server.Client()
			runtimeRoot := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cli := filepath.Join(runtimeRoot, "runtime")
			script := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestStageRuntimeHelper$ -- \"$@\"\n"
			if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			services.Executor = stageExecutorFunc(func(stageCtx context.Context, in StageInvocation) StageOutcome {
				if in.Stage != tc.stage {
					t.Fatalf("unexpected stage %s", in.Stage)
				}
				paths := services.Paths
				records := filepath.Join(runtimeRoot, "records")
				if err := os.Mkdir(records, 0700); err != nil {
					t.Fatal(err)
				}
				roots := map[string]string{paths.Profile: in.Profile.Path, paths.Workspace: in.Workspace,
					paths.Out: filepath.Join(in.Directory, "out"), paths.Inputs: filepath.Join(in.Directory, "inputs"),
					paths.Scratch: filepath.Join(in.Directory, "scratch"), paths.Records: records}
				var mounts []sandbox.Mount
				for _, target := range []string{paths.Profile, paths.Workspace, paths.Out, paths.Scratch} {
					mounts = append(mounts, sandbox.Mount{HostPath: roots[target], GuestPath: target,
						Writable: target == paths.Out || target == paths.Scratch || (tc.stage == model.StagePrepare && target == paths.Workspace)})
				}
				if tc.stage == model.StagePublish {
					mounts = append(mounts, sandbox.Mount{HostPath: roots[paths.Inputs], GuestPath: paths.Inputs})
				}
				box, err := sandbox.NewContainer(sandbox.ContainerConfig{RuntimePath: cli, Image: "fake@sha256:123",
					RuntimeEnv: map[string]string{"STAGE_FAKE_ROOT": runtimeRoot, "STAGE_FAKE_IGNORE_TERM": fmt.Sprint(tc.ignoreTerm)},
					AllowEnv:   []string{"EVALOOM_CHANNEL"},
					StagePaths: paths, StageMountRoots: roots, HostHome: filepath.Join(runtimeRoot, "home"),
					DataDir: r.dataDir, RunnerTokenFile: filepath.Join(runtimeRoot, "token"), ControlTimeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				launch := func(ctx context.Context, spec sandbox.SandboxSpec, command []string) (sandbox.Process, error) {
					if spec.Limits.Time != time.Duration(claim.Limits.TimeoutSec[string(in.Stage)])*time.Second ||
						!slices.Equal(command, []string{"bin/" + string(in.Stage)}) {
						t.Fatalf("claim limit/pinned command changed: %v %v", spec.Limits.Time, command)
					}
					p, err := box.RunControlled(ctx, spec, command)
					if err != nil {
						t.Errorf("container launch: %v", err)
						return nil, err
					}
					t.Cleanup(func() { _ = p.Kill(); _ = p.Wait() })
					if tc.want != "timeout" {
						deadline := time.Now().Add(5 * time.Second)
						for {
							if _, err := os.Stat(filepath.Join(runtimeRoot, "started")); err == nil {
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("runtime did not start")
							}
							time.Sleep(time.Millisecond)
						}
						if err := r.Heartbeat(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					return p, nil
				}
				outcome := in.RunProgram(stageCtx, launch, sandbox.SandboxSpec{Mounts: mounts, ChannelPath: in.ChannelPath,
					Env:    map[string]string{"EVALOOM_CHANNEL": "/sandbox/channel/channel.sock"},
					Limits: sandbox.ResourceLimits{CPU: 4, MemoryBytes: 8 << 30}})
				if tc.ownResult {
					// This is the executor's result-file collection boundary.
					outcome.Result = json.RawMessage(`{"contractVersion":"1","stage":"publish","status":"failed","errorClass":"cancelled","message":"cancelled"}`)
				}
				return outcome
			})
			if err := r.ExecuteStages(ctx, claim, services); err != nil {
				t.Fatal(err)
			}
			if len(reports.outcomes) != 1 || !slices.Equal(reports.started, []model.Stage{tc.stage}) {
				t.Fatalf("wrong reports: %+v", reports)
			}
			outcome := reports.outcomes[0]
			if tc.want == "" {
				if outcome.RunnerError != nil || !strings.Contains(string(outcome.Result), `"errorClass":"cancelled"`) {
					t.Fatalf("cooperative result was replaced: %+v", outcome)
				}
			} else if outcome.RunnerError == nil || *outcome.RunnerError != tc.want {
				t.Fatalf("runnerError=%s; want %s", runnerErrorValue(outcome), tc.want)
			}
			kept, err := os.ReadFile(filepath.Join(claim.Workspace.Path, "keep"))
			if err != nil || string(kept) != "workspace survives" {
				t.Fatalf("workspace was not retained: %q %v", kept, err)
			}
			if _, err := os.Stat(filepath.Join(r.dataDir, "executions", claim.Execution.ID, string(tc.stage)+"-1", "out")); err != nil {
				t.Fatalf("stage out was not retained: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(runtimeRoot, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			var term, removed time.Time
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var call stageRuntimeCall
				if err := json.Unmarshal([]byte(line), &call); err != nil {
					t.Fatal(err)
				}
				if call.Args[1] == "kill" && term.IsZero() {
					term = call.At
				}
				if call.Args[1] == "rm" && removed.IsZero() {
					removed = call.At
				}
			}
			if term.IsZero() || removed.Before(term) {
				t.Fatalf("missing TERM/forced removal sequence: %s", data)
			}
			elapsed := removed.Sub(term)
			if tc.ignoreTerm && (elapsed < 10*time.Second-200*time.Millisecond || elapsed > 12*time.Second) {
				t.Fatalf("forced removal after %v; want 10s grace", elapsed)
			}
			if !tc.ignoreTerm && elapsed >= StageCancelGrace {
				t.Fatalf("cooperative stop waited for KILL: %v", elapsed)
			}
			t.Logf("TERM to container removal: %v", elapsed)
		})
	}
}

func runnerErrorValue(outcome StageOutcome) string {
	if outcome.RunnerError == nil {
		return "<nil>"
	}
	return *outcome.RunnerError
}
