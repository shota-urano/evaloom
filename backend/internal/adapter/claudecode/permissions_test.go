package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type permissionProcess struct{}

func (permissionProcess) Wait() error           { return nil }
func (permissionProcess) Stop() error           { return nil }
func (permissionProcess) Kill() error           { return nil }
func (permissionProcess) ExitCode() (int, bool) { return 0, true }

func TestClaudeCodeToolPermissionsRejectInvalidBeforeCLI(t *testing.T) {
	cases := []struct{ name, policy, tools string }{
		{"unknown_key", `{"allow":[]}`, `[]`},
		{"unknown_key_with_deny", `{"deny":[],"extra":false}`, `[]`},
		{"case_sensitive_key", `{"Deny":[]}`, `[]`},
		{"missing_object", ``, `[]`},
		{"null_object", `null`, `[]`},
		{"array_object", `[]`, `[]`},
		{"invalid_json", `{"deny":`, `[]`},
		{"deny_string", `{"deny":"Bash"}`, `[]`},
		{"deny_object", `{"deny":{}}`, `[]`},
		{"deny_null", `{"deny":null}`, `[]`},
		{"deny_empty_string", `{"deny":["Bash",""]}`, `[]`},
		{"deny_number", `{"deny":[1]}`, `[]`},
		{"deny_null_element", `{"deny":[null]}`, `[]`},
		{"allow_string", `{}`, `"Bash"`},
		{"allow_null", `{}`, `null`},
		{"allow_empty_string", `{}`, `["Bash",""]`},
		{"allow_number", `{}`, `[1]`},
		{"allow_null_element", `{}`, `[null]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "success")
			f.request.Profile.Definition = json.RawMessage(fmt.Sprintf(`{"toolConfiguration":{"allowedTools":%s}}`, tc.tools))
			f.config.ResolveToolPermissions = func(_ context.Context, req adapter.HarnessRequest) (json.RawMessage, error) {
				if req.Profile.HarnessID != "harness" {
					t.Error("lost Harness ID")
				}
				return json.RawMessage(tc.policy), nil
			}
			calls := 0
			f.config.Launch = func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error) {
				calls++
				return permissionProcess{}, nil
			}
			f.h = New(f.config)
			_, result, _ := runResult(t, f)
			if result.Status != adapter.ResultFailed || result.ErrorClass == nil || *result.ErrorClass != adapter.ErrorConfig || calls != 0 {
				t.Fatalf("result=%+v CLI calls=%d; want config_error without even authentication CLI", result, calls)
			}
		})
	}
}

func TestClaudeCodeToolPermissionsLaunchArguments(t *testing.T) {
	cases := []struct {
		name, policy string
		allow, deny  []string
	}{
		{"empty_object", `{}`, []string{}, []string{}},
		{"empty_deny", `{"deny":[]}`, []string{}, []string{}},
		{"rules_preserved", `{"deny":["Bash(git push:*)","WebFetch"]}`, []string{"Bash(git push:*)", "Edit", "Bash(echo \"$HOME\":*)"}, []string{"Bash(git push:*)", "WebFetch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "success")
			definition, _ := json.Marshal(map[string]any{"toolConfiguration": map[string]any{"allowedTools": tc.allow}})
			f.request.Profile.Definition = definition
			f.config.ResolveToolPermissions = func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
				return json.RawMessage(tc.policy), nil
			}
			calls := 0
			f.config.Launch = func(_ context.Context, _ adapter.SandboxEnv, args []string, out, _ io.Writer) (sandbox.Process, error) {
				calls++
				i := slices.Index(args, "--settings")
				if i < 0 || i+1 >= len(args) {
					t.Fatalf("no settings argument: %q", args)
				}
				var settings struct {
					Permissions struct{ Allow, Deny []string }
				}
				if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(settings.Permissions.Allow, tc.allow) || !slices.Equal(settings.Permissions.Deny, tc.deny) {
					t.Fatalf("settings=%s", args[i+1])
				}
				if strings.Contains(args[i+1], `"allow":null`) || strings.Contains(args[i+1], `"deny":null`) {
					t.Fatal("empty permissions must be arrays")
				}
				if calls == 2 {
					i := slices.Index(args, "--permission-mode")
					if i < 0 || i+1 >= len(args) || args[i+1] != "dontAsk" {
						t.Fatal("missing dontAsk permission mode")
					}
				}
				if calls == 2 {
					_, _ = io.WriteString(out, `{"type":"result","subtype":"success","is_error":false}`+"\n")
				}
				return permissionProcess{}, nil
			}
			f.h = New(f.config)
			_, result, _ := runResult(t, f)
			if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil || calls != 2 {
				t.Fatalf("result=%+v CLI calls=%d; want preflight and execution", result, calls)
			}
		})
	}
}

func TestContainerLaunchOnlyAdapterDestinations(t *testing.T) {
	expected := []string{"api.anthropic.com:443", "console.anthropic.com:443", "claude.ai:443"}
	for _, network := range []sandbox.NetworkPolicy{sandbox.NetworkHarness, sandbox.NetworkNone, "", sandbox.NetworkSetup} {
		t.Run(string(network), func(t *testing.T) {
			env := adapter.SandboxEnv{Worktree: "/worktree", ConfigDir: "/config", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config"}}
			incoming := []string{"github.com:443", "api.github.com:443", "unrelated.example:80"}
			called := 0
			launch := containerLaunch(sandbox.ContainerConfig{AllowEnv: []string{"CLAUDE_CONFIG_DIR"}}, func(adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
				return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, ConfigDir: env.ConfigDir, Env: env.Env, Network: network, AllowedDestinations: incoming}, nil
			}, func(_ context.Context, _ sandbox.ContainerConfig, spec sandbox.SandboxSpec, _ []string) (sandbox.Process, error) {
				called++
				want := expected
				if network != sandbox.NetworkHarness {
					want = nil
				}
				if !slices.Equal(spec.AllowedDestinations, want) {
					t.Fatalf("Sandbox destinations=%v want=%v", spec.AllowedDestinations, want)
				}
				for _, destination := range spec.AllowedDestinations {
					host, port, err := net.SplitHostPort(destination)
					if err != nil || port != "443" || host == "github.com" || strings.HasSuffix(host, ".github.com") {
						t.Fatalf("forbidden destination %q", destination)
					}
				}
				// Changing a received list cannot change the next execution's policy.
				if len(spec.AllowedDestinations) > 0 {
					spec.AllowedDestinations[0] = "modified.example:443"
				}
				return permissionProcess{}, nil
			})
			for range 2 {
				_, err := launch(context.Background(), env, []string{"cli"}, io.Discard, io.Discard)
				if network == sandbox.NetworkSetup {
					if err == nil || called != 0 {
						t.Fatal("setup policy reached Harness Sandbox")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if network != sandbox.NetworkSetup && called != 2 {
				t.Fatalf("Sandbox calls=%d", called)
			}
			if !slices.Equal(incoming, []string{"github.com:443", "api.github.com:443", "unrelated.example:80"}) {
				t.Fatal("mutated caller-owned destinations")
			}
		})
	}
}
