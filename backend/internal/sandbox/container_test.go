package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

type invocation struct {
	Args []string
	Env  []string
}

// This fake is a CLI client plus a separate workload process group. Killing
// only the attach CLI leaves the workload alive, just as with a real daemon.
// No Docker daemon, images, network or model credentials are used by any test.
func TestRuntimeHelper(t *testing.T) {
	root := os.Getenv("SANDBOX_FAKE_ROOT")
	if root == "" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		os.Exit(90)
	}
	args := os.Args[separator+1:]
	mode := os.Getenv("SANDBOX_FAKE_MODE")
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			os.Exit(91)
		}
	}
	if args[0] == "leaf" {
		fmt.Println(os.Getpid())
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if args[0] == "workload" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		leaf := exec.Command(os.Args[0], "-test.run=^TestRuntimeHelper$", "--", "leaf")
		leaf.Env = os.Environ() // Only the explicitly supplied fake runtime env.
		var leafOutput bytes.Buffer
		leaf.Stdout = &leafOutput
		if err := leaf.Start(); err != nil {
			os.Exit(92)
		}
		write("leaf.pid", strconv.Itoa(leaf.Process.Pid))
		write("ready", "yes")
		<-term
		_ = leaf.Process.Kill()
		_ = leaf.Wait()
		os.Exit(0)
	}
	data, _ := json.Marshal(invocation{args, os.Environ()})
	log, err := os.OpenFile(filepath.Join(root, "calls.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(93)
	}
	_, _ = log.Write(append(data, '\n'))
	_ = log.Close()
	fail := func() {
		fmt.Fprintln(os.Stderr, "runtime diagnostic with SECRET_DO_NOT_REPORT")
		os.Exit(125)
	}
	if args[0] == "image" {
		switch mode {
		case "image-fail":
			fail()
		case "image-volume":
			fmt.Print(`{"Os":"linux","Config":{"Volumes":{"/unexpected":{}}}}`)
		case "image-darwin":
			fmt.Print(`{"Os":"darwin","Config":{}}`)
		default:
			fmt.Print(`{"Os":"linux","Config":{"Env":["IMAGE_SECRET=secret"]}}`)
		}
		os.Exit(0)
	}
	switch args[1] {
	case "create":
		if mode == "create-fail" {
			fail()
		}
		write("created", strings.Join(args, "\n"))
		if mode == "slow-create" {
			time.Sleep(time.Hour)
		}
		fmt.Print("container-id")
	case "start":
		if mode == "start-fail" {
			fail()
		}
		if mode == "slow-inspect" {
			// The test owns/reaps a separate running workload. The attach
			// client exits early without implying the container has stopped.
			os.Exit(0)
		}
		if mode == "sleep" {
			worker := exec.Command(os.Args[0], "-test.run=^TestRuntimeHelper$", "--", "workload")
			worker.Env = os.Environ()
			worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := worker.Start(); err != nil {
				os.Exit(94)
			}
			write("workload.pid", strconv.Itoa(worker.Process.Pid))
			_ = worker.Wait()
			write("exited", "yes")
		} else {
			if mode == "env-probe" {
				created, _ := os.ReadFile(filepath.Join(root, "created"))
				createArgs := strings.Split(string(created), "\n")
				index := slices.Index(createArgs, "-i")
				probe := exec.Command("/usr/bin/env", createArgs[index:]...)
				probe.Env = []string{"IMAGE_SECRET=secret"}
				probe.Stdout, probe.Stderr = os.Stdout, os.Stderr
				if err := probe.Run(); err != nil {
					os.Exit(95)
				}
			} else {
				fmt.Print("workload stdout")
				fmt.Fprint(os.Stderr, "workload stderr")
			}
			write("exited", "yes")
		}
	case "inspect":
		if mode == "slow-inspect" {
			write("inspecting", "yes")
			time.Sleep(3 * time.Second)
		}
		if mode == "inspect-fail" {
			fail()
		}
		if mode == "rm-blocked-created" {
			fmt.Print(`{"Status":"created","Running":false}`)
		} else if mode == "running-state" || mode == "rm-blocked-running" {
			fmt.Print(`{"Status":"running","Running":true}`)
		} else if _, err := os.Stat(filepath.Join(root, "exited")); err == nil {
			code := 0
			if mode == "exit-7" {
				code = 7
			}
			if mode == "oom" || mode == "signal" {
				code = 137
			}
			fmt.Printf(`{"Status":"exited","Running":false,"ExitCode":%d,"OOMKilled":%t}`, code, mode == "oom")
		} else {
			fmt.Print(`{"Status":"running","Running":true}`)
		}
	case "kill", "rm":
		if strings.HasPrefix(mode, "rm-blocked-") {
			if _, err := os.Stat(filepath.Join(root, "allow-remove")); err != nil {
				write("blocked", "yes")
				fail()
			}
		}
		if mode == "rm-fail" && args[1] == "rm" {
			fail()
		}
		if pidData, err := os.ReadFile(filepath.Join(root, "workload.pid")); err == nil {
			pid, _ := strconv.Atoi(string(pidData))
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			// Runtime removal acknowledges only after the workload parent has
			// reaped its child and the attach client has reaped the parent.
			deadline := time.Now().Add(5 * time.Second)
			for syscall.Kill(pid, 0) != syscall.ESRCH {
				if time.Now().After(deadline) {
					fail()
				}
				time.Sleep(time.Millisecond)
			}
		}
		if args[1] == "rm" {
			write("removed", "yes")
		}
	case "ls":
		if _, err := os.Stat(filepath.Join(root, "removed")); err != nil {
			fmt.Print("still-present")
		}
	default:
		os.Exit(96)
	}
	os.Exit(0)
}

