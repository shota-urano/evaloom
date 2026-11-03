package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func prepareCreation(t *testing.T) (remoteFixture, Repositories, string, CreateRequest) {
	t.Helper()
	f := newRemote(t)
	r := Repositories{WorktreeRoot: t.TempDir()}
	repository, err := r.Prepare(context.Background(), "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.FetchBase(context.Background(), "project-a", f.url, "main")
	if err != nil {
		t.Fatal(err)
	}
	return f, r, repository, CreateRequest{ProjectID: "project-a", RepositoryURL: f.url,
		TaskID: "task-001", ShortName: "add-login", BaseSHA: base}
}

func TestCreateNormalAndFreshNamesAndRealPaths(t *testing.T) {
	f, r, repository, request := prepareCreation(t)
	ownerBefore := snapshot(t, f.owner)
	cloneHead := git(t, repository, "rev-parse", "HEAD")
	// Worktree Root itself may have a symlink in its spelling. Return the real
	// path and measure containment against the resolved root, not a prefix.
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(r.WorktreeRoot, alias); err != nil {
		t.Fatal(err)
	}
	r.WorktreeRoot = alias
	for _, test := range []struct {
		ordinal int
		branch  string
		path    string
	}{
		{0, "evaloom/task-001-add-login", "task-001"},
		{1, "evaloom/task-001-add-login-r1", "task-001-r1"},
		{2, "evaloom/task-001-add-login-r2", "task-001-r2"},
	} {
		request.FreshNumber = test.ordinal
		created, err := r.Create(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if created.BranchName != test.branch || created.BaseSHA != request.BaseSHA ||
			git(t, created.Path, "symbolic-ref", "HEAD") != "refs/heads/"+test.branch ||
			git(t, created.Path, "rev-parse", "HEAD") != request.BaseSHA {
			t.Fatalf("creation = %+v, want branch=%s base=%s", created, test.branch, request.BaseSHA)
		}
		realRoot, err := filepath.EvalSymlinks(r.WorktreeRoot)
		if err != nil {
			t.Fatal(err)
		}
		realPath, err := filepath.EvalSymlinks(created.Path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(realRoot, realPath)
		if err != nil || rel != test.path || realPath != created.Path || !filepath.IsAbs(created.Path) {
			t.Fatalf("real path=%s root=%s relative=%s err=%v", realPath, realRoot, rel, err)
		}
		if data, err := os.ReadFile(filepath.Join(created.Path, "file.txt")); err != nil || string(data) != "initial\n" {
			t.Fatalf("worktree contents=%q err=%v", data, err)
		}
	}
	if after := snapshot(t, f.owner); !reflect.DeepEqual(ownerBefore, after) {
		t.Fatal("creation changed owner's files, index, refs, objects or configuration")
	}
	if git(t, repository, "rev-parse", "HEAD") != cloneHead {
		t.Fatal("creation changed the clone's checkout")
	}
}

func TestCreateRejectsLocalAndLiveRemoteBranchWithoutChangingSHA(t *testing.T) {
	for _, collision := range []string{"local", "packed-local", "live-remote", "fresh"} {
		t.Run(collision, func(t *testing.T) {
			f, r, repository, request := prepareCreation(t)
			oldSHA := request.BaseSHA
			write(t, filepath.Join(f.seed, "file.txt"), "new base\n")
			git(t, f.seed, "add", ".")
			git(t, f.seed, "commit", "-m", "new base")
			git(t, f.remote, "fetch", "--", f.seed, "HEAD:refs/heads/main")
			var err error
			request.BaseSHA, err = r.FetchBase(context.Background(), request.ProjectID, f.url, "main")
			if err != nil || request.BaseSHA == oldSHA {
				t.Fatalf("fixture requires different existing/new SHAs: %v", err)
			}
			ref := "refs/heads/evaloom/task-001-add-login"
			if collision == "fresh" {
				request.FreshNumber = 2
				ref += "-r2"
			}
			where := repository
			if collision == "live-remote" {
				where = f.remote
			}
			// Add the remote collision AFTER the final fetch, leaving no cached
			// tracking ref. Only a live ls-remote can discover it.
			harnessGit(t, where, "update-ref", ref, oldSHA)
			if collision == "packed-local" {
				harnessGit(t, where, "pack-refs", "--all")
			}
			before := readRepositoryState(t, repository)
			created, err := r.Create(context.Background(), request)
			if err == nil || created != (Created{}) || git(t, where, "rev-parse", ref) != oldSHA {
				t.Fatalf("collision overwrote existing branch: created=%+v err=%v", created, err)
			}
			if after := readRepositoryState(t, repository); before != after {
				t.Fatal("collision changed local refs or configuration")
			}
			suffix := ""
			if collision == "fresh" {
				suffix = "-r2"
			}
			if _, err := os.Lstat(filepath.Join(r.WorktreeRoot, request.TaskID+suffix)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("collision created a worktree path: %v", err)
			}
		})
	}
}

func TestCreateRejectsExistingPathsAndSymlinkEscape(t *testing.T) {
	for _, kind := range []string{"empty-directory", "directory-with-files", "outside-symlink", "dangling-symlink"} {
		t.Run(kind, func(t *testing.T) {
			_, r, repository, request := prepareCreation(t)
			path := filepath.Join(r.WorktreeRoot, request.TaskID)
			outside := t.TempDir()
			write(t, filepath.Join(outside, "keep"), "untouched")
			if strings.Contains(kind, "symlink") {
				target := outside
				if kind == "dangling-symlink" {
					target = filepath.Join(outside, "missing")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "directory-with-files" {
					write(t, filepath.Join(path, "keep"), "untouched")
				}
			}
			outsideBefore := snapshot(t, outside)
			before := readRepositoryState(t, repository)
			created, err := r.Create(context.Background(), request)
			if err == nil || created != (Created{}) || strings.Contains(kind, "symlink") && !errors.Is(err, ErrWorktreePath) {
				t.Fatalf("existing path accepted: created=%+v err=%v", created, err)
			}
			if before != readRepositoryState(t, repository) || !reflect.DeepEqual(outsideBefore, snapshot(t, outside)) {
				t.Fatal("path rejection changed repository or files outside the root")
			}
			if !strings.Contains(kind, "symlink") {
				files := snapshot(t, path)
				if kind == "empty-directory" && len(files) != 0 || kind == "directory-with-files" && (len(files) != 1 || files["keep"] != "untouched") {
					t.Fatalf("existing directory changed: %v", files)
				}
			}
		})
	}
}

func TestCreateValidatesInputsAndRedactsRemoteFailure(t *testing.T) {
	_, r, repository, valid := prepareCreation(t)
	calls := 0
	r.Transport = &recordingTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("credential=secret")
	}}
	for _, change := range []func(*CreateRequest){
		func(r *CreateRequest) { r.TaskID = "../outside" },
		func(r *CreateRequest) { r.TaskID = "task-001/../../outside" },
		func(r *CreateRequest) { r.ShortName = "bad/name" },
		func(r *CreateRequest) { r.ShortName = "name\nsecret" },
		func(r *CreateRequest) { r.ShortName = "" },
		func(r *CreateRequest) { r.BaseSHA = "HEAD" },
		func(r *CreateRequest) { r.FreshNumber = -1 },
	} {
		request := valid
		change(&request)
		if _, err := r.Create(context.Background(), request); err == nil || calls != 0 {
			t.Fatalf("invalid request reached transport: %v calls=%d", err, calls)
		}
	}
	before := readRepositoryState(t, repository)
	if _, err := r.Create(context.Background(), valid); err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatalf("remote failure: %v calls=%d", err, calls)
	}
	if before != readRepositoryState(t, repository) {
		t.Fatal("remote failure changed refs or configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Create(ctx, valid); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled creation: %v calls=%d", err, calls)
	}
}

func TestCreateConcurrentCallsNeverResetExistingBranch(t *testing.T) {
	_, r, repository, request := prepareCreation(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := r.Create(context.Background(), request)
			results <- err
		})
	}
	wg.Wait()
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 || git(t, repository, "rev-parse", "refs/heads/evaloom/task-001-add-login") != request.BaseSHA {
		t.Fatalf("concurrent creation successes=%d", successes)
	}
}

func TestCreateUsesSafeGitForCheckout(t *testing.T) {
	_, r, repository, request := prepareCreation(t)
	marker := filepath.Join(r.WorktreeRoot, "hook-ran")
	hook := filepath.Join(repository, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, hook, "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	harnessGit(t, repository, "config", "core.fsmonitor", "touch '"+marker+"'")
	if _, err := r.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree add executed sandbox configuration: %v", err)
	}
}
