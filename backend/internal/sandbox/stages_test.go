package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

var stageKinds = []SandboxKind{Prepare, Work, Check, Publish, Proposal}

func stageFixture(t *testing.T, mode string, kind SandboxKind) *fixture {
	t.Helper()
	f := newFixture(t, mode)
	f.config.StagePaths = contract.RequestPaths{
		Profile: "/job/profile", Workspace: "/job/workspace", Out: "/job/out",
		Inputs: "/job/inputs", Records: "/job/records", Scratch: "/job/scratch",
	}
	f.config.StageMountRoots = map[string]string{}
	for _, role := range []string{"profile", "workspace", "out", "inputs", "records", "scratch"} {
		// Match Runner storage: only the workspace lives outside dataDir.
		source := filepath.Join(f.config.DataDir, role)
		if role == "workspace" {
			source = f.config.WorktreeRoot
		} else if err := os.Mkdir(source, 0700); err != nil {
			t.Fatal(err)
		}
		f.config.StageMountRoots["/job/"+role] = source
	}
	f.spec.Kind, f.spec.Worktree, f.spec.GitDir = kind, "", ""
	f.spec.Env = map[string]string{
		"PATH": "/usr/bin:/bin", "LANG": "C", "TZ": "Pacific/Honolulu", "HOME": f.config.HostHome,
		"CODEX_HOME": f.spec.ConfigDir, "CLAUDE_CONFIG_DIR": f.spec.ConfigDir,
		"EVALOOM_REQUEST": "/job/request.json", "EVALOOM_RESULT": "/job/out/result.json", "EVALOOM_CHANNEL": "/job/channel.sock",
		"RUNNER_TOKEN": "runner-secret", "SSH_AUTH_SOCK": "/agent", "DOCKER_HOST": "unix:///var/run/docker.sock",
		"EVALOOM_SECRET_SLOT": "undeclared-secret", "NODE_OPTIONS": "--require /untrusted.js",
	}
	// Even an overly broad legacy allowlist must not expand the stage contract.
	f.config.AllowEnv = append(f.config.AllowEnv, "RUNNER_TOKEN", "SSH_AUTH_SOCK", "DOCKER_HOST", "EVALOOM_SECRET_SLOT", "NODE_OPTIONS")
	roles := []string{"out", "scratch"}
	switch kind {
	case Prepare:
		roles = append(roles, "profile", "workspace")
	case Work:
		roles = append(roles, "workspace", "inputs", "records")
	case Check, Publish:
		roles = append(roles, "profile", "workspace", "inputs")
	case Proposal:
		roles = append(roles, "inputs")
	}
	if kind != Work && kind != Proposal {
		f.spec.ConfigDir = ""
	}
	for _, role := range roles {
		target := "/job/" + role
		writable := role == "out" || role == "scratch" || (role == "workspace" && (kind == Prepare || kind == Work))
		f.spec.Mounts = append(f.spec.Mounts, Mount{HostPath: f.config.StageMountRoots[target], GuestPath: target, Writable: writable})
	}
	return f
}

func createCall(t *testing.T, f *fixture) []string {
	t.Helper()
	for _, call := range f.calls(t) {
		if len(call.Args) > 1 && call.Args[0] == "container" && call.Args[1] == "create" {
			return call.Args
		}
	}
	t.Fatal("runtime never received container create")
	return nil
}

func flagValues(args []string, flag string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			values = append(values, args[i+1])
		}
	}
	return values
}