type fixture struct {
	config ContainerConfig
	spec   SandboxSpec
	root   string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: root}
	for _, dir := range []string{"home", "data", "worktree", "git", "config", "runtime"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// The wrapper's executable and state are outside all sandbox mounts.
	wrapper := filepath.Join(root, "runtime", "cli")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\"'\"'") + "'"
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" -test.run=^TestRuntimeHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.config = ContainerConfig{
		RuntimePath: wrapper, Image: "test-image:explicit", ControlTimeout: 2 * time.Second,
		WorktreeRoot: filepath.Join(root, "worktree"), GitDirRoot: filepath.Join(root, "git"), ConfigDirRoot: filepath.Join(root, "config"),
		HostHome: filepath.Join(root, "home"), DataDir: filepath.Join(root, "data"), RunnerTokenFile: filepath.Join(root, "runner-token"),
		AllowEnv:   []string{"PATH", "LANG", "CODEX_HOME"},
		RuntimeEnv: map[string]string{"SANDBOX_FAKE_ROOT": filepath.Join(root, "runtime"), "SANDBOX_FAKE_MODE": mode, "GORACE": "atexit_sleep_ms=0"},
		Stdout:     &f.stdout, Stderr: &f.stderr,
	}
	f.spec = SandboxSpec{Kind: Harness, Worktree: filepath.Join(root, "worktree"), GitDir: filepath.Join(root, "git"), ConfigDir: filepath.Join(root, "config"),
		Env: map[string]string{"LANG": "C", "PATH": "/usr/bin:/bin", "CODEX_HOME": filepath.Join(root, "config"), "HOME": f.config.HostHome,
			"RUNNER_TOKEN": "sandbox-secret", "SSH_AUTH_SOCK": "/agent", "DOCKER_HOST": "unix:///var/run/docker.sock"},
		Limits: ResourceLimits{CPU: 1.5, MemoryBytes: 128 * 1024 * 1024, Time: 10 * time.Second}}
	return f
}

