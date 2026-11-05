package codex

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestAuthenticationWritebackEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel", "timeout", "preflight_failure"} {
		t.Run(outcome, func(t *testing.T) {
			mode := map[string]string{"success": "success", "failure": "exit", "cancel": "ignore-term", "timeout": "ignore-term", "preflight_failure": "config"}[outcome]
			f := newFixture(t, "refresh-"+mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if outcome == "timeout" {
				f.request.Limits.TimeoutSec = 1
			}
			id, err := f.h.Start(ctx, f.request, f.env)
			if err != nil {
				t.Fatal(err)
			}
			lines, _ := f.h.Logs(id)
			if outcome == "cancel" {
				waitAuthenticationCLI(t, ctx, f.env.Worktree, "started")
				_ = f.h.Cancel(id)
			}
			for line := range lines {
				if strings.Contains(line.Text, "private-refreshed-token") {
					t.Fatal("credential in logs")
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
			if err != nil || string(data) != `{"token":"private-refreshed-token"}` {
				t.Fatal("refreshed credential lost", err)
			}
			if _, err := os.Stat(filepath.Join(f.settings.ConfigDir(), "auth.json")); !os.IsNotExist(err) {
				t.Fatal("authentication placed in original configuration")
			}
			if _, err := f.settings.Files(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthenticationInvalidStoreNeverLaunchesCLI(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "array", "null", "oversize", "symlink", "directory", "permissions", "missing_resolver"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "success")
			switch mode {
			case "missing":
				_ = os.Remove(f.store)
			case "malformed":
				_ = os.WriteFile(f.store, []byte(`{"private-secret":`), 0600)
			case "array":
				_ = os.WriteFile(f.store, []byte(`[]`), 0600)
			case "null":
				_ = os.WriteFile(f.store, []byte(`null`), 0600)
			case "oversize":
				_ = os.WriteFile(f.store, []byte(strings.Repeat("x", harnessenv.AuthenticationLimit+1)), 0600)
			case "symlink":
				_ = os.Remove(f.store)
				_ = os.Symlink(filepath.Join(f.settings.ConfigDir(), "absent"), f.store)
			case "directory":
				_ = os.Remove(f.store)
				_ = os.Mkdir(f.store, 0700)
			case "permissions":
				_ = os.Chmod(f.store, 0644)
			case "missing_resolver":
				f.config.ResolveAuthentication = nil
			}
			launched := false
			f.config.Launch = func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error) {
				launched = true
				return nil, nil
			}
			f.h = New(f.config)
			_, result, logs := runResult(t, f)
			if launched || result.ErrorClass == nil || *result.ErrorClass != adapter.ErrorConfig {
				t.Fatalf("launched=%t result=%+v", launched, result)
			}
			for _, line := range logs {
				if strings.Contains(line.Text, "private-secret") {
					t.Fatal("credential in logs")
				}
			}
		})
	}
}

func TestAuthenticationRejectedWritebackPreservesResultAndLogsReason(t *testing.T) {
	for _, mode := range []string{"malformed", "oversize", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "success")
			before, _ := os.ReadFile(f.store)
			launch := f.config.Launch
			calls := 0
			f.config.Launch = func(ctx context.Context, env adapter.SandboxEnv, args []string, out, stderr io.Writer) (sandbox.Process, error) {
				calls++
				if calls == 2 {
					target := filepath.Join(env.ConfigDir, "auth.json")
					switch mode {
					case "malformed":
						_ = os.WriteFile(target, []byte(`{"private-secret":"invalid`), 0600)
					case "oversize":
						_ = os.WriteFile(target, []byte(strings.Repeat("private-secret", 6000)), 0600)
					case "symlink":
						_ = os.Remove(target)
						_ = os.Symlink(f.store, target)
					}
				}
				return launch(ctx, env, args, out, stderr)
			}
			f.h = New(f.config)
			_, result, logs := runResult(t, f)
			if result.Status != adapter.ResultSucceeded {
				t.Fatalf("writeback changed result: %+v", result)
			}
			after, err := os.ReadFile(f.store)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("invalid copy replaced stored credential")
			}
			warning := false
			for _, line := range logs {
				if strings.Contains(line.Text, "private-secret") {
					t.Fatal("credential in logs")
				}
				if strings.Contains(line.Text, "authentication writeback skipped:") {
					warning = true
				}
			}
			if !warning {
				t.Fatal("missing safe reason in execution logs")
			}
		})
	}
}

func TestAuthenticationWaitingExecutionStopsOnCancelOrTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "success")
			lease, err := harnessenv.AcquireAuthentication(context.Background(), f.store)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			// Invalid contents detect any read that incorrectly happens before locking.
			if err := os.WriteFile(f.store, []byte("invalid while locked"), 0600); err != nil {
				t.Fatal(err)
			}
			resolved := make(chan struct{})
			f.config.ResolveAuthentication = func(adapter.HarnessRequest) (string, error) { close(resolved); return f.store, nil }
			if mode == "timeout" {
				f.request.Limits.TimeoutSec = 1
			}
			f.h = New(f.config)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, err := f.h.Start(ctx, f.request, f.env)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-resolved:
			case <-ctx.Done():
				t.Fatal("store not resolved")
			}
			if mode == "cancel" {
				_ = f.h.Cancel(id)
			}
			result, err := f.h.Result(id)
			want := adapter.ErrorCancelled
			if mode == "timeout" {
				want = adapter.ErrorTimeout
			}
			if err != nil || result.ErrorClass == nil || *result.ErrorClass != want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.processes) != 0 {
				t.Fatal("CLI started while another execution held the lock")
			}
		})
	}
}

