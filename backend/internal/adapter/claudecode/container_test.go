package claudecode

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type runtimeCall struct {
	Args []string
	At   time.Time
}

// This executable models the runtime's inspected workload state separately from
// the exit status of its attach client. It never invokes Docker or real ClaudeCode.
func TestClaudeCodeRuntimeHelper(t *testing.T) {
	root := os.Getenv("CLAUDE_RUNTIME_ROOT")
	if root == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	mode := os.Getenv("CLAUDE_RUNTIME_MODE")
	refresh := strings.HasPrefix(mode, "refresh-")
	mode = strings.TrimPrefix(mode, "refresh-")
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			os.Exit(91)
		}
	}
	data, _ := json.Marshal(runtimeCall{Args: args, At: time.Now()})
	log, err := os.OpenFile(filepath.Join(root, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(92)
	}
	_, _ = log.Write(append(data, '\n'))
	_ = log.Close()
	if args[0] == "image" {
		if mode == "prepare-cancel" {
			write("preparing", "yes")
			time.Sleep(time.Hour)
		}
		fmt.Println(`{"Os":"linux","Config":{}}`)
		os.Exit(0)
	}
	if args[0] == "volume" {
		name := args[len(args)-1]
		switch args[1] {
		case "create":
			if mode == "volume-failure" {
				os.Exit(95)
			}
			write(name+".volume", "created")
		case "rm":
			_ = os.Remove(filepath.Join(root, name+".volume"))
		default:
			os.Exit(96)
		}
		os.Exit(0)
	}
	if args[1] == "cp" {
		if args[2] != "-" {
			if mode == "readback-corrupt" {
				os.Exit(0)
			}
			if mode == "readback-failure" {
				os.Exit(107)
			}
			data, err := os.ReadFile(filepath.Join(root, "configuration.tar"))
			if err != nil {
				os.Exit(106)
			}
			if strings.Contains(args[2], ":/evaloom-auth/") {
				if mode == "writeback-delay-failure" {
					time.Sleep(1200 * time.Millisecond)
					os.Exit(110)
				}
				tr := tar.NewReader(bytes.NewReader(data))
				tw := tar.NewWriter(os.Stdout)
				for {
					h, err := tr.Next()
					if err != nil {
						break
					}
					if h.Name == ".credentials.json" {
						_ = tw.WriteHeader(h)
						_, _ = io.Copy(tw, tr)
						break
					}
				}
				_ = tw.Close()
			} else {
				_, _ = os.Stdout.Write(data)
			}
			os.Exit(0)
		}
		if mode == "copy-failure" {
			os.Exit(97)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(98)
		}
		write("configuration.tar", string(data))
		os.Exit(0)
	}
	name := args[len(args)-1]
	if i := slices.Index(args, "--name"); i >= 0 {
		name = args[i+1]
	}
	phase, _ := os.ReadFile(filepath.Join(root, name+".phase"))
	code := 0
	if string(phase) == "exec" && strings.HasPrefix(mode, "exit-") {
		code, _ = strconv.Atoi(strings.TrimPrefix(mode, "exit-"))
	}
	if mode == "config-130" {
		code = 130
	}
	switch args[1] {
	case "create":
		phase := "exec"
		if slices.Contains(args, "auth") {
			phase = "preflight"
		}
		write(name+".phase", phase)
	case "start":
		if refresh {
			archive, err := os.ReadFile(filepath.Join(root, "configuration.tar"))
			if err != nil {
				os.Exit(108)
			}
			tr := tar.NewReader(bytes.NewReader(archive))
			var updated bytes.Buffer
			tw := tar.NewWriter(&updated)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					os.Exit(109)
				}
				data, _ := io.ReadAll(tr)
				if h.Name == ".credentials.json" {
					data = []byte(`{"token":"private-volume-refreshed"}`)
					h.Size = int64(len(data))
				}
				_ = tw.WriteHeader(h)
				_, _ = tw.Write(data)
			}
			_ = tw.Close()
			write("configuration.tar", updated.String())
		}

		if string(phase) == "exec" || mode == "preflight-term" {
			if mode == "term" || mode == "ignore-term" || mode == "preflight-term" {
				term := make(chan os.Signal, 1)
				signal.Notify(term, syscall.SIGTERM)
				write(name+".pid", strconv.Itoa(os.Getpid()))
				write("started", name)
				fmt.Println(`{"type":"system"}`)
				<-term
				write("term-received", "yes")
				if mode == "ignore-term" {
					for {
						time.Sleep(time.Hour)
					}
				}
				// Leave time for a competing context-triggered rm to be observed.
				time.Sleep(120 * time.Millisecond)
			}
			if mode == "multiple-results" {
				fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result_index":0,"result":"intermediate","usage":{"input_tokens":7,"output_tokens":4}}`)
				fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result_index":1,"result":"final summary","usage":{"input_tokens":17,"output_tokens":9}}`)
			} else {
				fmt.Println(`{"type":"result","subtype":"success","is_error":false}`)
			}
		}
		write(name+".exited", "yes")
	case "inspect":
		_, err := os.Stat(filepath.Join(root, name+".exited"))
		runtimeError := ""
		if mode == "runtime-error" && string(phase) == "exec" {
			runtimeError = "runtime failure"
		}
		fmt.Printf(`{"Status":"exited","Running":%t,"ExitCode":%d,"OOMKilled":%t,"Error":%q}`,
			err != nil, code, mode == "oom" && string(phase) == "exec", runtimeError)
	case "kill", "rm":
		pidData, _ := os.ReadFile(filepath.Join(root, name+".pid"))
		pid, _ := strconv.Atoi(string(pidData))
		if pid > 0 {
			sig := syscall.SIGTERM
			if args[1] == "rm" {
				sig = syscall.SIGKILL
			}
			_ = syscall.Kill(pid, sig)
		}
	default:
		os.Exit(93)
	}
	os.Exit(0)
}