func (f *fixture) sandbox(t *testing.T) *Container {
	t.Helper()
	c, err := NewContainer(f.config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fixture) calls(t *testing.T) []invocation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "runtime", "calls.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var calls []invocation
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var call invocation
		if err := json.Unmarshal(line, &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func requireClass(t *testing.T, err error, status model.ExecutionStatus, class model.ErrorClass) {
	t.Helper()
	var classified *Error
	if !errors.As(err, &classified) || classified.Status() != status || classified.ErrorClass() != class {
		t.Fatalf("error=%v; want %s(%s)", err, status, class)
	}
	if strings.Contains(err.Error(), "SECRET_DO_NOT_REPORT") {
		t.Fatal("raw runtime diagnostic exposed")
	}
}

func TestContainerMountEnvironmentAndLimitArguments(t *testing.T) {
	t.Setenv("HOST_SECRET", "host-secret-value")
	t.Setenv("RUNNER_TOKEN", "host-runner-token")
	for _, kind := range []SandboxKind{Harness, TestSetup, TestRun} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t, "normal")
			f.spec.Kind = kind
			if kind != Harness {
				f.spec.ConfigDir = ""
			}
			process, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/echo", "literal; $(touch forbidden)"})
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Wait(); err != nil {
				t.Fatal(err)
			}
			var create []string
			for _, call := range f.calls(t) {
				for _, env := range call.Env {
					if strings.HasPrefix(env, "HOST_SECRET=") || strings.HasPrefix(env, "RUNNER_TOKEN=") {
						t.Fatalf("host environment inherited by CLI: %v", call.Env)
					}
				}
				if slices.Contains(call.Args, "create") {
					create = call.Args
				}
			}
			value := func(flag string) string {
				i := slices.Index(create, flag)
				if i < 0 || i+1 == len(create) {
					t.Fatalf("missing %s: %v", flag, create)
				}
				return create[i+1]
			}
			if value("--network") != "none" || value("--cpus") != "1.5" || value("--memory") != "134217728" || value("--memory-swap") != "134217728" || value("--pull") != "never" {
				t.Fatalf("wrong resource/network/pull limits: %v", create)
			}
			var mounts []string
			for i, arg := range create {
				if arg == "--mount" {
					mounts = append(mounts, create[i+1])
				}
			}
			expected := []string{"type=bind,source=" + f.spec.Worktree + ",target=" + f.spec.Worktree,
				"type=bind,source=" + f.spec.GitDir + ",target=" + f.spec.GitDir}
			if kind == Harness {
				expected = append(expected, "type=bind,source="+f.spec.ConfigDir+",target="+f.spec.ConfigDir+",readonly")
			} else {
				expected[1] += ",readonly"
			}
			if !slices.Equal(mounts, expected) {
				t.Fatalf("mounts=%v; want %v", mounts, expected)
			}
			for _, mount := range mounts {
				for _, protected := range []string{f.config.HostHome, f.config.DataDir, f.config.RunnerTokenFile, "/var/run/docker.sock"} {
					if strings.Contains(mount, protected) {
						t.Fatalf("protected path %s in mount %s", protected, mount)
					}
				}
			}
			joined := strings.Join(create, "\n")
			for _, secret := range []string{"host-secret-value", "host-runner-token", "sandbox-secret", "SSH_AUTH_SOCK=", "DOCKER_HOST=", "HOME=" + f.config.HostHome} {
				if strings.Contains(joined, secret) {
					t.Fatalf("unallowlisted environment in arguments: %s", secret)
				}
			}
			if value("--entrypoint") != "/usr/bin/env" || !slices.Contains(create, "-i") || !slices.Contains(create, "HOME="+privateHome) || !slices.Contains(create, "LANG=C") {
				t.Fatalf("environment not isolated: %v", create)
			}
			if got := create[len(create)-1]; got != "literal; $(touch forbidden)" {
				t.Fatalf("command changed: %q", got)
			}
			if code, ok := process.ExitCode(); !ok || code != 0 || f.stdout.String() != "workload stdout" || f.stderr.String() != "workload stderr" {
				t.Fatalf("output/status: code=%d ok=%v stdout=%q stderr=%q", code, ok, f.stdout.String(), f.stderr.String())
			}
			before := len(f.calls(t))
			if process.Stop() != nil || process.Kill() != nil || process.Wait() != nil || len(f.calls(t)) != before {
				t.Fatal("completed lifecycle operations are not idempotent")
			}
		})
	}
}

func TestContainerClearsImageEnvironment(t *testing.T) {
	f := newFixture(t, "env-probe")
	p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/usr/bin/env"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(f.stdout.String()), "\n")
	slices.Sort(lines)
	want := []string{"HOME=" + privateHome, "LANG=C", "PATH=/usr/bin:/bin", "CODEX_HOME=" + f.spec.ConfigDir}
	slices.Sort(want)
	if !slices.Equal(lines, want) {
		t.Fatalf("effective workload env=%v; want exactly %v", lines, want)
	}
}

