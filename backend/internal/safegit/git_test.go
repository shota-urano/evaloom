package safegit_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

type fixture struct {
	t                      *testing.T
	root, repo, wt, remote string
	env                    []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, repo: filepath.Join(root, "repo"), wt: filepath.Join(root, "worktree"), remote: filepath.Join(root, "remote.git")}
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(name, "GIT_") {
			f.env = append(f.env, variable)
		}
	}
	f.env = append(f.env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	f.raw(f.root, "init", "-q", "--initial-branch=main", f.repo)
	f.raw(f.repo, "config", "user.name", "Evaloom Test")
	f.raw(f.repo, "config", "user.email", "test@example.invalid")
	write(t, filepath.Join(f.repo, "file.txt"), "initial\n", 0600)
	write(t, filepath.Join(f.repo, ".gitattributes"), "*.txt filter=Mixed.Driver diff=Mixed.Driver merge=Mixed.Driver\n", 0600)
	f.raw(f.repo, "add", ".")
	f.raw(f.repo, "commit", "-qm", "initial")
	f.raw(f.root, "init", "-q", "--bare", f.remote)
	f.raw(f.repo, "remote", "add", "origin", f.remote)
	f.raw(f.repo, "push", "-q", "origin", "main")
	f.raw(f.repo, "worktree", "add", "-qb", "evaloom/test", f.wt)
	return f
}

func (f *fixture) raw(repo string, args ...string) string {
	f.t.Helper()
	out, err := f.rawResult(repo, args...)
	if err != nil {
		f.t.Fatalf("raw git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (f *fixture) rawResult(repo string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = f.env
	return cmd.CombinedOutput()
}

func (f *fixture) safe(repo string, args ...string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := safegit.Output(ctx, repo, args...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			f.t.Fatalf("safe git %v: %v\n%s", args, err, exit.Stderr)
		}
		f.t.Fatal(err)
	}
	return string(out)
}

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) payload(name, body string) (script, marker string) {
	f.t.Helper()
	script, marker = filepath.Join(f.root, name+".sh"), filepath.Join(f.root, name+".ran")
	write(f.t, script, "#!/bin/sh\nprintf ran >> '"+strings.ReplaceAll(marker, "'", "'\\''")+"'\n"+body+"\n", 0700)
	return script, marker
}

func absent(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untrusted executable ran (%s), stat error %v", marker, err)
	}
}

func present(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("positive control did not execute %s: %v", marker, err)
	}
}

func TestHooksDisabledAcrossHostOperations(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint("customHooksPath=", custom), func(t *testing.T) {
			f := newFixture(t)
			hooks := filepath.Join(f.repo, ".git", "hooks")
			if custom {
				hooks = filepath.Join(f.root, "custom-hooks")
				if err := os.Mkdir(hooks, 0700); err != nil {
					t.Fatal(err)
				}
				f.raw(f.wt, "config", "core.hooksPath", hooks)
			}
			var markers []string
			for _, name := range []string{"post-checkout", "pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "pre-push", "reference-transaction", "post-index-change"} {
				script, marker := f.payload(name, "exit 0")
				data, err := os.ReadFile(script)
				if err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(hooks, name), string(data), 0700)
				markers = append(markers, marker)
			}
			f.safe(f.wt, "checkout", "--detach", "HEAD")
			write(t, filepath.Join(f.wt, "file.txt"), "changed\n", 0600)
			f.safe(f.wt, "add", "file.txt")
			f.safe(f.wt, "commit", "-qm", "safe commit")
			f.safe(f.wt, "fetch", "origin", "main")
			f.safe(f.wt, "push", "origin", "HEAD:refs/heads/evaloom/safe")
			newWT := filepath.Join(f.root, "new-worktree")
			f.safe(f.repo, "worktree", "add", "--detach", newWT, "HEAD")
			for _, marker := range markers {
				absent(t, marker)
			}
			// The same configuration really executes hooks without the wrapper.
			f.raw(f.wt, "checkout", "--detach", "HEAD")
			write(t, filepath.Join(f.wt, "file.txt"), "raw change\n", 0600)
			f.raw(f.wt, "add", "file.txt")
			f.raw(f.wt, "commit", "-qm", "raw commit")
			f.raw(f.wt, "push", "origin", "HEAD:refs/heads/evaloom/raw")
			for _, marker := range markers {
				present(t, marker)
			}
		})
	}
}