func TestAuthenticationSecondExecutionReadsAfterFirstWriteback(t *testing.T) {
	first := newFixture(t, "refresh-ignore-term")
	second := newFixture(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := first.h.Start(ctx, first.request, first.env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.h.Cancel(id); _, _ = first.h.Result(id) })
	lines, _ := first.h.Logs(id)
	waitAuthenticationCLI(t, ctx, first.env.Worktree, "started")
	go func() {
		for range lines {
		}
	}()
	resolved, launched := make(chan struct{}), make(chan struct{}, 2)
	second.config.ResolveAuthentication = func(adapter.HarnessRequest) (string, error) { close(resolved); return first.store, nil }
	launch := second.config.Launch
	second.config.Launch = func(ctx context.Context, env adapter.SandboxEnv, args []string, out, stderr io.Writer) (sandbox.Process, error) {
		data, err := os.ReadFile(filepath.Join(env.ConfigDir, "auth.json"))
		if err != nil || string(data) != `{"token":"private-refreshed-token"}` {
			t.Error("second CLI received stale credential")
		}
		launched <- struct{}{}
		return launch(ctx, env, args, out, stderr)
	}
	second.h = New(second.config)
	next, err := second.h.Start(ctx, second.request, second.env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.h.Cancel(next); _, _ = second.h.Result(next) })
	select {
	case <-resolved:
	case <-ctx.Done():
		t.Fatal("second did not resolve store")
	}
	select {
	case <-launched:
		t.Fatal("second launched while first held lock")
	case <-time.After(60 * time.Millisecond):
	}
	// Only first's copy has refreshed bytes until termination and writeback.
	data, err := os.ReadFile(first.store)
	if err != nil || string(data) != `{"fake":true}` {
		t.Fatal("writeback occurred before stop")
	}
	_ = first.h.Cancel(id)
	result, err := first.h.Result(id)
	if err != nil || result.ErrorClass == nil || *result.ErrorClass != adapter.ErrorCancelled {
		t.Fatalf("first=%+v err=%v", result, err)
	}
	result, err = second.h.Result(next)
	if err != nil || result.Status != adapter.ResultSucceeded {
		t.Fatalf("second=%+v err=%v", result, err)
	}
}

type authenticationDelayedProcess struct {
	done    <-chan struct{}
	stopped chan<- struct{}
}

func (p authenticationDelayedProcess) Wait() error { <-p.done; return nil }
func (p authenticationDelayedProcess) Stop() error {
	select {
	case p.stopped <- struct{}{}:
	default:
	}
	return nil
}
func (p authenticationDelayedProcess) Kill() error           { return p.Stop() }
func (p authenticationDelayedProcess) ExitCode() (int, bool) { return 0, true }

func TestAuthenticationWritebackWaitsForLastWriter(t *testing.T) {
	f := newFixture(t, "success")
	finished, stopped, started := make(chan struct{}), make(chan struct{}, 2), make(chan string, 1)
	var once sync.Once
	defer once.Do(func() { close(finished) })
	launch := f.config.Launch
	calls := 0
	f.config.Launch = func(ctx context.Context, env adapter.SandboxEnv, args []string, out, stderr io.Writer) (sandbox.Process, error) {
		calls++
		if calls == 1 {
			return launch(ctx, env, args, out, stderr)
		}
		started <- env.ConfigDir
		return authenticationDelayedProcess{done: finished, stopped: stopped}, nil
	}
	f.h = New(f.config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := f.h.Start(ctx, f.request, f.env)
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	select {
	case dir = <-started:
	case <-ctx.Done():
		t.Fatal("CLI not started")
	}
	_ = f.h.Cancel(id)
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("stop not requested")
	}
	initial, err := os.ReadFile(f.store)
	if err != nil || string(initial) != `{"fake":true}` {
		t.Fatal("store changed before last writer stopped")
	}
	status, err := f.h.Status(id)
	if err != nil || status != adapter.SessionRunning {
		t.Fatal("result published before writer stop")
	}
	// The final token refresh occurs while termination is still in progress.
	final := []byte(`{"token":"private-last-write"}`)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), final, 0600); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(finished) })
	result, err := f.h.Result(id)
	if err != nil || result.ErrorClass == nil || *result.ErrorClass != adapter.ErrorCancelled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(f.store)
	if err != nil || !bytes.Equal(got, final) {
		t.Fatal("last token refresh lost", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("working copy survived Result")
	}
}

// Use the fake CLI's explicit marker to synchronize cancellation independently
// of asynchronous log publication.
func waitAuthenticationCLI(t *testing.T, ctx context.Context, root, marker string) {
	t.Helper()
	for {
		// Creation precedes WriteFile's payload write. Cancelling on an empty
		// marker can kill the helper before its PID/name has been published.
		if data, err := os.ReadFile(filepath.Join(root, marker)); err == nil && len(data) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("CLI did not reach " + marker)
		case <-time.After(time.Millisecond):
		}
	}
}