func TestContainerRejectsUnsafeSpecsBeforeInvokingRuntime(t *testing.T) {
	cases := []struct {
		name string
		edit func(*fixture)
	}{
		{"unknown-kind", func(f *fixture) { f.spec.Kind = "other" }},
		{"network-host", func(f *fixture) { f.spec.Network = "host" }},
		{"network-allowlist-pending", func(f *fixture) { f.spec.Network = "harness" }},
		{"no-cpu", func(f *fixture) { f.spec.Limits.CPU = 0 }},
		{"nan-cpu", func(f *fixture) { f.spec.Limits.CPU = math.NaN() }},
		{"infinite-cpu", func(f *fixture) { f.spec.Limits.CPU = math.Inf(1) }},
		{"no-memory", func(f *fixture) { f.spec.Limits.MemoryBytes = 0 }},
		{"no-wall-time", func(f *fixture) { f.spec.Limits.Time = 0 }},
		{"no-worktree", func(f *fixture) { f.spec.Worktree = "" }},
		{"relative-worktree", func(f *fixture) { f.spec.Worktree = "relative" }},
		{"missing-worktree", func(f *fixture) { f.spec.Worktree += "-absent" }},
		{"home-as-worktree", func(f *fixture) { f.spec.Worktree = f.config.HostHome }},
		{"home-as-git", func(f *fixture) { f.spec.GitDir = f.config.HostHome }},
		{"home-as-config", func(f *fixture) { f.spec.ConfigDir = f.config.HostHome }},
		{"home-ancestor", func(f *fixture) { f.spec.Worktree = f.root }},
		{"data-as-worktree", func(f *fixture) { f.spec.Worktree = f.config.DataDir }},
		{"data-as-git", func(f *fixture) { f.spec.GitDir = f.config.DataDir }},
		{"data-as-config", func(f *fixture) { f.spec.ConfigDir = f.config.DataDir }},
		{"token-in-mount", func(f *fixture) { f.config.RunnerTokenFile = filepath.Join(f.spec.Worktree, "token") }},
		{"test-cannot-see-config", func(f *fixture) { f.spec.Kind = TestRun }},
		{"harness-needs-config", func(f *fixture) { f.spec.ConfigDir = "" }},
		{"harness-needs-git", func(f *fixture) { f.spec.GitDir = "" }},
		{"overlap-mounts", func(f *fixture) { f.spec.ConfigDir = f.spec.Worktree }},
		{"env-nul", func(f *fixture) { f.spec.Env["LANG"] = "C\x00secret" }},
		{"mount-symlink-to-home", func(f *fixture) {
			link := filepath.Join(f.root, "alias")
			if err := os.Symlink(f.config.HostHome, link); err != nil {
				t.Fatal(err)
			}
			f.spec.Worktree = link
		}},
		{"mount-symlink-to-data", func(f *fixture) {
			link := filepath.Join(f.root, "alias")
			if err := os.Symlink(f.config.DataDir, link); err != nil {
				t.Fatal(err)
			}
			f.spec.ConfigDir = link
		}},
		{"comma-in-mount", func(f *fixture) {
			f.spec.Worktree = filepath.Join(f.root, "bad,target=/host")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "normal")
			tc.edit(f)
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if p != nil || len(f.calls(t)) != 0 {
				t.Fatal("unsafe spec invoked runtime")
			}
		})
	}
}

func TestContainerRejectsMountRootEscapes(t *testing.T) {
	for _, field := range []string{"worktree", "git", "config"} {
		for _, escape := range []string{"home-credentials-symlink", "outside-direct", "root-prefix-sibling", "other-root-symlink"} {
			t.Run(field+"/"+escape, func(t *testing.T) {
				f := newFixture(t, "normal")
				source := map[string]*string{"worktree": &f.spec.Worktree, "git": &f.spec.GitDir, "config": &f.spec.ConfigDir}[field]
				root := *source
				outside := filepath.Join(f.root, "outside")
				switch escape {
				case "home-credentials-symlink":
					outside = filepath.Join(f.config.HostHome, ".ssh")
				case "root-prefix-sibling":
					outside = root + "-sibling"
				case "other-root-symlink":
					outside = f.spec.Worktree
					if field == "worktree" {
						outside = f.spec.GitDir
					}
				}
				if err := os.MkdirAll(outside, 0700); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(escape, "symlink") {
					link := filepath.Join(root, "escape")
					if err := os.Symlink(outside, link); err != nil {
						t.Fatal(err)
					}
					*source = link
				} else {
					*source = outside
				}
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
				requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
				if p != nil || len(f.calls(t)) != 0 {
					t.Fatal("mount outside its allowed root reached the runtime")
				}
			})
		}
	}
}

