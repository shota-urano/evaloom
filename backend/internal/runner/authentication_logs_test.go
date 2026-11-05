package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// The fake CLIs actually print credentials, including future refreshed values
// before updating the file. No Docker, installed harness or model API is used.
func TestAuthenticationExcludedBeforeRunnerFilesArtifactsAndSSE(t *testing.T) {
	for _, harness := range []string{"codex", "claude_code"} {
		for _, outcome := range []string{"success", "failure", "cancel", "timeout", "preflight_failure", "invalid_json", "oversize", "symlink"} {
			t.Run(harness+"/"+outcome, func(t *testing.T) {
				f := newLogControlPlane(t)
				r, err := New(f.config)
				if err != nil {
					t.Fatal(err)
				}
				root := t.TempDir()
				variable, authName := "CODEX_HOME", "auth.json"
				if harness == "claude_code" {
					variable, authName = "CLAUDE_CONFIG_DIR", ".credentials.json"
				}
				settings, err := harnessenv.Create(context.Background(), harnessenv.Config{
					Root: root, CommitSHA: strings.Repeat("a", 40), ConfigVariable: variable,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = settings.Close() })
				store := filepath.Join(root, "stored-auth")
				initial := `{"token":"private-original-token","nested":{"refresh":"private-original-refresh"}}`
				if err := os.WriteFile(store, []byte(initial), 0600); err != nil {
					t.Fatal(err)
				}
				fake := filepath.Join(root, "fake-cli")
				if err := os.WriteFile(fake, []byte(authenticationLogCLI), 0700); err != nil {
					t.Fatal(err)
				}
				launch := func(ctx context.Context, env adapter.SandboxEnv, args []string, out, stderr io.Writer) (sandbox.Process, error) {
					phase := "exec"
					if slices.Contains(args, "status") {
						phase = "preflight"
					}
					cmd := exec.CommandContext(ctx, fake)
					cmd.Dir, cmd.Stdout, cmd.Stderr = env.Worktree, out, stderr
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, variable + "=" + env.ConfigDir,
						"AUTH_FILE=" + filepath.Join(env.ConfigDir, authName), "ROOT=" + root,
						"PHASE=" + phase, "OUTCOME=" + outcome, "STORE=" + store}
					if err := cmd.Start(); err != nil {
						return nil, err
					}
					return &codexTestProcess{cmd: cmd}, nil
				}
				resolve := func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil }
				auth := func(adapter.HarnessRequest) (string, error) { return store, nil }
				var h adapter.Adapter
				if harness == "codex" {
					h = codex.New(codex.Config{Launch: launch, ResolveConfig: resolve, ResolveAuthentication: auth,
						CancelGrace: 20 * time.Millisecond, Configure: func(context.Context, adapter.HarnessRequest) ([]string, error) { return nil, nil }})
				} else {
					h = claudecode.New(claudecode.Config{Launch: launch, ResolveConfig: resolve, ResolveAuthentication: auth,
						CancelGrace: 20 * time.Millisecond, Configure: func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
							return json.RawMessage(`{}`), nil
						}})
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				request := adapter.HarnessRequest{ContractVersion: adapter.ContractVersion, ExecutionID: logExecution,
					Task: adapter.RequestTask{AcceptanceCriteria: "log exclusion"}, Project: adapter.RequestProject{Workdir: root},
					Profile: adapter.RequestProfile{ModelID: "fake", Definition: json.RawMessage(`{}`)}, Context: adapter.RequestContext{RevisionIDs: []string{}}}
				if outcome == "timeout" {
					request.Limits.TimeoutSec = 1
				}
				log, err := r.NewLog(logExecution, "harness", nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = log.Finish(false) })
				delivered := startLogDelivery(t, log)
				id, err := h.Start(ctx, request, adapter.SandboxEnv{Worktree: root, ConfigDir: settings.ConfigDir(), Env: settings.Env()})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = h.Cancel(id); _, _ = h.Result(id) })
				lines, err := h.Logs(id)
				if err != nil {
					t.Fatal(err)
				}
				source, sink := io.Pipe()
				defer source.Close()
				defer sink.Close()
				readDone := make(chan error, 1)
				go func() { readDone <- log.Read(source) }()
				collected := make(chan []string, 1)
				go func() {
					var texts []string
					for line := range lines {
						texts = append(texts, line.Text)
						_, _ = io.WriteString(sink, line.Text+"\n")
					}
					_ = sink.Close()
					collected <- texts
				}()
				// The fake CLI has already printed old/new credentials but remains
				// alive. Neither the local file nor event delivery may contain them.
				for {
					if _, err := os.Stat(filepath.Join(root, "printed")); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("fake CLI did not print")
					case <-time.After(time.Millisecond):
					}
				}
				info, err := os.Stat(log.Path())
				if err != nil || info.Size() != 0 {
					t.Fatal("CLI output released before recovery")
				}
				var count int
				if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='log'`).Scan(&count); err != nil || count != 0 {
					t.Fatal("CLI output delivered before recovery")
				}
				if outcome == "cancel" {
					_ = h.Cancel(id)
				} else if outcome != "timeout" {
					if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				result, err := h.Result(id)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]adapter.ErrorClass{"failure": adapter.ErrorExecutionFailed, "cancel": adapter.ErrorCancelled,
					"timeout": adapter.ErrorTimeout, "preflight_failure": adapter.ErrorConfig}[outcome]
				if want == "" {
					if result.Status != adapter.ResultSucceeded {
						t.Fatalf("result=%+v", result)
					}
				} else if result.ErrorClass == nil || *result.ErrorClass != want {
					t.Fatalf("result=%+v", result)
				}
				if err := await(t, readDone); err != nil {
					t.Fatal(err)
				}
				texts := <-collected
				if err := log.Finish(false); err != nil {
					t.Fatal(err)
				}
				if err := await(t, delivered); err != nil {
					t.Fatal(err)
				}
				local, err := os.ReadFile(log.Path())
				if err != nil {
					t.Fatal(err)
				}
				var savedPath string
				var redacted int
				if err := f.db.QueryRow(`SELECT storage_path, redacted FROM artifacts WHERE id=?`, log.id).Scan(&savedPath, &redacted); err != nil || redacted != 1 {
					t.Fatal("missing redacted artifact metadata", err)
				}
				saved, err := os.ReadFile(filepath.Join(f.dataDir, savedPath))
				if err != nil || string(local) != string(saved) {
					t.Fatal("saved artifact differs", err)
				}
				if strings.Contains(string(local), "private-") || !strings.Contains(string(local), "[REDACTED]") {
					t.Fatal("authentication leaked into Runner file/artifact")
				}
				// Verify the actual HTTP SSE frames, including replayed payloads.
				_, events := f.stream(ctx)
				for _, text := range texts {
					if strings.Contains(text, "private-") {
						t.Fatal("authentication leaked into adapter Logs")
					}
					readLogFrame(t, events, "harness", text)
				}
				invalid := outcome == "invalid_json" || outcome == "oversize" || outcome == "symlink"
				if !invalid && !strings.Contains(string(local), "ordinary diagnostic") {
					t.Fatal("ordinary output lost")
				}
				stored, err := os.ReadFile(store)
				if err != nil {
					t.Fatal(err)
				}
				if invalid {
					if string(stored) != initial || !strings.Contains(string(local), "authentication writeback skipped:") {
						t.Fatal("invalid writeback lost store/reason")
					}
				} else if !strings.Contains(string(stored), "private-refreshed-token") {
					t.Fatal("refresh not persisted")
				}
			})
		}
	}
}

const authenticationLogCLI = `#!/bin/sh
set -eu
test -f "$AUTH_FILE"
printf '{"type":"auth.debug","credentials":'
cat "$AUTH_FILE"
printf '}\n'
cat "$AUTH_FILE" >&2
printf '\nprivate-original-token private-original-refresh\n' >&2
# A future refreshed value can be printed before the file is changed.
printf '%s\n' 'private-refreshed-token private-refreshed-refresh' >&2
if [ "$PHASE" = preflight ] && [ "$OUTCOME" != preflight_failure ]; then exit 0; fi
printf '%s' '{"token":"private-refreshed-token","nested":{"refresh":"private-refreshed-refresh"}}' > "$AUTH_FILE"
printf '{"type":"auth.debug","credentials":'
cat "$AUTH_FILE"
printf '}\n'
cat "$AUTH_FILE" >&2
printf '\nordinary diagnostic\n' >&2
case "$OUTCOME" in
 invalid_json) printf '%s' '{"token":"private-unrecoverable' > "$AUTH_FILE"; printf 'private-unrecoverable\n' >&2 ;;
 oversize) dd if=/dev/zero of="$AUTH_FILE" bs=65537 count=1 2>/dev/null; printf 'private-unrecoverable\n' >&2 ;;
 symlink) rm -f "$AUTH_FILE"; ln -s "$STORE" "$AUTH_FILE"; printf 'private-unrecoverable\n' >&2 ;;
esac
touch "$ROOT/printed"
while [ ! -f "$ROOT/release" ]; do sleep 0.01; done
case "$OUTCOME" in failure|preflight_failure) exit 7 ;; esac
printf '%s\n' '{"type":"turn.completed"}' '{"type":"result","subtype":"success","is_error":false,"result":"ordinary result"}'
`
