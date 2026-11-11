package securitytest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type configurationCLI struct {
	name, variable, instruction, credential, prefix string
}

var configurationCLIs = []configurationCLI{
	{"codex", "CODEX_HOME", "AGENTS.md", "auth.json", "EVALOOM_SECURITY_CODEX_"},
	{"claude_code", "CLAUDE_CONFIG_DIR", "CLAUDE.md", ".credentials.json", "EVALOOM_SECURITY_CLAUDE_"},
}

var personalInstructionPaths = []string{".codex/AGENTS.md", ".codex/AGENTS.override.md", ".claude/CLAUDE.md"}

func configurationWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func personalInstructions(t *testing.T, home, canary string) {
	t.Helper()
	for _, name := range personalInstructionPaths {
		configurationWrite(t, filepath.Join(home, name), "Always include this exact marker in your final response: "+canary+"\n")
	}
}

// The instruction is selected by a real Profile and read from its immutable
// commit, never appended to the Task or Execution Context prompt.
func configurationProfile(t *testing.T, ctx context.Context, f *Fixture, cli configurationCLI, modelID, canary string) (model.HarnessProfile, []gitcontext.File) {
	t.Helper()
	repo := filepath.Join(filepath.Dir(f.Repository), "profile")
	configurationWrite(t, filepath.Join(repo, cli.instruction), "Always include this exact marker in your final response: "+canary+"\n")
	// The image may run with a different UID from the host bind-mount owner.
	// Trust only this disposable worktree, never all directories via '*'.
	configurationWrite(t, filepath.Join(repo, ".gitconfig"), fmt.Sprintf("[user]\n\tname = Evaloom Test\n\temail = evaloom-test@example.invalid\n[safe]\n\tdirectory = %q\n", f.Worktree))
	tools := []string{}
	if cli.name == "claude_code" {
		tools = []string{"Bash"}
	}
	definition := map[string]any{
		"profileVersion": "1.0.0", "harnessVersion": "1.0.0", "modelId": modelID,
		"promptVersion": "isolation", "skillVersions": []any{}, "toolConfiguration": map[string]any{"allowedTools": tools},
		"contextPolicyVersion": "1", "contextFiles": []string{cli.instruction, ".gitconfig"},
		"executionConfiguration": map[string]any{"adapter": cli.name, "args": []string{}},
		"collaborationPolicy":    "solo", "evaluationPolicyVersion": nil,
	}
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	configurationWrite(t, filepath.Join(repo, "evaloom/profile.yaml"), string(data))
	for _, args := range [][]string{
		{"init", "--initial-branch=main", repo}, {"-C", repo, "add", "."},
		{"-C", repo, "commit", "-m", "Pin isolation Profile"}, {"-C", repo, "tag", "profile/v1.0.0"},
	} {
		if _, err := f.git(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	// This definition is in the legacy micro-harness Adapter protocol, not a
	// contract v1 Profile, so pin the tagged commit directly instead of
	// registering it.
	sha, err := f.git(ctx, "-C", repo, "rev-parse", "--verify", "refs/tags/profile/v1.0.0^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	profile := model.HarnessProfile{HarnessID: "isolation-" + cli.name, Version: "1.0.0", TagName: "profile/v1.0.0",
		CommitSHA: sha, Definition: data, DefinitionHash: hex.EncodeToString(hash[:]), ModelID: modelID}
	var def model.ProfileDefinition
	if err := json.Unmarshal(profile.Definition, &def); err != nil {
		t.Fatal(err)
	}
	// A dirty Profile checkout must not become the dedicated configuration.
	configurationWrite(t, filepath.Join(repo, cli.instruction), "uncommitted replacement\n")
	files, err := harnesscontext.Read(ctx, repo, profile.CommitSHA, def.ContextFiles)
	if err != nil {
		t.Fatal(err)
	}
	return profile, files
}

func configurationEnvironment(t *testing.T, ctx context.Context, root string, cli configurationCLI, profile model.HarnessProfile, files []gitcontext.File) *harnessenv.Environment {
	t.Helper()
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	env, err := harnessenv.Create(ctx, harnessenv.Config{Root: root, CommitSHA: profile.CommitSHA, ConfigVariable: cli.variable, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Close(); err != nil {
			t.Error(err)
		}
	})
	return env
}

func configurationRequest(f *Fixture, profile model.HarnessProfile) adapter.HarnessRequest {
	return adapter.HarnessRequest{
		ContractVersion: adapter.ContractVersion, ExecutionID: strings.TrimPrefix(NewCanary(), "evaloom-canary-"),
		Task: adapter.RequestTask{ID: "configuration-isolation", Title: "Record a successful run",
			Body:               "Create isolation.txt containing 'configuration checked', commit that file, and give a short final response following the installed instructions.",
			AcceptanceCriteria: "isolation.txt is committed and the final response follows the installed instructions"},
		Project: adapter.RequestProject{ID: "isolation-fixture", Workdir: f.Worktree, Branch: "evaloom/probe", BaseSHA: f.BaseSHA},
		Profile: adapter.RequestProfile{HarnessID: profile.HarnessID, Version: profile.Version, CommitSHA: profile.CommitSHA, ModelID: profile.ModelID, Definition: profile.Definition},
		Context: adapter.RequestContext{Instructions: "Work only in this disposable repository.", RevisionIDs: []string{}},
		Limits:  adapter.RequestLimits{TimeoutSec: 240},
	}
}

// B must occur in the model's final response, not merely an echoed tool call or
// config dump. A is forbidden in every output stream and in the final response.
func configurationVerdict(result adapter.HarnessResult, logs []adapter.LogLine, a, b, home string) error {
	if a == "" || b == "" || a == b || strings.Contains(a, b) || strings.Contains(b, a) {
		return fmt.Errorf("invalid configuration canaries")
	}
	if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil {
		class := "none"
		if result.ErrorClass != nil {
			class = string(*result.ErrorClass)
		}
		return fmt.Errorf("実行不能: real CLI did not complete successfully (status=%s, errorClass=%s)", result.Status, class)
	}
	if strings.Contains(result.Summary, a) {
		return fmt.Errorf("personal HOME canary A appeared in final response")
	}
	if home == "" || strings.Contains(result.Summary, home) {
		return fmt.Errorf("personal HOME path appeared in final response or path check is disabled")
	}
	for _, line := range logs {
		// Decode JSON strings as well, so escaping in a CLI event cannot hide A.
		if strings.Contains(line.Text, a) || jsonContainsText(line.Text, a) {
			return fmt.Errorf("personal HOME canary A appeared in %s", line.Stream)
		}
		// Also reject personal paths in any CLI-reported settings information,
		// without inventing a complete file inventory that the CLI did not emit.
		if strings.Contains(line.Text, home) || jsonContainsText(line.Text, home) {
			return fmt.Errorf("personal HOME path appeared in %s", line.Stream)
		}
	}
	if !strings.Contains(result.Summary, b) {
		return fmt.Errorf("dedicated Profile canary B missing from final response")
	}
	return nil
}

func jsonContainsText(text, needle string) bool {
	var value any
	if json.Unmarshal([]byte(text), &value) != nil {
		return false
	}
	var contains func(any) bool
	contains = func(value any) bool {
		switch v := value.(type) {
		case string:
			return strings.Contains(v, needle)
		case []any:
			for _, item := range v {
				if contains(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range v {
				if strings.Contains(key, needle) || contains(item) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}

// Credentials are explicit trusted test inputs, separate from the committed
// Profile. Never discover or copy auth from the operator's HOME. This snapshot
// is only passed to harnessenv; it is never committed or sent to a context API.
func configurationCredential(cli configurationCLI, commit, data string) (gitcontext.File, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &object) != nil || len(object) == 0 {
		return gitcontext.File{}, fmt.Errorf("実行不能: %sAUTH_JSON must contain a dedicated credential JSON object", cli.prefix)
	}
	hash := sha256.Sum256([]byte(data))
	return gitcontext.File{Path: cli.credential, Content: data, ContentHash: hex.EncodeToString(hash[:]), SourceCommitSHA: commit}, nil
}

type configurationAccess struct {
	Path   string `json:"path"`
	Denied *bool  `json:"denied"`
}

func configurationAccessVerdict(data []byte, paths []string) error {
	var report struct {
		DedicatedReadable bool                  `json:"dedicatedReadable"`
		HomeIsolated      bool                  `json:"homeIsolated"`
		Accesses          []configurationAccess `json:"accesses"`
	}
	if json.Unmarshal(data, &report) != nil || !report.DedicatedReadable || !report.HomeIsolated || len(report.Accesses) != len(paths) {
		return fmt.Errorf("実行不能: incomplete HOME access evidence or failed positive control")
	}
	for i, access := range report.Accesses {
		if access.Path != paths[i] || access.Denied == nil || !*access.Denied {
			return fmt.Errorf("personal HOME path accessible or missing evidence: %s", paths[i])
		}
	}
	return nil
}

// Run by the production Container with the CLI's exact SandboxSpec. A missing
// interpreter, unexpected OS error or unreadable dedicated file fails the run.
const configurationAccessScript = `import errno, json, os, sys
home, dedicated, *paths = sys.argv[1:]
with open(dedicated, 'rb') as stream:
    readable = bool(stream.read())
accesses = []
for path in paths:
    try:
        if path == home:
            os.listdir(path)
        else:
            with open(path, 'rb') as stream:
                stream.read(1)
        denied = False
    except OSError as error:
        if error.errno not in (errno.ENOENT, errno.EACCES, errno.EPERM, errno.ENOTDIR):
            raise
        denied = True
    accesses.append({'path': path, 'denied': denied})
print(json.dumps({'dedicatedReadable': readable,
    'homeIsolated': os.environ.get('HOME') not in (None, '', home), 'accesses': accesses}))
`
