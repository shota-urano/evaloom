package securitytest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func TestConfigurationCanaryPlacement(t *testing.T) {
	for _, cli := range configurationCLIs {
		t.Run(cli.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			f, err := NewFixture(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			a, b := NewCanary(), NewCanary()
			personalInstructions(t, f.Home, a)
			profile, files := configurationProfile(t, ctx, f, cli, "fixture-model", b)
			env := configurationEnvironment(t, ctx, filepath.Join(root, "configuration"), cli, profile, files)
			for _, name := range personalInstructionPaths {
				data, err := os.ReadFile(filepath.Join(f.Home, name))
				if err != nil || !strings.Contains(string(data), a) || strings.Contains(string(data), b) {
					t.Fatalf("personal instruction %s: %v", name, err)
				}
			}
			placed, err := env.Files(ctx)
			if err != nil || len(placed) != 2 {
				t.Fatalf("placed=%v err=%v", placed, err)
			}
			data, err := os.ReadFile(filepath.Join(env.ConfigDir(), cli.instruction))
			if err != nil || !strings.Contains(string(data), b) || strings.Contains(string(data), a) || strings.Contains(string(data), "uncommitted replacement") {
				t.Fatalf("dedicated instruction not pinned: %v", err)
			}
			if vars := env.Env(); len(vars) != 1 || vars[cli.variable] != env.ConfigDir() {
				t.Fatalf("configuration vars=%v", vars)
			}
			request, err := json.Marshal(configurationRequest(f, profile))
			if err != nil || strings.Contains(string(request), a) || strings.Contains(string(request), b) {
				t.Fatal("request supplies a canary instead of testing file loading")
			}
			for _, file := range placed {
				if file.SourceCommitSHA != profile.CommitSHA || strings.Contains(file.Content, a) {
					t.Fatal("dedicated configuration is not isolated/pinned")
				}
			}
		})
	}
}

func TestConfigurationVerdict(t *testing.T) {
	a, b := "canary-A", "canary-B"
	for _, tc := range []struct {
		name, summary, stdout, stderr string
		failed                        bool
		want                          string
	}{
		{"pass", b, "event", "diagnostic", false, ""},
		{"empty", "", "", "", false, "B missing"},
		{"echo_is_not_evidence", "done", b, "", false, "B missing"},
		{"A_in_summary", a + b, "", "", false, "A appeared"},
		{"A_in_stdout", b, a, "", false, "A appeared"},
		{"A_in_stderr", b, "", a, false, "A appeared"},
		{"escaped_A", b, `{"text":"canary-\u0041"}`, "", false, "A appeared"},
		{"nested_escaped_A", b, `{"event":[{"text":"canary-\u0041"}]}`, "", false, "A appeared"},
		{"reported_personal_config", b, `{"loadedFiles":["/personal/.codex/AGENTS.md"]}`, "", false, "HOME path appeared"},
		{"escaped_personal_config", b, `{"loadedFiles":["\u002fpersonal/.claude/CLAUDE.md"]}`, "", false, "HOME path appeared"},
		{"stderr_personal_config", b, "", "/personal/.claude/settings.json", false, "HOME path appeared"},
		{"summary_personal_config", b + " /personal/.codex/AGENTS.md", "", "", false, "HOME path appeared"},
		{"failed_even_with_B", b, "", "", true, "実行不能"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := adapter.HarnessResult{Status: adapter.ResultSucceeded, Summary: tc.summary}
			if tc.failed {
				result.Status = adapter.ResultFailed
			}
			err := configurationVerdict(result, []adapter.LogLine{{Stream: "stdout", Text: tc.stdout}, {Stream: "stderr", Text: tc.stderr}}, a, b, "/personal")
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("verdict=%v want=%q", err, tc.want)
			}
		})
	}
	for _, pair := range [][2]string{{"", b}, {a, ""}, {a, a}, {a, a + "suffix"}} {
		if configurationVerdict(adapter.HarnessResult{Status: adapter.ResultSucceeded, Summary: b}, nil, pair[0], pair[1], "/personal") == nil {
			t.Fatal("accepted invalid canaries")
		}
	}
}

func TestConfigurationAccessVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       bool
	}{
		{"denied", `{"dedicatedReadable":true,"homeIsolated":true,"accesses":[{"path":"/personal","denied":true}]}`, true},
		{"readable", `{"dedicatedReadable":true,"homeIsolated":true,"accesses":[{"path":"/personal","denied":false}]}`, false},
		{"missing_result", `{"dedicatedReadable":true,"homeIsolated":true,"accesses":[{"path":"/personal"}]}`, false},
		{"wrong_path", `{"dedicatedReadable":true,"homeIsolated":true,"accesses":[{"path":"/other","denied":true}]}`, false},
		{"missing_access", `{"dedicatedReadable":true,"homeIsolated":true,"accesses":[]}`, false},
		{"no_positive_control", `{"homeIsolated":true,"accesses":[{"path":"/personal","denied":true}]}`, false},
		{"wrong_HOME", `{"dedicatedReadable":true,"accesses":[{"path":"/personal","denied":true}]}`, false},
		{"missing_file", "", false}, {"invalid_JSON", "{", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := configurationAccessVerdict([]byte(tc.data), []string{"/personal"}); (err == nil) != tc.want {
				t.Fatalf("verdict=%v wantPass=%t", err, tc.want)
			}
		})
	}
}

// This is a host-side test of the script/oracle, not a Sandbox isolation test.
// A readable disposable HOME must be rejected; ENOENT must produce complete
// evidence. Only the security_model test proves the product Sandbox boundary.
func TestConfigurationAccessScript(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home, dedicated := filepath.Join(root, "personal"), filepath.Join(root, "dedicated", "AGENTS.md")
	personalInstructions(t, home, NewCanary())
	configurationWrite(t, dedicated, "dedicated instructions\n")
	run := func(home, dedicated string) ([]byte, []string, error) {
		paths := []string{home}
		for _, name := range personalInstructionPaths {
			paths = append(paths, filepath.Join(home, name))
		}
		args := append([]string{"-c", configurationAccessScript, home, dedicated}, paths...)
		cmd := exec.CommandContext(ctx, "python3", args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(root, "sandbox-home"), "PYTHONDONTWRITEBYTECODE=1"}
		data, err := cmd.CombinedOutput()
		return data, paths, err
	}
	data, paths, err := run(home, dedicated)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, data)
	}
	if err := configurationAccessVerdict(data, paths); err == nil || !strings.Contains(err.Error(), "accessible") {
		t.Fatalf("readable personal HOME was accepted: %v", err)
	}
	data, paths, err = run(filepath.Join(root, "absent-home"), dedicated)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, data)
	}
	if err := configurationAccessVerdict(data, paths); err != nil {
		t.Fatal(err)
	}
	if data, _, err := run(home, filepath.Join(root, "missing-instructions")); err == nil {
		t.Fatalf("missing positive control was accepted: %s", data)
	}
}

func TestConfigurationCredentialRequiresExplicitJSON(t *testing.T) {
	for _, cli := range configurationCLIs {
		for _, bad := range []string{"", "null", "{}", "[]", "not-json"} {
			if _, err := configurationCredential(cli, strings.Repeat("a", 40), bad); err == nil || !strings.Contains(err.Error(), "実行不能") {
				t.Fatalf("invalid credential accepted: %v", err)
			}
		}
		file, err := configurationCredential(cli, strings.Repeat("a", 40), `{"fixture":"dummy-secret"}`)
		if err != nil || file.Path != cli.credential || file.ContentHash == "" || file.SourceCommitSHA != strings.Repeat("a", 40) {
			t.Fatalf("credential metadata invalid: %v", err)
		}
	}
}