func TestFSMonitorDisabledIncludingExistingCallers(t *testing.T) {
	f := newFixture(t)
	script, marker := f.payload("fsmonitor", "printf 'token\\000'\nexit 0")
	f.raw(f.wt, "config", "core.fsmonitor", script)
	f.safe(f.wt, "status", "--porcelain")
	sha, err := gitcontext.HeadCommitSHA(context.Background(), f.wt)
	if err != nil {
		t.Fatal(err)
	}
	files, err := gitcontext.Read(context.Background(), f.wt, sha, func(path string) bool { return path == "file.txt" })
	if err != nil || len(files) != 1 || files[0].Content != "initial\n" {
		t.Fatalf("Read = %v, %v", files, err)
	}
	if _, err := registry.ValidateRepository(context.Background(), registry.RepositoryInput{
		Path: f.wt, URL: f.remote, BaseBranch: "main", WorktreeRoot: filepath.Join(f.root, "managed"),
	}); err != nil {
		t.Fatal(err)
	}
	absent(t, marker)
	f.raw(f.wt, "status", "--porcelain")
	present(t, marker)
}

func TestNamedFiltersDisabledThroughIncludesAndWorktreeConfig(t *testing.T) {
	for _, kind := range []string{"clean", "smudge", "process"} {
		for _, source := range []string{"include", "worktree"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				f := newFixture(t)
				body := "cat"
				if kind == "process" {
					body = "exit 1" // Deliberately not a filter protocol server.
				}
				script, marker := f.payload("filter", body)
				config := filepath.Join(f.root, "included.config")
				if source == "include" {
					f.raw(f.wt, "config", "include.path", config)
				} else {
					f.raw(f.wt, "config", "extensions.worktreeConfig", "true")
					config = strings.TrimSpace(f.raw(f.wt, "rev-parse", "--git-path", "config.worktree"))
				}
				f.raw(f.root, "config", "--file", config, "filter.Mixed.Driver."+kind, script)
				f.raw(f.root, "config", "--file", config, "filter.Mixed.Driver.required", "true")
				if kind == "clean" {
					write(t, filepath.Join(f.wt, "file.txt"), "changed\n", 0600)
					f.safe(f.wt, "add", "file.txt")
				} else {
					if err := os.Remove(filepath.Join(f.wt, "file.txt")); err != nil {
						t.Fatal(err)
					}
					f.safe(f.wt, "checkout", "--", "file.txt")
				}
				absent(t, marker)
				if kind == "clean" {
					write(t, filepath.Join(f.wt, "file.txt"), "another change\n", 0600)
					f.raw(f.wt, "add", "file.txt")
				} else {
					if err := os.Remove(filepath.Join(f.wt, "file.txt")); err != nil {
						t.Fatal(err)
					}
					// A failing process filter is still a successful attack: it
					// executed before Git rejected its output.
					_, _ = f.rawResult(f.wt, "checkout", "--", "file.txt")
				}
				present(t, marker)
			})
		}
	}
}

func TestConditionalIncludeInNewWorktree(t *testing.T) {
	for _, condition := range []string{"gitdir", "onbranch"} {
		for _, safe := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/safe=%v", condition, safe), func(t *testing.T) {
				f := newFixture(t)
				script, marker := f.payload("conditional-filter", "cat")
				config := filepath.Join(f.root, "conditional.config")
				f.raw(f.root, "config", "--file", config, "filter.Mixed.Driver.smudge", script)
				match := "onbranch:evaloom/conditional"
				if condition == "gitdir" {
					match = "gitdir:**/worktrees/conditional"
				}
				f.raw(f.repo, "config", "includeIf."+match+".path", config)
				args := []string{"worktree", "add", "-b", "evaloom/conditional", filepath.Join(f.root, "conditional"), "HEAD"}
				if safe {
					f.safe(f.repo, args...)
					absent(t, marker)
				} else {
					f.raw(f.repo, args...)
					present(t, marker)
				}
			})
		}
	}
}

