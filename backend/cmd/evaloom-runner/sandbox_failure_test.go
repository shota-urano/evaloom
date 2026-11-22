package main

import (
	"database/sql"

	"encoding/json"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const fakeDaemonMessage = `Error response from daemon: invalid mount config for type "bind": bind source path does not exist: /socket_mnt/fixture`

// evaloom-6pi: when the runtime refuses to inspect the image, create the
// container, or report its state after the stage ran, the reason reaches the
// stage log (stream=runner), the log artifact and executions.error_message,
// with secrets excluded and the length bounded. error_class stays
// sandbox_error. Real Docker is out of scope; only the runtime CLI is fake.
func TestSandboxFailureReasonIsRecorded(t *testing.T) {
	for _, mode := range []string{"image", "create", "wait", "work-wait", "relay-create", "relay-volume"} {
		t.Run(mode, func(t *testing.T) {
			f := newExecutionFixture(t, "success")
			stage := "prepare"
			if mode == "work-wait" {
				// A work Adapter on Runner's real Launch wiring. Like the
				// production Adapters, it reduces a Sandbox Wait failure to an
				// error class and drops the error (evaloom-6pi evaluation).
				// codex/claude_code need the proxy network, which this fake
				// runtime does not provide.
				stage = "work"
				f.workAdapter = func(w runner.WorkAdapterConfig) adapter.Adapter {
					a, err := securitytest.NewWorkProbe(w, json.RawMessage(`{}`))
					if err != nil {
						t.Error(err)
					}
					return a
				}
			}
			f.config.Container.RuntimeEnv["EVALOOM_FAKE_FAIL"] = mode
			f.config.Container.RuntimeEnv["EVALOOM_FAKE_TOKEN_FILE"] = f.config.TokenFile
			data, _ := json.Marshal(f.config)
			if err := os.WriteFile(f.configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			token, err := os.ReadFile(f.config.TokenFile)
			if err != nil || len(token) == 0 {
				t.Fatalf("runner token: %v", err)
			}
			f.start()
			f.waitStatus("failed")
			if stage == "work" {
				f.stages([]string{"prepare-1", "work-1"})
			} else {
				f.stages([]string{"prepare-1"})
			}
			var class, stageClass string
			var message sql.NullString
			if err := f.db.QueryRow(`SELECT e.error_class, e.error_message, r.error_class FROM executions e
				JOIN stage_runs r ON r.execution_id = e.id WHERE e.id = ? AND r.stage = ?`, f.id, stage).Scan(&class, &message, &stageClass); err != nil {
				t.Fatal(err)
			}
			if class != "sandbox_error" || stageClass != "sandbox_error" {
				t.Fatalf("error class %s / stage %s; want sandbox_error; msg=%q", class, stageClass, message.String)
			}
			requireSafeReason(t, "error_message", message.String, string(token))
			if utf8.RuneCountInString(message.String) > 1000 {
				t.Fatalf("error_message is not bounded: %d characters", utf8.RuneCountInString(message.String))
			}

			// The Runner's own line is a stream=runner log event.
			rows, err := f.db.Query(`SELECT json_extract(payload, '$.stream'), json_extract(payload, '$.text') FROM execution_events
				WHERE execution_id = ? AND type = 'log' ORDER BY id`, f.id)
			if err != nil {
				t.Fatal(err)
			}
			var runnerLines []string
			for rows.Next() {
				var stream, text string
				if err := rows.Scan(&stream, &text); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(text, string(token)) || strings.Contains(text, "fixture-bearer-value") {
					t.Fatalf("log event leaked a secret: %s", text)
				}
				if stream == "runner" && strings.HasPrefix(text, "sandbox_error: ") {
					runnerLines = append(runnerLines, text)
				}
			}
			rows.Close()
			if len(runnerLines) != 1 {
				t.Fatalf("runner reason lines %q; want 1", runnerLines)
			}
			requireSafeReason(t, "log event", runnerLines[0], string(token))
			if utf8.RuneCountInString(runnerLines[0]) > 1000 {
				t.Fatalf("log line is not bounded: %d characters", utf8.RuneCountInString(runnerLines[0]))
			}

			// The stage's log artifact holds the same line and is not empty.
			var storage string
			var size, redacted int
			if err := f.db.QueryRow(`SELECT storage_path, size_bytes, redacted FROM artifacts
				WHERE execution_id = ? AND kind = 'log' AND stage = ?`, f.id, stage).Scan(&storage, &size, &redacted); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(filepath.Dir(f.config.DataDir), "control-data", storage))
			if err != nil {
				t.Fatal(err)
			}
			if size == 0 || len(saved) == 0 || redacted != 1 {
				t.Fatalf("log artifact size=%d saved=%d redacted=%d", size, len(saved), redacted)
			}
			requireSafeReason(t, "log artifact", string(saved), string(token))
			if strings.HasPrefix(mode, "relay-") {
				// spec 09 3.6 item 6: no stage container without its channel.
				created, _ := filepath.Glob(filepath.Join(f.config.Container.RuntimeEnv["EVALOOM_FAKE_RUNTIME"], "*.json"))
				for _, path := range created {
					if !strings.HasSuffix(path, "-relay.json") {
						t.Fatalf("stage container created without its channel: %s", path)
					}
				}
				if !strings.Contains(message.String, "stage channel relay unconfirmed") {
					t.Fatalf("error_message lacks the channel failure: %q", message.String)
				}
			}
		})
	}
}

func requireSafeReason(t *testing.T, where, text, token string) {
	t.Helper()
	if !strings.Contains(text, "invalid mount config for type") || !strings.Contains(text, "runtime exit code 1") {
		t.Fatalf("%s lacks the runtime reason: %q", where, text)
	}
	if strings.Contains(text, token) || strings.Contains(text, "fixture-bearer-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("%s did not exclude secrets: %q", where, text)
	}
}