func useFakeContainerRuntime(t *testing.T, f *fixture, mode string) string {
	t.Helper()
	root := filepath.Dir(f.env.Worktree)
	for _, name := range []string{"runtime-state", "personal-home", "data", "git"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(root, "runtime-state")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(root, "runtime")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run='^TestClaudeCodeRuntimeHelper$' -- \"$@\"\n"
	if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := sandbox.ContainerConfig{RuntimePath: runtime, Image: "test-claude-image",
		RuntimeEnv: map[string]string{"CLAUDE_RUNTIME_ROOT": state, "CLAUDE_RUNTIME_MODE": mode, "GORACE": "atexit_sleep_ms=0"}, AllowEnv: []string{"CLAUDE_CONFIG_DIR"},
		WorktreeRoot: root, GitDirRoot: filepath.Join(root, "git"), ConfigDirRoot: root, HostHome: filepath.Join(root, "personal-home"),
		DataDir: filepath.Join(root, "data"), RunnerTokenFile: filepath.Join(root, "data", "token"), ControlTimeout: 3 * time.Second}
	f.config.Launch = ContainerLaunch(config, func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, ConfigDir: env.ConfigDir, GitDir: config.GitDirRoot,
			Env: env.Env, Network: sandbox.NetworkNone, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 128 << 20, Time: 15 * time.Second}}, nil
	})
	f.config.CancelGrace = 600 * time.Millisecond
	f.h = New(f.config)
	return state
}

func runtimeCalls(t *testing.T, root string) []runtimeCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	var calls []runtimeCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call runtimeCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func TestContainerLaunchErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want adapter.ErrorClass
	}{
		{"exit-128", adapter.ErrorExecutionFailed}, {"exit-130", adapter.ErrorExecutionFailed},
		{"exit-137", adapter.ErrorExecutionFailed}, {"exit-255", adapter.ErrorExecutionFailed}, {"config-130", adapter.ErrorConfig},
		{"oom", adapter.ErrorSandbox}, {"runtime-error", adapter.ErrorSandbox},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFixture(t, "success")
			root := useFakeContainerRuntime(t, f, tc.mode)
			_, result, _ := runResult(t, f)
			if result.Status != adapter.ResultFailed || result.ErrorClass == nil || *result.ErrorClass != tc.want {
				t.Fatalf("result=%+v, want errorClass=%s", result, tc.want)
			}
			var starts int
			for _, call := range runtimeCalls(t, root) {
				if slices.Equal(call.Args[:2], []string{"container", "start"}) {
					starts++
				}
			}
			want := 2
			if tc.mode == "config-130" {
				want = 1
			}
			if starts != want {
				t.Fatalf("started %d commands, want %d", starts, want)
			}
		})
	}
}