func TestRelativeIncludesPreserveSymlinkSemantics(t *testing.T) {
	for _, throughParent := range []bool{true, false} {
		t.Run(fmt.Sprint("symlinkParent=", throughParent), func(t *testing.T) {
			f := newFixture(t)
			script, marker := f.payload("relative-filter", "cat")
			external := filepath.Join(f.root, "external")
			if err := os.MkdirAll(filepath.Join(external, "deep"), 0700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(external, "payload.config")
			include := "../link/../payload.config"
			if throughParent {
				if err := os.Symlink(filepath.Join(external, "deep"), filepath.Join(f.repo, "link")); err != nil {
					t.Fatal(err)
				}
			} else {
				// Includes in a symlinked file are relative to the symlink's
				// directory, rather than the physical target's directory.
				outer := filepath.Join(external, "outer.config")
				f.raw(f.root, "config", "--file", outer, "include.path", "payload.config")
				if err := os.Symlink(outer, filepath.Join(f.repo, "outer.config")); err != nil {
					t.Fatal(err)
				}
				include, config = "../outer.config", filepath.Join(f.repo, "payload.config")
			}
			f.raw(f.root, "config", "--file", config, "filter.Mixed.Driver.clean", script)
			f.raw(f.repo, "config", "include.path", include)
			write(t, filepath.Join(f.wt, "file.txt"), "changed\n", 0600)
			f.safe(f.wt, "add", "file.txt")
			absent(t, marker)
			write(t, filepath.Join(f.wt, "file.txt"), "another change\n", 0600)
			f.raw(f.wt, "add", "file.txt")
			present(t, marker)
		})
	}
}

func TestDiffDriversDisabled(t *testing.T) {
	for _, key := range []string{"diff.external", "diff.Mixed.Driver.command", "diff.Mixed.Driver.textconv"} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			script, marker := f.payload("diff", "exit 0")
			f.raw(f.wt, "config", key, script)
			write(t, filepath.Join(f.wt, "file.txt"), "changed\n", 0600)
			out := f.safe(f.wt, "diff")
			if !strings.Contains(out, "+changed") {
				t.Fatalf("safe diff lost content: %s", out)
			}
			absent(t, marker)
			f.raw(f.wt, "diff")
			present(t, marker)
		})
	}
}

func TestFilterDriverNameWithEquals(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.wt, ".gitattributes"), "*.txt filter=Mixed=Driver\n", 0600)
	script, marker := f.payload("equals-filter", "cat")
	f.raw(f.wt, "config", "filter.Mixed=Driver.clean", script)
	write(t, filepath.Join(f.wt, "file.txt"), "changed\n", 0600)
	f.safe(f.wt, "add", "file.txt")
	absent(t, marker)
	write(t, filepath.Join(f.wt, "file.txt"), "another change\n", 0600)
	f.raw(f.wt, "add", "file.txt")
	present(t, marker)
}

func TestMergeDriverDisabledDuringCheckout(t *testing.T) {
	for _, safe := range []bool{true, false} {
		t.Run(fmt.Sprint("safe=", safe), func(t *testing.T) {
			f := newFixture(t)
			write(t, filepath.Join(f.repo, "file.txt"), "upstream change\n", 0600)
			f.raw(f.repo, "add", "file.txt")
			f.raw(f.repo, "commit", "-qm", "upstream")
			write(t, filepath.Join(f.wt, "file.txt"), "local change\n", 0600)
			script, marker := f.payload("merge", "exit 1")
			f.raw(f.wt, "config", "merge.Mixed.Driver.driver", script)
			args := []string{"checkout", "--merge", "--detach", "main"}
			if safe {
				_, _ = safegit.Output(context.Background(), f.wt, args...)
				absent(t, marker)
			} else {
				_, _ = f.rawResult(f.wt, args...)
				present(t, marker)
			}
			if out := f.raw(f.wt, "ls-files", "--unmerged"); !strings.Contains(out, "file.txt") {
				t.Fatalf("checkout did not attempt the conflicting merge: %s", out)
			}
		})
	}
}

func TestSSHAndGitProxyCommandsDisabled(t *testing.T) {
	// A real local TCP endpoint closes each connection during negotiation, so
	// the safe default transport fails promptly without contacting another host.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	for _, key := range []string{"core.sshCommand", "core.gitProxy"} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			script, marker := f.payload("connection", "exit 1")
			f.raw(f.wt, "config", key, script)
			scheme := "ssh"
			if key == "core.gitProxy" {
				scheme = "git"
			}
			url := scheme + "://" + listener.Addr().String() + "/repo.git"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := safegit.Output(ctx, f.wt, "ls-remote", url); err == nil {
				t.Fatal("closed endpoint unexpectedly succeeded")
			}
			absent(t, marker)
			_, _ = f.rawResult(f.wt, "ls-remote", url)
			present(t, marker)
		})
	}
}

func TestExternalVCSTransportFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.raw(f.wt, "config", "remote.origin.vcs", "ext")
	if _, err := safegit.Output(context.Background(), f.wt, "ls-remote", "origin"); err == nil || !strings.Contains(err.Error(), "external VCS transport is disabled") {
		t.Fatalf("external VCS selector was not rejected: %v", err)
	}
}

func TestCredentialHelpersAndAskPassDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	for _, key := range []string{"credential.helper", "credential." + server.URL + ".helper", "core.askPass"} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			script, marker := f.payload("credential", "exit 1")
			f.raw(f.wt, "config", "--add", key, script)
			f.raw(f.wt, "config", "--add", key, script)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := safegit.Output(ctx, f.wt, "ls-remote", server.URL+"/repo.git"); err == nil {
				t.Fatal("unauthenticated remote unexpectedly succeeded")
			}
			absent(t, marker)
			_, _ = f.rawResult(f.wt, "ls-remote", server.URL+"/repo.git")
			present(t, marker)
		})
	}
}

func TestRemoteCommandsAndExtProtocolDisabled(t *testing.T) {
	for _, key := range []string{"remote.origin.uploadpack", "remote.origin.receivepack", "protocol.ext.allow"} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			script, marker := f.payload("transport", "exit 1")
			args := []string{"fetch", "origin", "main"}
			if key == "protocol.ext.allow" {
				f.raw(f.wt, "config", key, "always")
				args = []string{"ls-remote", "ext::" + script}
			} else {
				f.raw(f.wt, "config", key, script)
				if key == "remote.origin.receivepack" {
					args = []string{"push", "origin", "HEAD:refs/heads/evaloom/test"}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := safegit.Output(ctx, f.wt, args...)
			if key != "protocol.ext.allow" && err != nil {
				t.Fatalf("safe transport failed: %v", err)
			}
			if key == "protocol.ext.allow" && err == nil {
				t.Fatal("ext protocol accepted")
			}
			absent(t, marker)
			_, _ = f.rawResult(f.wt, args...)
			present(t, marker)
		})
	}
}

func TestSigningAndEditorDisabled(t *testing.T) {
	for _, key := range []string{"gpg.program", "gpg.openpgp.program", "core.editor"} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			script, marker := f.payload("program", "exit 1")
			f.raw(f.wt, "config", key, script)
			args := []string{"commit", "--allow-empty", "-qm", "safe"}
			if key != "core.editor" {
				f.raw(f.wt, "config", "commit.gpgSign", "true")
			}
			f.safe(f.wt, args...)
			if key == "core.editor" {
				// Without a supplied message, fail rather than launch an editor.
				_, _ = safegit.Output(context.Background(), f.wt, "commit", "--allow-empty")
				args = []string{"commit", "--allow-empty"}
			}
			absent(t, marker)
			_, _ = f.rawResult(f.wt, args...)
			present(t, marker)
		})
	}
}

func TestAmbientOverridesAndUnknownCommandsRejected(t *testing.T) {
	f := newFixture(t)
	script, marker := f.payload("ambient", "exit 1")
	f.raw(f.wt, "config", "alias.attack", "!"+script)
	global := filepath.Join(f.root, "global.config")
	f.raw(f.root, "config", "--file", global, "core.fsmonitor", script)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", script)
	t.Setenv("GIT_DIR", f.remote)
	t.Setenv("GIT_WORK_TREE", f.repo)
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_SSH_COMMAND", script)
	t.Setenv("GIT_ASKPASS", script)
	t.Setenv("SSH_ASKPASS", script)
	t.Setenv("GIT_EDITOR", script)
	f.safe(f.wt, "status", "--porcelain")
	if got := strings.TrimSpace(f.safe(f.wt, "rev-parse", "--show-toplevel")); got != f.wt {
		// macOS may return the /private spelling of a temporary directory.
		real, err := filepath.EvalSymlinks(f.wt)
		if err != nil || real != got {
			t.Fatalf("ambient repository override survived: %s", got)
		}
	}
	for _, args := range [][]string{nil, {"attack"}, {"-c", "core.fsmonitor=" + script, "status"}, {"external-helper"}} {
		if _, err := safegit.Output(context.Background(), f.wt, args...); err == nil {
			t.Fatalf("unsafe command accepted: %v", args)
		}
	}
	absent(t, marker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := safegit.Output(ctx, f.wt, "status"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled operation: %v", err)
	}
}
