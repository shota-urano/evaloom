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

// The fake CLIs use distinct initial, preflight and final credentials. They print
// known values during execution and final values without LF at command exit.
// No Docker, installed harness or model API is used.
func TestAuthenticationExcludedBeforeRunnerFilesArtifactsAndSSE(t *testing.T) {
	for _, harness := range []string{"codex", "claude_code"} {
		for _, outcome := range []string{"success", "failure", "cancel", "timeout", "preflight_failure", "invalid_json", "oversize", "symlink", "preflight_invalid_json", "preflight_oversize", "preflight_symlink"} {
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
						"PHASE=" + phase, "HARNESS=" + harness, "OUTCOME=" + outcome, "STORE=" + store}
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
				// The CLI prints ordinary output and then stays alive until released.
				// Assert storage AND event delivery while it is still running.
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
				wantDiagnostic := "ordinary diagnostic"
				if strings.HasPrefix(outcome, "preflight_") {
					wantDiagnostic = "ordinary preflight diagnostic"
				}
				streamDeadline := time.After(500 * time.Millisecond)
				for {
					live, err := os.ReadFile(log.Path())
					if err != nil {
						t.Fatal(err)
					}
					var count int
					err = f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='log'
						AND json_extract(payload, '$.text')=?`, wantDiagnostic).Scan(&count)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(live), wantDiagnostic) && count > 0 {
						break
					}
					select {
					case <-streamDeadline:
						t.Fatal("ordinary diagnostic is not stored and delivered while CLI is running (Streaming=true)")
					case <-time.After(time.Millisecond):
					}
				}
				if status, err := h.Status(id); err != nil || status != adapter.SessionRunning {
					t.Fatal("CLI ended before streaming assertion", err)
				}
				storedBeforeExit, err := os.ReadFile(store)
				if err != nil || string(storedBeforeExit) != initial {
					t.Fatal("command-boundary recovery persisted credentials before Execution ended")
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
				if strings.Contains(result.Summary, "private-") {
					t.Fatal("authentication leaked into summary")
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
				if !strings.Contains(string(local), wantDiagnostic) {
					t.Fatal("ordinary output lost")
				}
				if strings.HasPrefix(outcome, "preflight_") && outcome != "preflight_failure" {
					if strings.Contains(string(local), "suppressed after failed recovery") ||
						!strings.Contains(string(local), "authentication log recovery failed:") {
						t.Fatal("CLI output released after failed preflight recovery, or missing safe diagnostic")
					}
				}
				stored, err := os.ReadFile(store)
				if err != nil {
					t.Fatal(err)
				}
				if invalid {
					if string(stored) != initial || !strings.Contains(string(local), "authentication writeback skipped:") {
						t.Fatal("invalid writeback lost store/reason")
					}
				} else {
					wantToken := "private-refreshed-token"
					if outcome == "preflight_failure" {
						wantToken = "private-preflight-token"
					}
					if !strings.Contains(string(stored), wantToken) {
						t.Fatal("refresh not persisted")
					}
				}
			})
		}
	}
}

const authenticationLogCLI = `#!/bin/sh
set -eu
if [ "$PHASE" = preflight ]; then
 test -f "$AUTH_FILE"
 printf '{"type":"auth.debug","credentials":'
 cat "$AUTH_FILE"
 printf '}\n'
 cat "$AUTH_FILE" >&2
 printf '\nprivate-original-token private-original-refresh\nordinary preflight diagnostic\n' >&2
 printf '%s' '{"token":"private-preflight-token","nested":{"refresh":"private-preflight-refresh"}}' > "$AUTH_FILE"
 case "$OUTCOME" in
  preflight_invalid_json) printf '%s' '{"token":"private-invalid' > "$AUTH_FILE" ;;
  preflight_oversize) dd if=/dev/zero of="$AUTH_FILE" bs=65537 count=1 2>/dev/null ;;
  preflight_symlink) rm -f "$AUTH_FILE"; ln -s "$STORE" "$AUTH_FILE" ;;
 esac
 if [ "$OUTCOME" != preflight_failure ]; then exit 0; fi
else
 case "$OUTCOME" in
  preflight_invalid_json|preflight_oversize|preflight_symlink)
   # Even a successful later read must not re-enable output after failed recovery.
   printf 'private-unrecoverable\nsuppressed after failed recovery\n' >&2
   rm -f "$AUTH_FILE" ;;
  *)
   printf '{"type":"auth.debug","credentials":'
   cat "$AUTH_FILE"
   printf '}\n'
   cat "$AUTH_FILE" >&2
   printf '\nprivate-original-token private-original-refresh private-preflight-token private-preflight-refresh\n' >&2 ;;
 esac
 printf 'ordinary diagnostic\n' >&2
 printf '%s' '{"token":"private-refreshed-token","nested":{"refresh":"private-refreshed-refresh"}}' > "$AUTH_FILE"
 # Unknown values printed while a command is running are outside the guarantee.
 # A final partial line is flushed only after the command-boundary reread.
 printf 'private-refreshed-token' >&2
 case "$OUTCOME" in
  invalid_json) printf '%s' '{"token":"private-unrecoverable' > "$AUTH_FILE" ;;
  oversize) dd if=/dev/zero of="$AUTH_FILE" bs=65537 count=1 2>/dev/null ;;
  symlink) rm -f "$AUTH_FILE"; ln -s "$STORE" "$AUTH_FILE" ;;
 esac
fi
touch "$ROOT/printed"
while [ ! -f "$ROOT/release" ]; do sleep 0.01; done
case "$OUTCOME" in failure|preflight_failure) exit 7 ;; esac
if [ "$HARNESS" = codex ]; then
 printf '%s\n' '{"type":"turn.completed"}'
 printf '%s' '{"type":"item.completed","item":{"type":"agent_message","text":"private-original-token private-preflight-token private-refreshed-token"}}'
else
 printf '%s' '{"type":"result","subtype":"success","is_error":false,"result":"private-original-token private-preflight-token private-refreshed-token"}'
fi
`