func TestContainerLaunchMultipleResults(t *testing.T) {
	f := newFixture(t, "success")
	useFakeContainerRuntime(t, f, "multiple-results")
	_, result, logs := runResult(t, f)
	if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil || result.Summary != "final summary" ||
		result.Usage.InputTokens == nil || *result.Usage.InputTokens != 17 || result.Usage.OutputTokens == nil || *result.Usage.OutputTokens != 9 {
		t.Fatalf("result=%+v logs=%+v", result, logs)
	}
}

func TestContainerLaunchCancellationSequence(t *testing.T) {
	for _, tc := range []struct {
		mode, trigger string
	}{
		{"term", "adapter"}, {"ignore-term", "adapter"}, {"prepare-cancel", "adapter"},
		{"preflight-term", "adapter"}, {"term", "context"},
		{"term", "deadline"}, {"ignore-term", "deadline"},
	} {
		t.Run(tc.mode+"/"+tc.trigger, func(t *testing.T) {
			mode := tc.mode
			f := newFixture(t, "success")
			root := useFakeContainerRuntime(t, f, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tc.trigger == "deadline" {
				deadlineCtx, deadlineCancel := context.WithTimeout(ctx, time.Second)
				defer deadlineCancel()
				ctx = deadlineCtx
			}
			id, err := f.h.Start(ctx, f.request, f.env)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.h.Cancel(id); _, _ = f.h.Result(id) })
			marker := "started"
			if mode == "prepare-cancel" {
				marker = "preparing"
			}
			for {
				if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("runtime did not reach " + marker)
				}
				time.Sleep(time.Millisecond)
			}
			cancelledAt := time.Now()
			switch tc.trigger {
			case "adapter":
				if err := f.h.Cancel(id); err != nil {
					t.Fatal(err)
				}
			case "context":
				cancel()
			case "deadline":
				cancelledAt, _ = ctx.Deadline()
				<-ctx.Done()
			}
			r, err := f.h.Result(id)
			want := adapter.ErrorCancelled
			if tc.trigger == "deadline" {
				want = adapter.ErrorTimeout
			}
			if err != nil || r.Status != adapter.ResultFailed || r.ErrorClass == nil || *r.ErrorClass != want {
				t.Fatalf("result=%+v err=%v", r, err)
			}
			calls := runtimeCalls(t, root)
			if mode == "prepare-cancel" {
				if time.Since(cancelledAt) >= f.config.CancelGrace {
					t.Fatal("preparation cancellation waited for grace")
				}
				for _, call := range calls {
					if call.Args[0] == "container" {
						t.Fatalf("container launched after preparation cancellation: %v", call)
					}
				}
				return
			}
			name, _ := os.ReadFile(filepath.Join(root, "started"))
			var term, removed time.Time
			for _, call := range calls {
				if call.Args[len(call.Args)-1] != string(name) {
					continue
				}
				if call.Args[1] == "kill" {
					if !slices.Contains(call.Args, "TERM") {
						t.Fatalf("unexpected signal: %v", call.Args)
					}
					term = call.At
				}
				if call.Args[1] == "rm" {
					removed = call.At
				}
			}
			if term.IsZero() || removed.Before(term) {
				t.Fatalf("TERM/removal order incorrect: %v", calls)
			}
			if _, err := os.Stat(filepath.Join(root, "term-received")); err != nil {
				t.Fatal("workload did not receive TERM")
			}
			if mode == "ignore-term" {
				if removed.Sub(cancelledAt) < f.config.CancelGrace {
					t.Fatalf("forced removal before grace: %v", removed.Sub(cancelledAt))
				}
			} else {
				if _, err := os.Stat(filepath.Join(root, string(name)+".exited")); err != nil {
					t.Fatal("removed before workload completed TERM handling")
				}
				if removed.Sub(cancelledAt) >= f.config.CancelGrace {
					t.Fatal("cooperative workload waited for forced removal")
				}
			}
			pidData, _ := os.ReadFile(filepath.Join(root, string(name)+".pid"))
			pid, _ := strconv.Atoi(string(pidData))
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("runtime process %d survived Wait: %v", pid, err)
			}
			if _, err := os.Stat(f.env.Worktree); err != nil {
				t.Fatal("worktree not retained")
			}
		})
	}
}

