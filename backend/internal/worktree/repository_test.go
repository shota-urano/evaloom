package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

func git(t *testing.T, repository string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := safegit.Output(ctx, repository, args...)
	if err != nil {
		t.Fatalf("Git %v: %v", args, err)
	}
	return strings.TrimSuffix(string(out), "\n")
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

type remoteFixture struct {
	seed, remote, owner, url string
}

func newRemote(t *testing.T) remoteFixture {
	t.Helper()
	root := t.TempDir()
	f := remoteFixture{seed: filepath.Join(root, "seed"), remote: filepath.Join(root, "remote.git"), owner: filepath.Join(root, "owner")}
	git(t, root, "init", "--initial-branch=main", f.seed)
	config := filepath.Join(f.seed, ".git", "config")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	write(t, config, string(data)+"\n[user]\nname = Evaloom Test\nemail = evaloom@example.invalid\n")
	write(t, filepath.Join(f.seed, "file.txt"), "initial\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "initial")
	git(t, root, "init", "--bare", "--initial-branch=main", f.remote)
	git(t, f.remote, "fetch", "--", f.seed, "HEAD:refs/heads/main")
	git(t, root, "clone", "--no-local", "--", f.remote, f.owner)
	write(t, filepath.Join(f.owner, "file.txt"), "owner's staged edit\n")
	git(t, f.owner, "add", "file.txt")
	write(t, filepath.Join(f.owner, ".env"), "OWNER_SECRET=must-stay-local\n")

	// Serve a real bare repository using Git's dumb HTTP protocol. Generate
	// the ref advertisement using safegit instead of spawning http-backend or
	// update-server-info outside the safety wrapper. All objects are loose.
	files := http.FileServer(http.Dir(f.remote))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info/refs":
			out, err := safegit.Output(r.Context(), f.remote, "for-each-ref", "--format=%(objectname)%09%(refname)")
			if err != nil {
				http.Error(w, "advertise refs", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(out)
		case "/objects/info/packs":
			_, _ = w.Write([]byte("\n"))
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

type recordingTransport struct {
	mu       sync.Mutex
	commands []string
	run      func(context.Context, string, ...string) ([]byte, error)
}

func (r *recordingTransport) Output(ctx context.Context, directory string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.commands = append(r.commands, args[0])
	r.mu.Unlock()
	if r.run != nil {
		return r.run(ctx, directory, args...)
	}
	return safegit.Output(ctx, directory, args...)
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			name, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files[name] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPrepareClonesThenFetchesWithoutChangingOwner(t *testing.T) {
	f := newRemote(t)
	before := snapshot(t, f.owner)
	root := filepath.Join(t.TempDir(), "worktrees")
	transport := &recordingTransport{}
	repositories := Repositories{WorktreeRoot: root, Transport: transport}
	ctx := context.Background()
	first, err := repositories.Prepare(ctx, "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	if first != filepath.Join(realRoot, repositoryDirectory, "project-a") {
		t.Fatalf("unexpected clone path: %s", first)
	}
	initial := git(t, f.seed, "rev-parse", "HEAD")
	if got := git(t, first, "rev-parse", "refs/remotes/origin/main"); got != initial {
		t.Fatalf("clone has %s, want %s", got, initial)
	}
	if _, err := os.Stat(filepath.Join(first, "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone unexpectedly checked out project files: %v", err)
	}
	identity, err := os.Stat(filepath.Join(first, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	// Preserve a local branch through fetch; preparation never resets heads.
	git(t, first, "checkout", "--no-track", "-b", "evaloom/retained", initial)
	write(t, filepath.Join(f.seed, "file.txt"), "remote update\n")
	git(t, f.seed, "add", "file.txt")
	git(t, f.seed, "commit", "-m", "remote update")
	updated := git(t, f.seed, "rev-parse", "HEAD")
	git(t, f.remote, "fetch", "--", f.seed, "HEAD:refs/heads/main")
	// A new preparer instance models reuse after a Runner restart.
	second, err := (Repositories{WorktreeRoot: root, Transport: transport}).Prepare(ctx, "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	afterIdentity, _ := os.Stat(filepath.Join(second, ".git"))
	if second != first || !os.SameFile(identity, afterIdentity) {
		t.Fatal("second preparation replaced the clone")
	}
	if got := git(t, second, "rev-parse", "refs/remotes/origin/main"); got != updated || updated == initial {
		t.Fatalf("fetch did not import new commit: got %s, want %s", got, updated)
	}
	if got := git(t, second, "rev-parse", "HEAD"); got != initial {
		t.Fatalf("fetch moved local HEAD to %s", got)
	}
	other, err := repositories.Prepare(ctx, "project-b", f.url)
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity, _ := os.Stat(filepath.Join(other, ".git"))
	if other == first || os.SameFile(identity, otherIdentity) {
		t.Fatal("Projects share a Git directory")
	}
	if got := transport.commands; !reflect.DeepEqual(got, []string{"clone", "fetch", "clone"}) {
		t.Fatalf("network commands = %v", got)
	}
	if after := snapshot(t, f.owner); !reflect.DeepEqual(before, after) {
		t.Fatal("owner's working tree, index, refs, config or objects changed")
	}
	ownerFiles := snapshot(t, filepath.Join(f.owner, ".git", "objects"))
	cloneFiles := snapshot(t, filepath.Join(first, ".git", "objects"))
	for name := range cloneFiles {
		if _, ok := ownerFiles[name]; ok {
			ownerInfo, _ := os.Stat(filepath.Join(f.owner, ".git", "objects", name))
			cloneInfo, _ := os.Stat(filepath.Join(first, ".git", "objects", name))
			if os.SameFile(ownerInfo, cloneInfo) {
				t.Fatal("clone shares hardlinked objects with owner")
			}
		}
	}
}

func TestPrepareRejectsSymlinksAndInvalidExistingDirectories(t *testing.T) {
	for _, part := range []string{"storage", "project", "git", "objects", "refs", "not-repository", "alternates", "nested-ref", "git-file"} {
		t.Run(part, func(t *testing.T) {
			f := newRemote(t)
			before := snapshot(t, f.owner)
			root := t.TempDir()
			storage := filepath.Join(root, repositoryDirectory)
			project := filepath.Join(storage, "project-a")
			switch part {
			case "storage":
				if err := os.Symlink(f.owner, storage); err != nil {
					t.Fatal(err)
				}
			case "project":
				if err := os.Mkdir(storage, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.owner, project); err != nil {
					t.Fatal(err)
				}
			case "not-repository":
				if err := os.MkdirAll(project, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.MkdirAll(project, 0700); err != nil {
					t.Fatal(err)
				}
				if part == "git-file" {
					write(t, filepath.Join(project, ".git"), "gitdir: "+filepath.Join(f.owner, ".git")+"\n")
				} else if part == "git" {
					if err := os.Symlink(filepath.Join(f.owner, ".git"), filepath.Join(project, ".git")); err != nil {
						t.Fatal(err)
					}
				} else {
					git(t, root, "init", project)
					git(t, project, "remote", "add", "origin", f.url)
					if part == "nested-ref" {
						if err := os.Mkdir(filepath.Join(project, ".git", "refs", "remotes"), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(filepath.Join(f.owner, ".git", "refs", "heads"), filepath.Join(project, ".git", "refs", "remotes", "origin")); err != nil {
							t.Fatal(err)
						}
					} else if part == "alternates" {
						write(t, filepath.Join(project, ".git", "objects", "info", "alternates"), filepath.Join(f.owner, ".git", "objects")+"\n")
					} else {
						// Rename only a generated empty fixture directory.
						path := filepath.Join(project, ".git", part)
						if err := os.Rename(path, path+".original"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(filepath.Join(f.owner, ".git", part), path); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			transport := &recordingTransport{}
			path, err := (Repositories{WorktreeRoot: root, Transport: transport}).Prepare(context.Background(), "project-a", f.url)
			if err == nil || path != "" || len(transport.commands) != 0 {
				t.Fatalf("unsafe path accepted: %q, %v, %v", path, err, transport.commands)
			}
			if !reflect.DeepEqual(before, snapshot(t, f.owner)) {
				t.Fatal("owner repository changed")
			}
		})
	}
}

func TestPrepareRejectsChangedOrigin(t *testing.T) {
	f := newRemote(t)
	r := Repositories{WorktreeRoot: t.TempDir()}
	path, err := r.Prepare(context.Background(), "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	git(t, path, "remote", "set-url", "origin", f.owner)
	transport := &recordingTransport{}
	r.Transport = transport
	if path, err := r.Prepare(context.Background(), "project-a", f.url); err == nil || path != "" || len(transport.commands) != 0 {
		t.Fatalf("changed origin accepted: %q, %v", path, err)
	}
}

func TestPrepareRejectsFetchDestinationOverrides(t *testing.T) {
	for _, source := range []string{"config", "include", "conditional-include", "worktree-config", "url-rewrite", "legacy-remotes", "legacy-branches", "bundle-uri"} {
		t.Run(source, func(t *testing.T) {
			f := newRemote(t)
			transport := &recordingTransport{}
			r := Repositories{WorktreeRoot: t.TempDir(), Transport: transport}
			ctx := context.Background()
			repository, err := r.Prepare(ctx, "project-a", f.url)
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(repository, ".git", "config")
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			// Origin still resolves to the registered URL. A URL argument to
			// fetch can nevertheless select a remote of that exact name.
			payload := fmt.Sprintf("\n[ReMoTe %q]\nUrL = %s\n", f.url, f.owner)
			switch source {
			case "config":
				write(t, config, string(data)+payload)
			case "include", "conditional-include":
				included := filepath.Join(repository, ".git", "included.config")
				write(t, included, payload)
				section := "include"
				if source == "conditional-include" {
					section = `includeIf "gitdir:/never-matches/"`
				}
				write(t, config, string(data)+"\n["+section+"]\npath = included.config\n")
			case "worktree-config":
				write(t, config, string(data)+"\n[extensions]\nworktreeConfig = true\n")
				write(t, filepath.Join(repository, ".git", "config.worktree"), payload)
			case "url-rewrite":
				// get-url only rewrites once: hide the malicious rewrite behind
				// a second rule that makes origin resolve to the registered URL.
				git(t, repository, "remote", "set-url", "origin", "https://alias.invalid/repo")
				data, err = os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				write(t, config, string(data)+fmt.Sprintf("\n[url %q]\ninsteadOf = https://alias.invalid/repo\n[url %q]\ninsteadOf = %s\n", f.url, f.owner, f.url))
			case "legacy-remotes", "legacy-branches":
				kind := strings.TrimPrefix(source, "legacy-")
				name := filepath.Join(repository, ".git", kind, f.url)
				if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
					t.Fatal(err)
				}
				content := f.owner + "#main\n"
				if kind == "remotes" {
					content = "URL: " + f.owner + "\nPull: refs/heads/main:refs/remotes/origin/main\n"
				}
				write(t, name, content)
			case "bundle-uri":
				write(t, config, string(data)+"\n[fetch]\nbundleURI = "+f.owner+"\n")
			}
			if got := git(t, repository, "remote", "get-url", "origin"); got != f.url {
				t.Fatalf("test setup changed origin resolution: %q", got)
			}
			ownerConfig := filepath.Join(f.owner, ".git", "config")
			ownerData, err := os.ReadFile(ownerConfig)
			if err != nil {
				t.Fatal(err)
			}
			write(t, ownerConfig, string(ownerData)+"\n[user]\nname = Owner\nemail = owner@example.invalid\n")
			git(t, f.owner, "commit", "-m", "owner-only commit")
			ownerSHA := git(t, f.owner, "rev-parse", "HEAD")
			ownerBefore := snapshot(t, f.owner)
			cloneBefore := snapshot(t, repository)
			path, err := r.Prepare(ctx, "project-a", f.url)
			if err == nil || path != "" || !reflect.DeepEqual(transport.commands, []string{"clone"}) {
				t.Errorf("destination override accepted: path=%q, err=%v, commands=%v", path, err, transport.commands)
			}
			if !reflect.DeepEqual(ownerBefore, snapshot(t, f.owner)) {
				t.Error("owner repository changed")
			}
			if !reflect.DeepEqual(cloneBefore, snapshot(t, repository)) {
				t.Error("rejected clone changed")
			}
			if _, err := safegit.Output(ctx, repository, "cat-file", "-e", ownerSHA); err == nil {
				t.Error("fetch imported an object from the owner's local repository")
			}
		})
	}
}

func TestPrepareResolvesWorktreeRootSymlink(t *testing.T) {
	f := newRemote(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path, err := (Repositories{WorktreeRoot: link}).Prepare(context.Background(), "project-a", f.url)
	real, _ = filepath.EvalSymlinks(real)
	if err != nil || path != filepath.Join(real, repositoryDirectory, "project-a") {
		t.Fatalf("resolved path = %q, %v", path, err)
	}
}

func TestPrepareFailureCleanupAndCancellation(t *testing.T) {
	f := newRemote(t)
	root := t.TempDir()
	r := Repositories{WorktreeRoot: root, Transport: &recordingTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("transport secret must not leak")
	}}}
	path, err := r.Prepare(context.Background(), "project-a", f.url)
	if path != "" || err == nil || err.Error() != "worktree: clone failed" {
		t.Fatalf("unexpected failure: %q, %v", path, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, repositoryDirectory))
	if err != nil || len(entries) != 1 || entries[0].Name() != ".project-a.lock" {
		t.Fatalf("partial clone remains: %v, %v", entries, err)
	}
	r.Transport = nil
	if _, err := r.Prepare(context.Background(), "project-a", f.url); err != nil {
		t.Fatalf("retry after failed clone: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if path, err := r.Prepare(ctx, "project-a", f.url); path != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %q, %v", path, err)
	}
	r.Transport = &recordingTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("fetch secret must not leak")
	}}
	if path, err := r.Prepare(context.Background(), "project-a", f.url); path != "" || err == nil || err.Error() != "worktree: fetch failed" {
		t.Fatalf("fetch failure = %q, %v", path, err)
	}
}

func TestConcurrentPreparationsShareOneClone(t *testing.T) {
	f := newRemote(t)
	root := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	transport := &recordingTransport{run: func(ctx context.Context, path string, args ...string) ([]byte, error) {
		if args[0] == "clone" {
			close(entered)
			<-release
		}
		return safegit.Output(ctx, path, args...)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 2)
	go func() {
		_, err := (Repositories{WorktreeRoot: root, Transport: transport}).Prepare(ctx, "project-a", f.url)
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// While the first clone holds the lock, a second caller can cancel safely.
	short, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	defer stop()
	_, err := (Repositories{WorktreeRoot: root, Transport: transport}).Prepare(short, "project-a", f.url)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting for clone lock: %v", err)
	}
	go func() {
		_, err := (Repositories{WorktreeRoot: root, Transport: transport}).Prepare(ctx, "project-a", f.url)
		result <- err
	}()
	close(release)
	for range 2 {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	if got := transport.commands; !reflect.DeepEqual(got, []string{"clone", "fetch"}) {
		t.Fatalf("concurrent commands = %v", got)
	}
}

func TestPrepareValidatesInputsBeforeNetwork(t *testing.T) {
	for _, id := range []string{"", "..", "../owner", "project/a", ".hidden", "PROJECT"} {
		t.Run("id="+id, func(t *testing.T) {
			if _, err := (Repositories{WorktreeRoot: t.TempDir()}).Prepare(context.Background(), id, "https://example.invalid/repo.git"); err == nil {
				t.Fatal("invalid ID accepted")
			}
		})
	}
	for _, value := range []string{"", "/owner/repo", "./repo", "file:///owner/repo", "ext::command", "https://user:secret@example.invalid/repo", "ssh://git:secret@example.invalid/repo", "custom://example.invalid/repo"} {
		if networkURL(value) {
			t.Errorf("unsafe URL accepted: %q", value)
		}
	}
	for _, value := range []string{"https://example.invalid/repo.git", "http://127.0.0.1:1234/repo", "ssh://git@example.invalid/repo", "git@example.invalid:repo.git", "git://example.invalid/repo"} {
		if !networkURL(value) {
			t.Errorf("network URL rejected: %q", value)
		}
	}
}