func TestContainerAllowsManagedHomeSubtrees(t *testing.T) {
	for _, aliases := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlinks=%t", aliases), func(t *testing.T) {
			f := newFixture(t, "normal")
			var expected []string
			for i, pair := range []struct{ root, source *string }{
				{&f.config.WorktreeRoot, &f.spec.Worktree},
				{&f.config.GitDirRoot, &f.spec.GitDir},
				{&f.config.ConfigDirRoot, &f.spec.ConfigDir},
			} {
				root := filepath.Join(f.config.HostHome, "evaloom-managed", strconv.Itoa(i))
				real := filepath.Join(root, "execution")
				if err := os.MkdirAll(real, 0700); err != nil {
					t.Fatal(err)
				}
				*pair.root, *pair.source = root, real
				if aliases {
					// Canonicalize both the allowed root and the mount source,
					// while preserving the original target spelling for Git.
					*pair.root = filepath.Join(f.root, fmt.Sprintf("root-alias-%d", i))
					*pair.source = filepath.Join(f.root, fmt.Sprintf("source-alias-%d", i))
					if err := os.Symlink(root, *pair.root); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(real, *pair.source); err != nil {
						t.Fatal(err)
					}
				}
				mount := "type=bind,source=" + real + ",target=" + *pair.source
				if i == 2 {
					mount += ",readonly"
				}
				expected = append(expected, mount)
			}
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			for _, call := range f.calls(t) {
				if slices.Contains(call.Args, "create") {
					var mounts []string
					for i, arg := range call.Args {
						if arg == "--mount" {
							mounts = append(mounts, call.Args[i+1])
						}
					}
					if !slices.Equal(mounts, expected) {
						t.Fatalf("managed HOME mounts=%v; want %v", mounts, expected)
					}
					return
				}
			}
			t.Fatal("container was not created")
		})
	}
}

func TestContainerProtectedPathsStillRejectedWithinAllowedRoot(t *testing.T) {
	for _, field := range []string{"worktree", "git", "config"} {
		for _, protected := range []string{"home", "home-ancestor", "data-child", "token-parent"} {
			t.Run(field+"/"+protected, func(t *testing.T) {
				f := newFixture(t, "normal")
				// Even a broad caller allowlist cannot override the denylist.
				f.config.WorktreeRoot, f.config.GitDirRoot, f.config.ConfigDirRoot = f.root, f.root, f.root
				path := f.config.HostHome
				switch protected {
				case "home-ancestor":
					path = f.root
				case "data-child":
					path = filepath.Join(f.config.DataDir, "logs")
				case "token-parent":
					path = filepath.Join(f.root, "credentials")
					f.config.RunnerTokenFile = filepath.Join(path, "runner-token")
				}
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				*map[string]*string{"worktree": &f.spec.Worktree, "git": &f.spec.GitDir, "config": &f.spec.ConfigDir}[field] = path
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
				requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
				if p != nil || len(f.calls(t)) != 0 {
					t.Fatal("protected mount reached runtime despite denylist")
				}
			})
		}
	}
}

