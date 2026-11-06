package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/runner"
)

func environmentRunner(t *testing.T, f *authFixture, token string) *runner.Runner {
	t.Helper()
	root := t.TempDir()
	tokenPath := filepath.Join(root, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		RunnerID: "mac-mini", ControlPlaneURL: f.server.URL,
		TokenFile: tokenPath, WorktreeRoot: root, DataDir: root,
		HarnessAuthFiles:        map[string]string{},
		GitHubAppPrivateKeyFile: filepath.Join(home, ".config", "evaloom", "publisher.pem"),
		Container:               runner.ContainerConfig{RuntimePath: "/usr/local/bin/docker", Image: "harness:test", ProxyImage: "proxy:test", ProxyBinary: "/opt/evaloom/proxy"},
		CommitAuthor:            runner.CommitAuthorConfig{Name: "evaloom", Email: "bot@example.test"},
		DefaultTimeouts:         runner.DefaultTimeouts{SetupSec: 900, HarnessSec: 3600, TestSec: 1800},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHarnessEnvironmentPersistsEveryPlacedFile(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Run(variable, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			paths := []string{"config.toml", "AGENTS.md", "prompts/developer.md", "skills/check/SKILL.md", "skills/check/data.bin"}
			repo, _ := contextRepo(t, "config.toml", "model = 'fixture'\n")
			for _, name := range paths[1:] {
				writeContextFile(t, repo, name, "pinned "+name+"\r\n日本語\x00\xff")
			}
			projectGit(t, repo, "add", ".")
			projectGit(t, repo, "commit", "--quiet", "-m", "profile")
			sha, err := gitcontext.HeadCommitSHA(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			// Read the immutable commit even after changing the harness worktree.
			for _, name := range paths {
				writeContextFile(t, repo, name, "uncommitted change")
			}
			files, err := harnesscontext.Read(context.Background(), repo, sha, paths)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET profile_commit_sha = ? WHERE id = ?`, sha, otherExecution); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE harness_profiles SET commit_sha = ? WHERE id = 'profile-a'`, sha); err != nil {
				t.Fatal(err)
			}
			setContextDefinition(t, f, "codex", paths)
			r := environmentRunner(t, f, token)
			cfg := harnessenv.Config{Root: t.TempDir(), CommitSHA: sha, ConfigVariable: variable, Files: files}
			env, reply, err := r.PrepareHarnessEnvironment(context.Background(), otherExecution, input.HeadSHA, input.Project, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = env.Close() })
			manifest, err := env.Files(context.Background())
			if err != nil || !reflect.DeepEqual(manifest, files) {
				t.Fatalf("placed manifest differs: %v %v", manifest, err)
			}
			rows := pinnedRows(t, f)
			if len(rows) != len(files)+2 || len(reply.RevisionIDs) != len(rows) {
				t.Fatalf("unrecorded files: %v", rows)
			}
			for i, file := range manifest {
				row := rows[i]
				if row.Layer != "harness" || row.Kind != "harness_config" || row.SourcePath != file.Path ||
					row.ContentHash != contextHash(file.Content) || row.ContentHash != file.ContentHash || row.Content != file.Content ||
					row.SourceCommitSHA != sha || row.ExecutionID != otherExecution {
					t.Fatalf("record differs from disk manifest: %+v %+v", row, file)
				}
			}
			// The writable configuration inherits exactly the manifest that the
			// real Runner/context API persisted. CLI writes never become pins.
			copy, err := env.WorkingCopy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = copy.Close() })
			store := filepath.Join(t.TempDir(), "credential")
			authContent := `{"token":"private-context-token"}`
			if err := os.WriteFile(store, []byte(authContent), 0600); err != nil {
				t.Fatal(err)
			}
			lease, err := harnessenv.AcquireAuthentication(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := lease.Install(copy); err != nil {
				t.Fatal(err)
			}
			for _, row := range pinnedRows(t, f) {
				if row.SourcePath == "auth.json" || row.SourcePath == ".credentials.json" || row.Content == authContent || row.ContentHash == contextHash(authContent) {
					t.Fatal("authentication persisted in context_revisions")
				}
			}
			copied, err := copy.Files(context.Background())
			if err != nil || !reflect.DeepEqual(copied, manifest) {
				t.Fatalf("copy differs from persisted configuration: %v %v", copied, err)
			}
			if err := os.WriteFile(filepath.Join(copy.ConfigDir(), "cli-state"), []byte("runtime only"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := copy.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(copy.ConfigDir()); !os.IsNotExist(err) {
				t.Fatal("copy survived cleanup")
			}
			if !reflect.DeepEqual(pinnedRows(t, f), rows) {
				t.Fatal("CLI state changed context_revisions")
			}
			// A repeated preparation uses a fresh directory but preserves revision IDs.
			again, retry, err := r.PrepareHarnessEnvironment(context.Background(), otherExecution, input.HeadSHA, input.Project, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = again.Close() })
			if env.ConfigDir() == again.ConfigDir() || !reflect.DeepEqual(reply, retry) {
				t.Fatalf("retry changed pins or shared a directory: %+v %+v", reply, retry)
			}
			assertContextCounts(t, f, len(files)+2, 1)
		})
	}
}

func TestHarnessEnvironmentFailureDoesNotExposePartialConfiguration(t *testing.T) {
	for _, mode := range []string{"invalid-pin", "rejected-save", "db-rollback", "bad-response", "empty-revisions", "transport", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			r := environmentRunner(t, f, token)
			cfg := harnessenv.Config{Root: t.TempDir(), CommitSHA: strings.Repeat("b", 40), ConfigVariable: "CODEX_HOME", Files: input.Harness}
			ctx := context.Background()
			switch mode {
			case "invalid-pin":
				cfg.Files[0].SourceCommitSHA = strings.Repeat("c", 40)
			case "rejected-save":
				cfg.Files[0].Path = "unselected.toml"
			case "db-rollback":
				if _, err := f.db.Exec(`CREATE TRIGGER reject_task_revision BEFORE INSERT ON context_revisions WHEN NEW.layer = 'task' BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			case "bad-response":
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"revisionIds":null}`)) })
			case "empty-revisions":
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"revisionIds":[]}`)) })
			case "transport":
				f.server.Close()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			env, _, err := r.PrepareHarnessEnvironment(ctx, otherExecution, input.HeadSHA, input.Project, cfg)
			if err == nil || env != nil {
				t.Fatalf("failure exposed environment: %v %v", env, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation:", err)
			}
			entries, readErr := os.ReadDir(cfg.Root)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("failed preparation left files: %v %v", entries, readErr)
			}
			assertContextCounts(t, f, 0, 0)
		})
	}
}