func TestStageRuntimeMountsAndPermissions(t *testing.T) {
	for _, kind := range stageKinds {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "normal", kind)
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/echo", "literal; $(touch forbidden)"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			args := createCall(t, f)
			var want []string
			for _, mount := range f.spec.Mounts {
				arg := "type=bind,source=" + mount.HostPath + ",target=" + mount.GuestPath + ",bind-recursive=disabled"
				if !mount.Writable {
					arg += ",readonly"
				}
				want = append(want, arg)
			}
			if f.spec.ConfigDir != "" {
				want = append(want, "type=bind,source="+f.spec.ConfigDir+",target="+f.spec.ConfigDir+",bind-recursive=disabled,readonly")
			}
			if got := flagValues(args, "--mount"); !slices.Equal(got, want) {
				t.Fatalf("mounts=%v, want %v", got, want)
			}
			for _, mount := range flagValues(args, "--mount") {
				for _, protected := range []string{f.config.HostHome, f.config.RunnerTokenFile, ",source=" + f.config.DataDir + ",", "/var/run/docker.sock"} {
					if strings.Contains(mount, protected) {
						t.Fatalf("protected source in %s", mount)
					}
				}
			}
			for flag, value := range map[string]string{"--cpus": "1.5", "--memory": "134217728", "--memory-swap": "134217728", "--entrypoint": "/usr/bin/env", "--network": "none"} {
				if !slices.Equal(flagValues(args, flag), []string{value}) {
					t.Fatalf("wrong %s: %v", flag, args)
				}
			}
			workdir := "/job/profile"
			if kind == Work {
				workdir = "/job/workspace"
			} else if kind == Proposal {
				workdir = ""
			}
			wantWorkdir := []string{workdir}
			if kind == Proposal {
				wantWorkdir = nil
			}
			if !slices.Equal(flagValues(args, "--workdir"), wantWorkdir) || args[len(args)-1] != "literal; $(touch forbidden)" {
				t.Fatalf("command or workdir changed: %v", args)
			}
		})
	}
}

func TestStageEnvironmentIsContractAllowlist(t *testing.T) {
	t.Setenv("HOST_SECRET", "host-secret")
	for _, kind := range stageKinds {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "env-probe", kind)
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/usr/bin/env"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(f.stdout.String()), "\n") {
				key, value, _ := strings.Cut(line, "=")
				got[key] = value
			}
			want := map[string]string{"HOME": got["HOME"], "PATH": "/usr/bin:/bin", "LANG": "C", "TZ": "UTC"}
			if kind != Proposal {
				want["EVALOOM_REQUEST"] = "/job/request.json"
			}
			if kind == Prepare || kind == Check || kind == Publish {
				want["EVALOOM_RESULT"] = "/job/out/result.json"
			} else {
				want["CODEX_HOME"], want["CLAUDE_CONFIG_DIR"] = f.spec.ConfigDir, f.spec.ConfigDir
			}
			if len(got) != len(want) {
				t.Fatalf("unexpected environment: %v", got)
			}
			for key, value := range want {
				if got[key] != value {
					t.Fatalf("%s=%q, want %q", key, got[key], value)
				}
			}
			if !strings.HasPrefix(got["HOME"], "/job/scratch/.home-") || !slices.Contains(flagValues(createCall(t, f), "--tmpfs"), got["HOME"]+":rw,nosuid,nodev,noexec") {
				t.Fatalf("HOME must be an empty private mount inside scratch: %v", got)
			}
			for _, call := range f.calls(t) {
				if slices.Contains(call.Env, "HOST_SECRET=host-secret") {
					t.Fatal("runtime inherited host environment")
				}
			}
		})
	}
}