func TestContainerCaseInsensitiveProtectedMounts(t *testing.T) {
	for _, field := range []string{"worktree", "git", "config"} {
		for _, protected := range []string{"home", "home-ancestor", "data", "data-child", "data-ancestor", "token-parent", "missing-token-parent"} {
			t.Run(field+"/"+protected, func(t *testing.T) {
				f := newFixture(t, "normal")
				// A common allowed root must not override filesystem identity
				// or ancestor checks for protected paths.
				f.config.WorktreeRoot, f.config.GitDirRoot, f.config.ConfigDirRoot = f.root, f.root, f.root
				path := f.config.HostHome
				switch protected {
				case "home-ancestor":
					path = filepath.Join(f.root, "personal")
					f.config.HostHome = filepath.Join(path, "home")
				case "data":
					path = f.config.DataDir
				case "data-child":
					path = filepath.Join(f.config.DataDir, "logs")
				case "data-ancestor":
					path = filepath.Join(f.root, "state")
					f.config.DataDir = filepath.Join(path, "data")
				case "token-parent", "missing-token-parent":
					path = filepath.Join(f.root, "credentials")
					f.config.RunnerTokenFile = filepath.Join(path, "runner-token")
				}
				for _, dir := range []string{path, f.config.HostHome, f.config.DataDir} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if protected == "token-parent" {
					if err := os.WriteFile(f.config.RunnerTokenFile, []byte("secret"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				// Change every component below the common root, including the
				// protected ancestor in data-child. EvalSymlinks does not
				// normalize this spelling on case-insensitive macOS volumes.
				rel, err := filepath.Rel(f.root, path)
				if err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(f.root, strings.ToUpper(rel))
				original, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				alternate, err := os.Stat(alias)
				if os.IsNotExist(err) {
					t.Skip("case-sensitive filesystem: alternate spelling does not name the same directory")
				}
				if err != nil || !os.SameFile(original, alternate) {
					t.Fatalf("case alias must identify the protected directory: %v", err)
				}
				*map[string]*string{"worktree": &f.spec.Worktree, "git": &f.spec.GitDir, "config": &f.spec.ConfigDir}[field] = alias
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
				requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
				if p != nil || len(f.calls(t)) != 0 {
					t.Fatal("case alias of protected mount reached runtime")
				}
			})
		}
	}
}

func TestContainerRejectsOverlappingMountSources(t *testing.T) {
	for _, kind := range []SandboxKind{Harness, TestSetup, TestRun} {
		for _, pair := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
			if kind != Harness && pair[1] == 2 {
				continue // Test sandboxes cannot mount ConfigDir at all.
			}
			for _, relation := range []string{"same", "first-contains-second", "second-contains-first"} {
				for _, spelling := range []string{"literal", "symlink", "case-alias"} {
					t.Run(fmt.Sprintf("%s/%d-%d/%s/%s", kind, pair[0], pair[1], relation, spelling), func(t *testing.T) {
						f := newFixture(t, "normal")
						f.spec.Kind = kind
						if kind != Harness {
							f.spec.ConfigDir = ""
						}
						// Common allowed roots ensure rejection comes from the mount
						// overlap check, not the per-source root or protection checks.
						f.config.WorktreeRoot, f.config.GitDirRoot, f.config.ConfigDirRoot = f.root, f.root, f.root
						first := filepath.Join(f.root, "shared")
						second := first
						switch relation {
						case "first-contains-second":
							second = filepath.Join(first, "nested")
						case "second-contains-first":
							first = filepath.Join(second, "nested")
						}
						for _, dir := range []string{first, second} {
							if err := os.MkdirAll(dir, 0700); err != nil {
								t.Fatal(err)
							}
						}
						switch spelling {
						case "symlink":
							alias := filepath.Join(f.root, "alias")
							if err := os.Symlink(second, alias); err != nil {
								t.Fatal(err)
							}
							second = alias
						case "case-alias":
							rel, err := filepath.Rel(f.root, second)
							if err != nil {
								t.Fatal(err)
							}
							alias := filepath.Join(f.root, strings.ToUpper(rel))
							original, err := os.Stat(second)
							if err != nil {
								t.Fatal(err)
							}
							alternate, err := os.Stat(alias)
							if os.IsNotExist(err) {
								t.Skip("case-sensitive filesystem: alternate spelling does not name the same directory")
							}
							if err != nil || !os.SameFile(original, alternate) {
								t.Fatalf("case alias must identify the same mount source: %v", err)
							}
							second = alias
						}
						sources := []*string{&f.spec.Worktree, &f.spec.GitDir, &f.spec.ConfigDir}
						*sources[pair[0]], *sources[pair[1]] = first, second
						p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
						if p != nil {
							_ = waitProcess(t, p)
						}
						requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
						var classified *Error
						if !errors.As(err, &classified) || classified.Op != "overlapping sandbox mounts" {
							t.Fatalf("mounts must be rejected specifically for overlap: %v", err)
						}
						if p != nil || len(f.calls(t)) != 0 {
							t.Fatal("overlapping mounts reached runtime")
						}
					})
				}
			}
		}
	}
}

func TestContainerAllowsCaseInsensitiveMountRootAliases(t *testing.T) {
	for _, kind := range []SandboxKind{Harness, TestSetup, TestRun} {
		for _, field := range []string{"worktree", "git", "config"} {
			if kind != Harness && field == "config" {
				continue
			}
			t.Run(string(kind)+"/"+field, func(t *testing.T) {
				f := newFixture(t, "normal")
				f.spec.Kind = kind
				if kind != Harness {
					f.spec.ConfigDir = ""
				}
				source := map[string]*string{"worktree": &f.spec.Worktree, "git": &f.spec.GitDir, "config": &f.spec.ConfigDir}[field]
				// Exercise ancestor identity as well as root identity: only the
				// root component changes case; the source is a nested directory.
				original := filepath.Join(*source, "execution")
				if err := os.Mkdir(original, 0700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(f.root, strings.ToUpper(field), "execution")
				info, err := os.Stat(original)
				if err != nil {
					t.Fatal(err)
				}
				alternate, err := os.Stat(alias)
				if os.IsNotExist(err) {
					t.Skip("case-sensitive filesystem: alternate spelling does not name the same directory")
				}
				if err != nil || !os.SameFile(info, alternate) {
					t.Fatalf("case alias must identify the allowed mount source: %v", err)
				}
				*source = alias
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
				if err != nil {
					t.Fatal(err)
				}
				if err := waitProcess(t, p); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestContainerRequiresExplicitConfiguration(t *testing.T) {
	for _, edit := range []func(*ContainerConfig){
		func(c *ContainerConfig) { c.RuntimePath = "" },
		func(c *ContainerConfig) { c.RuntimePath = "docker" },
		func(c *ContainerConfig) { c.Image = "" },
		func(c *ContainerConfig) { c.Image = "--privileged" },
		func(c *ContainerConfig) { c.ControlTimeout = 0 },
		func(c *ContainerConfig) { c.HostHome = "" },
		func(c *ContainerConfig) { c.DataDir = "" },
		func(c *ContainerConfig) { c.RunnerTokenFile = "" },
		func(c *ContainerConfig) { c.WorktreeRoot = "" },
		func(c *ContainerConfig) { c.GitDirRoot = "relative" },
		func(c *ContainerConfig) { c.ConfigDirRoot += "-missing" },
		func(c *ContainerConfig) { c.WorktreeRoot = c.RuntimePath },
		func(c *ContainerConfig) { c.RequiresMacOS = true },
		func(c *ContainerConfig) { c.AllowEnv = []string{"HOME"} },
		func(c *ContainerConfig) { c.AllowEnv = []string{"BAD=NAME"} },
		func(c *ContainerConfig) { c.RuntimeEnv["1BAD"] = "value" },
	} {
		f := newFixture(t, "normal")
		edit(&f.config)
		_, err := NewContainer(f.config)
		requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
		if len(f.calls(t)) != 0 {
			t.Fatal("invalid config invoked runtime")
		}
	}
}

func TestContainerFailureClassificationAndNoFallback(t *testing.T) {
	for _, mode := range []string{"image-fail", "image-volume", "image-darwin", "create-fail", "start-fail", "inspect-fail", "running-state", "oom", "signal", "rm-fail", "exit-7", "missing-runtime"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			if mode == "missing-runtime" {
				f.config.RuntimePath += "-missing"
			}
			marker := filepath.Join(f.spec.Worktree, "host-fallback")
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/sh", "-c", "touch '" + marker + "'"})
			if err == nil {
				err = p.Wait()
			}
			if mode == "exit-7" {
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != 7 {
					t.Fatalf("ordinary command failure=%v", err)
				}
			} else {
				requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("command was executed on host")
			}
			for _, call := range f.calls(t) {
				if slices.Contains(call.Args, "start") && (strings.HasPrefix(mode, "image-") || mode == "create-fail") {
					t.Fatal("started after failed isolation preparation")
				}
			}
		})
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake runtime did not become ready: %s", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitProcess(t *testing.T, p Process) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("sandbox did not stop within test deadline")
		return nil
	}
}

func TestContainerDeadlineAndCancellationStopWholeWorkload(t *testing.T) {
	for _, action := range []string{"deadline", "cancel", "stop", "kill"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, "sleep")
			if action == "deadline" {
				f.spec.Limits.Time = time.Second
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			p, err := f.sandbox(t).Run(ctx, f.spec, []string{"/untrusted/workload"})
			if err != nil {
				t.Fatal(err)
			}
			// Ensure failures in the test itself cannot leave the test workload.
			t.Cleanup(func() {
				cancel()
				_ = p.Kill()
			})
			waitForFile(t, filepath.Join(f.root, "runtime", "ready"))
			var pids []int
			for _, file := range []string{"workload.pid", "leaf.pid"} {
				data, err := os.ReadFile(filepath.Join(f.root, "runtime", file))
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(string(data))
				if err != nil || syscall.Kill(pid, 0) != nil {
					t.Fatalf("workload was not alive before stop: %s %v", data, err)
				}
				pids = append(pids, pid)
			}
			if _, valid := p.ExitCode(); valid {
				t.Fatal("running process has a confirmed exit code")
			}
			switch action {
			case "cancel":
				cancel()
			case "stop":
				if err := p.Stop(); err != nil {
					t.Fatal(err)
				}
			case "kill":
				if err := p.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			err = waitProcess(t, p)
			if action == "deadline" {
				requireClass(t, err, model.ExecutionTimedOut, model.ErrorTimeout)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < f.spec.Limits.Time || time.Since(start) > 5*time.Second {
					t.Fatalf("wall time not enforced promptly: %v (%v)", time.Since(start), err)
				}
			}
			if action == "cancel" {
				requireClass(t, err, model.ExecutionCancelled, model.ErrorCancelled)
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation cause lost")
				}
			}
			for _, pid := range pids {
				if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
					t.Fatalf("process %d survived Wait: %v", pid, err)
				}
			}
			if _, err := os.Stat(f.spec.Worktree); err != nil {
				t.Fatalf("worktree was removed: %v", err)
			}
			if p.Stop() != nil || p.Kill() != nil {
				t.Fatal("stop/kill after completion failed")
			}
		})
	}
}

func TestContainerDeadlineWhileInspectBlocks(t *testing.T) {
	f := newFixture(t, "slow-inspect")
	f.spec.Limits.Time = time.Second
	f.config.ControlTimeout = 5 * time.Second
	// A separate process group stands in for a daemon-owned container. This
	// test owns the reaper so an early attach exit cannot stop/reap it for us.
	worker := exec.Command(os.Args[0], "-test.run=^TestRuntimeHelper$", "--", "workload")
	for key, value := range f.config.RuntimeEnv {
		worker.Env = append(worker.Env, key+"="+value)
	}
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-worker.Process.Pid, syscall.SIGTERM)
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
			<-workerDone
		}
	})
	runtimeDir := filepath.Join(f.root, "runtime")
	if err := os.WriteFile(filepath.Join(runtimeDir, "workload.pid"), []byte(strconv.Itoa(worker.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(runtimeDir, "ready"))
	leafData, err := os.ReadFile(filepath.Join(runtimeDir, "leaf.pid"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := strconv.Atoi(string(leafData))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/untrusted/workload"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill(); _ = waitProcess(t, p) })
	waitForFile(t, filepath.Join(runtimeDir, "inspecting"))
	if syscall.Kill(worker.Process.Pid, 0) != nil || syscall.Kill(leaf, 0) != nil {
		t.Fatal("workload must be alive while initial inspection blocks")
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		requireClass(t, err, model.ExecutionTimedOut, model.ErrorTimeout)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < f.spec.Limits.Time {
			t.Fatalf("lost wall-time deadline: %v (%v)", time.Since(start), err)
		}
	case <-time.After(time.Until(start.Add(2 * time.Second))):
		t.Fatalf("inspection delayed workload stop beyond 1s limit: elapsed=%v", time.Since(start))
	}
	for _, pid := range []int{worker.Process.Pid, leaf} {
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Fatalf("process %d survived deadline during inspection: %v", pid, err)
		}
	}
}

func TestContainerDeadlineDuringCreation(t *testing.T) {
	f := newFixture(t, "slow-create")
	f.spec.Limits.Time = 500 * time.Millisecond
	p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
	requireClass(t, err, model.ExecutionTimedOut, model.ErrorTimeout)
	if p != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("creation deadline lost")
	}
	var removed bool
	for _, call := range f.calls(t) {
		if slices.Contains(call.Args, "start") {
			t.Fatal("started after creation timed out")
		}
		removed = removed || slices.Contains(call.Args, "rm")
	}
	if !removed {
		t.Fatal("partially created container was not removed by name")
	}
}

func TestContainerWaitDoesNotReleaseUnconfirmedSandbox(t *testing.T) {
	for _, status := range []string{"created", "running"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, "rm-blocked-"+status)
			f.config.ControlTimeout = 300 * time.Millisecond
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
			if err != nil {
				t.Fatal(err)
			}
			// Allow cleanup even if the assertion fails, while the state dir
			// still exists (before t.TempDir's cleanup).
			allowRemoval := func() {
				if err := os.WriteFile(filepath.Join(f.root, "runtime", "allow-remove"), nil, 0600); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() { allowRemoval(); _ = p.Kill() })
			waitForFile(t, filepath.Join(f.root, "runtime", "blocked"))
			done := make(chan error, 1)
			go func() { done <- p.Wait() }()
			select {
			case err := <-done:
				t.Fatalf("Wait released sandbox with state %s and unconfirmed removal: %v", status, err)
			case <-time.After(50 * time.Millisecond):
			}
			allowRemoval()
			requireClass(t, waitProcess(t, p), model.ExecutionFailed, model.ErrorSandbox)
		})
	}
}
