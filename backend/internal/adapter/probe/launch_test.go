package probe

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type processStub struct {
	err  error
	code int
	set  bool
}

func (p processStub) Wait() error           { return p.err }
func (processStub) Stop() error             { return nil }
func (processStub) Kill() error             { return nil }
func (p processStub) ExitCode() (int, bool) { return p.code, p.set }

// launchStub stands in for Runner's Work Sandbox launch and records every
// command with the environment it received.
type launchStub struct {
	commands [][]string
	envs     []adapter.SandboxEnv
	run      func(command []string, stdout io.Writer) processStub
	err      error
}

func (l *launchStub) launch(_ context.Context, env adapter.SandboxEnv, command []string, stdout, _ io.Writer) (sandbox.Process, error) {
	l.commands = append(l.commands, command)
	l.envs = append(l.envs, env)
	if l.err != nil {
		return nil, l.err
	}
	return l.run(command, stdout), nil
}

func TestLaunchSandboxRunsEverythingThroughLaunch(t *testing.T) {
	stub := &launchStub{run: func(command []string, stdout io.Writer) processStub {
		if command[0] == "/bin/cat" {
			_, _ = io.WriteString(stdout, validReport)
		}
		return processStub{set: true}
	}}
	h := NewWithConfig(Config{Sandbox: LaunchSandbox{Launch: stub.launch}, Command: []string{"/usr/bin/env", "python3", "-c", "probe"}, Report: ".evaloom-probe/r.json"})
	req := request(t)
	env := adapter.SandboxEnv{Worktree: req.Project.Workdir, ConfigDir: "/config", Env: map[string]string{"CODEX_HOME": "/config"}}
	id, err := h.Start(context.Background(), req, env)
	if err != nil {
		t.Fatal(err)
	}
	if result := wait(t, h, id); result.Status != adapter.ResultSucceeded {
		t.Fatalf("%+v", result)
	}
	got := []string{}
	for i, command := range stub.commands {
		got = append(got, strings.Join(command, " "))
		if stub.envs[i].ConfigDir != "/config" || stub.envs[i].Env["CODEX_HOME"] != "/config" {
			t.Fatalf("session environment not passed: %+v", stub.envs[i])
		}
	}
	want := []string{"/usr/bin/env true", "/usr/bin/env python3 -c probe", "/bin/cat .evaloom-probe/r.json"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands %v; want %v", got, want)
	}
}

func TestLaunchSandboxFailsClosed(t *testing.T) {
	req := request(t)
	env := adapter.SandboxEnv{Worktree: req.Project.Workdir}
	for name, sandbox := range map[string]Sandbox{
		"no launch":    LaunchSandbox{},
		"launch error": LaunchSandbox{Launch: (&launchStub{err: errors.New("runtime missing")}).launch},
		"check exit nonzero": LaunchSandbox{Launch: (&launchStub{run: func([]string, io.Writer) processStub {
			return processStub{err: &sandbox.ExitError{Code: 1}, code: 1, set: true}
		}}).launch},
		"exit unknown": LaunchSandbox{Launch: (&launchStub{run: func([]string, io.Writer) processStub { return processStub{} }}).launch},
	} {
		if _, err := NewWithConfig(Config{Sandbox: sandbox, Command: []string{"x"}, Report: "r.json"}).Start(context.Background(), req, env); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := (LaunchSandbox{Launch: (&launchStub{}).launch}).Check(context.Background()); err == nil {
		t.Fatal("Check without a session environment must not report availability")
	}
	for _, name := range []string{"", "/etc/passwd", "../x", "-n", "a/../../b"} {
		if _, err := (LaunchSandbox{Launch: (&launchStub{}).launch}).ReadFile(context.Background(), env, name); err == nil {
			t.Fatalf("accepted report path %q", name)
		}
	}
	if _, err := NewWithConfig(Config{Sandbox: &sandboxStub{}}).Start(context.Background(), req, env); err == nil {
		t.Fatal("missing command accepted")
	}
}