func TestStageMountAuthorityIsIndependentOfSpec(t *testing.T) {
	for _, change := range []string{"no stage wiring", "missing root", "overlapping targets", "overlapping sources", "protected root", "reserved target"} {
		t.Run(change, func(t *testing.T) {
			f := stageFixture(t, "normal", Work)
			switch change {
			case "no stage wiring":
				f.config.StageMountRoots = nil
			case "missing root":
				delete(f.config.StageMountRoots, "/job/records")
			case "overlapping targets":
				f.config.StagePaths.Records = "/job/inputs/records"
				f.config.StageMountRoots[f.config.StagePaths.Records] = f.config.StageMountRoots["/job/records"]
				delete(f.config.StageMountRoots, "/job/records")
			case "overlapping sources":
				f.config.StageMountRoots["/job/records"] = f.config.StageMountRoots["/job/profile"]
			case "protected root":
				f.config.StageMountRoots["/job/out"] = f.config.HostHome
			case "reserved target":
				f.config.StagePaths.Profile = "/proc"
				f.config.StageMountRoots["/proc"] = f.config.StageMountRoots["/job/profile"]
				delete(f.config.StageMountRoots, "/job/profile")
			}
			c, err := NewContainer(f.config)
			if err == nil {
				_, err = c.Run(context.Background(), f.spec, []string{"/bin/true"})
			}
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if len(f.calls(t)) != 0 {
				t.Fatal("invalid mount authority reached runtime")
			}
		})
	}
	// Stage-only callers need no legacy worktree or Git-directory authority.
	f := stageFixture(t, "normal", Work)
	f.config.WorktreeRoot, f.config.GitDirRoot = "", ""
	c := f.sandbox(t)
	// Mutating the original map after construction must not grant new authority.
	f.config.StageMountRoots["/job/out"] = f.config.HostHome
	p, err := c.Run(context.Background(), f.spec, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitProcess(t, p); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedConfigurationSupportsAIStages(t *testing.T) {
	for _, kind := range []SandboxKind{Work, Proposal} {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "normal", kind)
			env := workingSettings(t, f.config.ConfigDirRoot)
			ctx, closeConfig, err := WithWorkingConfiguration(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := closeConfig(); err != nil {
					t.Error(err)
				}
			}()
			f.spec.ConfigDir, f.spec.ConfigWritable = env.ConfigDir(), true
			f.spec.Env["CODEX_HOME"], f.spec.Env["CLAUDE_CONFIG_DIR"] = env.ConfigDir(), env.ConfigDir()
			w := ctx.Value(configurationKey{}).(*workingConfiguration)
			if err := w.verify(ctx, f.spec); err != nil {
				t.Fatal(err)
			}
			args, err := f.sandbox(t).createArgs(f.spec, []string{"/bin/true"}, "verified", w.volume)
			if err != nil {
				t.Fatal(err)
			}
			mounts := flagValues(args, "--mount")
			if mounts[len(mounts)-1] != "type=volume,source="+w.volume+",target="+env.ConfigDir()+",volume-nocopy" {
				t.Fatalf("working configuration must use its verified volume: %v", mounts)
			}
		})
	}
}