func TestContainerLaunchUsesPreparedHarnessSandbox(t *testing.T) {
	f := newFixture(t, "success")
	root := filepath.Dir(f.env.Worktree)
	paths := map[string]string{}
	for _, name := range []string{"personal-home", "data", "git", "config-root"} {
		paths[name] = filepath.Join(root, name)
		if err := os.Mkdir(paths[name], 0700); err != nil {
			t.Fatal(err)
		}
	}
	capture := filepath.Join(root, "runtime-args")
	runtime := filepath.Join(root, "runtime")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$CAPTURE"
case "$1:$2" in
  image:inspect) printf '%s\n' '{"Os":"linux","Config":{}}' ;;
  volume:create|volume:rm) exit 0 ;;
  container:cp) if [ "$3" = - ]; then cat > "$CAPTURE.tar"; else cat "$CAPTURE.tar"; fi ;;
  container:create) exit 0 ;;
  container:start) printf '%s\n' '{"type":"result","subtype":"success","is_error":false}' ;;
  container:inspect) printf '%s\n' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
  container:rm) exit 0 ;;
  *) exit 19 ;;
esac
`
	if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := sandbox.ContainerConfig{RuntimePath: runtime, Image: "test-claude-image", RuntimeEnv: map[string]string{"CAPTURE": capture}, AllowEnv: []string{"CLAUDE_CONFIG_DIR", "PATH"}, WorktreeRoot: root, GitDirRoot: paths["git"], ConfigDirRoot: root, HostHome: paths["personal-home"], DataDir: paths["data"], RunnerTokenFile: filepath.Join(paths["data"], "token"), ControlTimeout: time.Second}
	scope := func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, ConfigDir: env.ConfigDir, GitDir: paths["git"], Env: env.Env, Network: sandbox.NetworkNone, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 128 << 20, Time: time.Second}}, nil
	}
	f.config.Launch = ContainerLaunch(config, scope)
	f.h = New(f.config)
	_, result, _ := runResult(t, f)
	if result.Status != adapter.ResultSucceeded {
		t.Fatalf("result=%+v", result)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(data), "\n")
	for _, want := range []string{"--read-only", "--network", "none", "--workdir", f.env.Worktree, "HOME=/evaloom-home", "--json", "auth", "status", "-p", "--output-format", "stream-json", "--permission-prompts", "none"} {
		if !slices.Contains(args, want) {
			t.Fatalf("runtime missing %q: %s", want, data)
		}
	}
	if strings.Count(string(data), "container\ncreate\n") != 3 {
		t.Fatalf("preflight and exec not both sandboxed: %s", data)
	}
	var copiedDir string
	for _, arg := range args {
		if strings.HasPrefix(arg, "CLAUDE_CONFIG_DIR=") {
			copiedDir = strings.TrimPrefix(arg, "CLAUDE_CONFIG_DIR=")
			break
		}
	}
	if copiedDir == "" || copiedDir == f.env.ConfigDir || !strings.Contains(string(data), "type=volume,source=evaloom-config-") || strings.Contains(string(data), "type=bind,source="+copiedDir) {
		t.Fatal("configuration must use a private working volume")
	}
	bad := f.env
	bad.ConfigDir = "/wrong"
	launch := ContainerLaunch(config, scope)
	// The resolver deliberately returns a different worktree than the request.
	badLaunch := ContainerLaunch(config, func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		spec, _ := scope(env)
		spec.Worktree = "/wrong"
		return spec, nil
	})
	if _, err := badLaunch(context.Background(), f.env, []string{"claude"}, io.Discard, io.Discard); err == nil {
		t.Fatal("scope mismatch accepted")
	}
	writableLaunch := ContainerLaunch(config, func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		spec, _ := scope(env)
		spec.ConfigWritable = true
		return spec, nil
	})
	if _, err := writableLaunch(context.Background(), f.env, []string{"claude"}, io.Discard, io.Discard); err == nil {
		t.Fatal("Claude Code accepted a writable configuration without a verified capability")
	}
	if _, err := launch(context.Background(), bad, []string{"claude"}, io.Discard, io.Discard); err == nil {
		t.Fatal("invalid config mount accepted")
	}
}

func TestContainerAuthenticationRecoveredAfterEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel", "timeout", "preflight_failure"} {
		t.Run(outcome, func(t *testing.T) {
			mode := map[string]string{"success": "success", "failure": "exit-130", "cancel": "ignore-term", "timeout": "ignore-term", "preflight_failure": "config-130"}[outcome]
			f := newFixture(t, "success")
			root := useFakeContainerRuntime(t, f, "refresh-"+mode)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if outcome == "timeout" {
				f.request.Limits.TimeoutSec = 1
			}
			id, err := f.h.Start(ctx, f.request, f.env)
			if err != nil {
				t.Fatal(err)
			}
			lines, _ := f.h.Logs(id)
			for line := range lines {
				if strings.Contains(line.Text, "private-volume-refreshed") {
					t.Fatal("volume authentication in logs")
				}
				if outcome == "cancel" && strings.Contains(line.Text, "system") {
					_ = f.h.Cancel(id)
				}
			}
			result, err := f.h.Result(id)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]adapter.ErrorClass{"failure": adapter.ErrorExecutionFailed, "cancel": adapter.ErrorCancelled, "timeout": adapter.ErrorTimeout, "preflight_failure": adapter.ErrorConfig}[outcome]
			if outcome == "success" {
				if result.Status != adapter.ResultSucceeded {
					t.Fatalf("result=%+v", result)
				}
			} else if result.ErrorClass == nil || *result.ErrorClass != want {
				t.Fatalf("result=%+v", result)
			}
			data, err := os.ReadFile(f.store)
			if err != nil || string(data) != `{"token":"private-volume-refreshed"}` {
				t.Fatal("volume refresh not recovered", err)
			}
			// Recovery happens only after a removal confirming writers stopped.
			recovered := false
			stopped := false
			for _, call := range runtimeCalls(t, root) {
				if len(call.Args) > 1 && call.Args[0] == "container" && call.Args[1] == "rm" {
					stopped = true
				}
				if len(call.Args) > 2 && call.Args[1] == "cp" && strings.Contains(call.Args[2], ":/evaloom-auth/") {
					if !stopped {
						t.Fatal("credential recovered before stopped Sandbox")
					}
					recovered = true
				}
				if len(call.Args) > 2 && call.Args[1] == "start" && strings.HasPrefix(call.Args[len(call.Args)-1], "evaloom-auth-") {
					t.Fatal("recovery container must never start")
				}
			}
			if !recovered {
				t.Fatal("volume was not read for writeback")
			}
		})
	}
}

func TestAuthenticationWritebackFailureCannotChangeFinishedExecution(t *testing.T) {
	f := newFixture(t, "success")
	useFakeContainerRuntime(t, f, "writeback-delay-failure")
	f.request.Limits.TimeoutSec = 1
	_, result, logs := runResult(t, f)
	if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil {
		t.Fatalf("writeback failure/deadline changed finished result: %+v", result)
	}
	warning := false
	for _, line := range logs {
		if strings.Contains(line.Text, "authentication writeback skipped:") {
			warning = true
		}
	}
	if !warning {
		t.Fatal("writeback failure not logged")
	}
	data, err := os.ReadFile(f.store)
	if err != nil || string(data) != `{"fake":true}` {
		t.Fatal("failed recovery changed store", err)
	}
}
