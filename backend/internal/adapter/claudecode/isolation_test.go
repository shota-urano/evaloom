package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// isolationLaunch records every CLI argv and answers the execution command
// with the given stream-json lines.
func isolationLaunch(calls *[][]string, stream string) Launch {
	return func(_ context.Context, _ adapter.SandboxEnv, args []string, out, _ io.Writer) (sandbox.Process, error) {
		*calls = append(*calls, append([]string{}, args...))
		if slices.Contains(args, "-p") {
			_, _ = io.WriteString(out, stream)
		}
		return permissionProcess{}, nil
	}
}

const isolationResult = `{"type":"result","session_id":"isolation-session","subtype":"success","is_error":false}` + "\n"

func runAdapter(t *testing.T, h adapter.Adapter, start func(context.Context) (adapter.SessionID, error)) (adapter.HarnessResult, []adapter.LogLine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := h.Logs(id)
	if err != nil {
		t.Fatal(err)
	}
	var logs []adapter.LogLine
	for line := range lines {
		logs = append(logs, line)
	}
	r, err := h.Result(id)
	if err != nil {
		t.Fatal(err)
	}
	return r, logs
}

// Both the work stage and the routing proposal pass the account isolation
// settings and --strict-mcp-config on every CLI command (preflight and execution).
func TestClaudeCodeAccountIsolationArguments(t *testing.T) {
	for _, stage := range []string{"work", "proposal"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, "success")
			var calls [][]string
			f.config.Launch = isolationLaunch(&calls, isolationResult)
			h := New(f.config)
			outPath := filepath.Join(t.TempDir(), "out", "proposal.json")
			start := func(ctx context.Context) (adapter.SessionID, error) { return h.Start(ctx, f.request, f.env) }
			if stage == "proposal" {
				env := f.env
				env.Worktree = ""
				req := adapter.ProposalRequest{Model: "test-model", InputPath: filepath.Join(t.TempDir(), "input.json"), OutPath: outPath}
				start = func(ctx context.Context) (adapter.SessionID, error) {
					return h.(adapter.ProposalAdapter).StartProposal(ctx, req, env)
				}
			}
			r, logs := runAdapter(t, h, start)
			if r.Status != adapter.ResultSucceeded || len(calls) != 2 {
				t.Fatalf("result=%+v calls=%d logs=%+v", r, len(calls), logs)
			}
			for _, args := range calls {
				if !slices.Contains(args, "--strict-mcp-config") {
					t.Fatalf("missing --strict-mcp-config: %q", args)
				}
				i := slices.Index(args, "--settings")
				if i < 0 || i+1 >= len(args) {
					t.Fatalf("no settings argument: %q", args)
				}
				var settings map[string]json.RawMessage
				if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
					t.Fatal(err)
				}
				for key, want := range map[string]string{"disableClaudeAiConnectors": "true", "syncClaudeAiSkills": "false", "syncClaudeAiPlugins": "false", "autoMemoryEnabled": "false"} {
					if string(settings[key]) != want {
						t.Fatalf("%s=%s want %s in %s", key, settings[key], want, args[i+1])
					}
				}
				// toolPermissions are passed exactly as before the isolation keys.
				wantPermissions := `{"allow":["Bash","Edit"],"deny":["WebFetch"]}`
				if stage == "proposal" {
					wantPermissions = `{"allow":["Read","Edit(//` + strings.TrimPrefix(outPath, "/") + `)"]}`
				}
				if string(settings["permissions"]) != wantPermissions || len(settings) != 5 {
					t.Fatalf("settings=%s", args[i+1])
				}
			}
		})
	}
}

// The first system init must not show claude.ai connectors or synced skills.
func TestClaudeCodeAccountStateInSystemInitFails(t *testing.T) {
	cases := []struct {
		name, init string
		breach     bool
	}{
		{"normal", `{"type":"system","subtype":"init","session_id":"isolation-session","mcp_servers":[{"name":"local","status":"connected"}],"skills":["review","debug"]}`, false},
		{"no_lists", `{"type":"system","subtype":"init","session_id":"isolation-session"}`, false},
		{"connector", `{"type":"system","subtype":"init","session_id":"isolation-session","mcp_servers":[{"name":"claude.ai Gmail","status":"needs-auth","source":"claudeai"}],"skills":[]}`, true},
		{"synced_skill", `{"type":"system","subtype":"init","session_id":"isolation-session","mcp_servers":[],"skills":["review","anthropic-skills:pdf"]}`, true},
		{"synced_skill_object", `{"type":"system","subtype":"init","session_id":"isolation-session","skills":[{"name":"anthropic-skills:docx"}]}`, true},
	}
	for _, stage := range []string{"work", "proposal"} {
		for _, tc := range cases {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				f := newFixture(t, "success")
				var calls [][]string
				f.config.Launch = isolationLaunch(&calls, tc.init+"\n"+isolationResult)
				h := New(f.config)
				start := func(ctx context.Context) (adapter.SessionID, error) { return h.Start(ctx, f.request, f.env) }
				if stage == "proposal" {
					env := f.env
					env.Worktree = ""
					dir := t.TempDir()
					req := adapter.ProposalRequest{Model: "test-model", InputPath: filepath.Join(dir, "input.json"), OutPath: filepath.Join(dir, "proposal.json")}
					start = func(ctx context.Context) (adapter.SessionID, error) {
						return h.(adapter.ProposalAdapter).StartProposal(ctx, req, env)
					}
				}
				r, logs := runAdapter(t, h, start)
				logged := slices.ContainsFunc(logs, func(l adapter.LogLine) bool {
					return l.Stream == "stderr" && strings.Contains(l.Text, "isolation breach")
				})
				if !tc.breach {
					if r.Status != adapter.ResultSucceeded || logged {
						t.Fatalf("normal init rejected: result=%+v logs=%+v", r, logs)
					}
					return
				}
				if r.Status != adapter.ResultFailed || r.ErrorClass == nil || *r.ErrorClass != adapter.ErrorExecutionFailed || !logged {
					t.Fatalf("account state accepted: result=%+v logs=%+v", r, logs)
				}
			})
		}
	}
}

// Only the first system init is inspected; later init lines keep today's handling.
func TestClaudeCodeAccountStateOnlyFirstInit(t *testing.T) {
	e := &events{}
	e.read([]byte(`{"type":"system","subtype":"init","session_id":"s","skills":["review"]}`))
	e.read([]byte(`{"type":"system","subtype":"init","session_id":"s","skills":["anthropic-skills:pdf"]}`))
	if e.isolationBreach != "" || e.invalid {
		t.Fatalf("events=%+v", e)
	}
	e = &events{}
	e.read([]byte(`{"type":"system","subtype":"init","session_id":"s","mcp_servers":[{"name":"x","source":"claudeai"}]}`))
	if e.isolationBreach == "" || e.invalid {
		t.Fatalf("events=%+v", e)
	}
}