func TestStageInvalidSpecsFailBeforeRuntime(t *testing.T) {
	for _, tt := range []struct {
		name   string
		kind   SandboxKind
		change func(*testing.T, *fixture)
	}{
		{"work profile", Work, func(t *testing.T, f *fixture) {
			f.spec.Mounts = append(f.spec.Mounts, Mount{f.config.StageMountRoots["/job/profile"], "/job/profile", false})
		}},
		{"check writable workspace", Check, func(t *testing.T, f *fixture) { f.spec.Mounts[3].Writable = true }},
		{"publish writable workspace", Publish, func(t *testing.T, f *fixture) { f.spec.Mounts[3].Writable = true }},
		{"writable profile", Prepare, func(t *testing.T, f *fixture) { f.spec.Mounts[2].Writable = true }},
		{"writable inputs", Work, func(t *testing.T, f *fixture) { f.spec.Mounts[3].Writable = true }},
		{"writable records", Work, func(t *testing.T, f *fixture) { f.spec.Mounts[4].Writable = true }},
		{"unexpected socket mount", Work, func(t *testing.T, f *fixture) {
			f.spec.Mounts = append(f.spec.Mounts, Mount{f.root, "/var/run/docker.sock", true})
		}},
		{"missing mount", Work, func(t *testing.T, f *fixture) { f.spec.Mounts = f.spec.Mounts[1:] }},
		{"duplicate mount", Work, func(t *testing.T, f *fixture) { f.spec.Mounts = append(f.spec.Mounts, f.spec.Mounts[0]) }},
		{"host HOME", Work, func(t *testing.T, f *fixture) { f.spec.Mounts[0].HostPath = f.config.HostHome }},
		{"data root", Work, func(t *testing.T, f *fixture) { f.spec.Mounts[0].HostPath = f.config.DataDir }},
		{"host ancestor", Work, func(t *testing.T, f *fixture) { f.spec.Mounts[0].HostPath = f.root }},
		{"wrong source role", Work, func(t *testing.T, f *fixture) { f.spec.Mounts[0].HostPath = f.config.StageMountRoots["/job/profile"] }},
		{"escaping symlink", Work, func(t *testing.T, f *fixture) {
			link := filepath.Join(f.spec.Mounts[0].HostPath, "escape")
			if err := os.Symlink(f.config.DataDir, link); err != nil {
				t.Fatal(err)
			}
			f.spec.Mounts[0].HostPath = link
		}},
		{"token below source", Work, func(t *testing.T, f *fixture) {
			f.config.RunnerTokenFile = filepath.Join(f.spec.Mounts[0].HostPath, "runner-token")
		}},
		{"config aliases profile", Work, func(t *testing.T, f *fixture) {
			f.config.ConfigDirRoot = f.config.StageMountRoots["/job/profile"]
			f.spec.ConfigDir = f.config.ConfigDirRoot
		}},
		{"config in check", Check, func(t *testing.T, f *fixture) { f.spec.ConfigDir = f.config.ConfigDirRoot }},
		{"config env escapes", Work, func(t *testing.T, f *fixture) { f.spec.Env["CODEX_HOME"] = f.config.HostHome }},
		{"unverified writable config", Work, func(t *testing.T, f *fixture) { f.spec.ConfigWritable = true }},
		{"unknown kind", SandboxKind("unknown"), func(t *testing.T, f *fixture) {}},
		{"legacy mount fields", Work, func(t *testing.T, f *fixture) { f.spec.Worktree = f.config.WorktreeRoot }},
		{"network not wired", Work, func(t *testing.T, f *fixture) { f.spec.Network = NetworkHarness }},
		{"zero time", Work, func(t *testing.T, f *fixture) { f.spec.Limits.Time = 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := stageFixture(t, "normal", tt.kind)
			tt.change(t, f)
			c, err := NewContainer(f.config)
			if err == nil {
				_, err = c.Run(context.Background(), f.spec, []string{"/bin/true"})
			}
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if calls := f.calls(t); len(calls) != 0 {
				t.Fatalf("invalid specification reached runtime: %v", calls)
			}
		})
	}
	for _, kind := range stageKinds {
		t.Run(string(kind)+" rejects unwired authority", func(t *testing.T) {
			for _, authority := range []string{"secrets", "channel"} {
				if authority == "secrets" && (kind == Prepare || kind == Publish) {
					continue // Supported by the stage key transport.
				}
				f := stageFixture(t, "normal", kind)
				if authority == "secrets" {
					f.spec.Secrets = map[string]string{"slot": "value"}
				} else {
					f.spec.ChannelPath = "/socket"
				}
				_, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
				requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
				if len(f.calls(t)) != 0 {
					t.Fatal("unwired authority reached runtime")
				}
			}
		})
	}
}

func TestEveryStageDeadlineStopsWholeWorkload(t *testing.T) {
	for _, kind := range stageKinds {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "sleep", kind)
			f.spec.Limits.Time = time.Second
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/untrusted/workload"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Kill() })
			waitForFile(t, filepath.Join(f.root, "runtime", "ready"))
			var pids []int
			for _, name := range []string{"workload.pid", "leaf.pid"} {
				data, err := os.ReadFile(filepath.Join(f.root, "runtime", name))
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(string(data))
				if err != nil || syscall.Kill(pid, 0) != nil {
					t.Fatalf("workload was not alive: %s, %v", data, err)
				}
				pids = append(pids, pid)
			}
			err = waitProcess(t, p)
			requireClass(t, err, model.ExecutionTimedOut, model.ErrorTimeout)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("deadline cause lost")
			}
			for _, pid := range pids {
				if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
					t.Fatalf("process %d survived deadline: %v", pid, err)
				}
			}
		})
	}
}
