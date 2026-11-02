package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

func TestFetchBaseUsesRemoteBranchAndPreservesOwner(t *testing.T) {
	f := newRemote(t)
	ctx := context.Background()
	r := Repositories{WorktreeRoot: t.TempDir()}
	repo, err := r.Prepare(ctx, "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	initial := git(t, f.owner, "rev-parse", "HEAD")
	git(t, f.owner, "switch", "-c", "owners-work")
	write(t, filepath.Join(f.owner, "file.txt"), "owner's unstaged edit, distinct from index\n")
	git(t, repo, "checkout", "--no-track", "-b", "retained", initial)
	write(t, filepath.Join(repo, "file.txt"), "retained work\n")
	before, cloneBefore := snapshot(t, f.owner), snapshot(t, repo)
	ownerStatus := git(t, f.owner, "status", "--porcelain")
	if !strings.Contains(ownerStatus, "MM file.txt") || !strings.Contains(ownerStatus, "?? .env") {
		t.Fatalf("fixture lacks staged, unstaged and untracked changes: %s", ownerStatus)
	}
	// The Base Branch differs from both local main and the remote default HEAD.
	git(t, f.seed, "switch", "-c", "release/current")
	write(t, filepath.Join(f.seed, "file.txt"), "remote release\n")
	git(t, f.seed, "add", "file.txt")
	git(t, f.seed, "commit", "-m", "remote release")
	remoteSHA := git(t, f.seed, "rev-parse", "HEAD")
	git(t, f.remote, "fetch", "--", f.seed, "HEAD:refs/heads/release/current")
	for _, want := range []string{remoteSHA, initial} {
		// Exercise a non-fast-forward remote reset too, without changing HEAD.
		harnessGit(t, f.remote, "update-ref", "refs/heads/release/current", want)
		sha, err := r.FetchBase(ctx, "project-a", f.url, "release/current")
		if err != nil || sha != want || sha != git(t, f.remote, "rev-parse", "refs/heads/release/current") {
			t.Fatalf("base SHA=%s want=%s err=%v", sha, want, err)
		}
		if git(t, repo, "rev-parse", "HEAD") != initial || git(t, repo, "symbolic-ref", "HEAD") != "refs/heads/retained" {
			t.Fatal("fetch moved clone checkout")
		}
		if after := snapshot(t, repo); after["file.txt"] != cloneBefore["file.txt"] || after[".git/index"] != cloneBefore[".git/index"] {
			t.Fatal("fetch changed retained clone work or index")
		}
		if after := snapshot(t, f.owner); !reflect.DeepEqual(before, after) || git(t, f.owner, "status", "--porcelain") != ownerStatus ||
			git(t, f.owner, "symbolic-ref", "HEAD") != "refs/heads/owners-work" {
			t.Fatal("fetch changed owner's files, index, branch, refs or config")
		}
	}
}

func TestFetchBaseRejectsMissingRemoteBranchInsteadOfUsingStaleRef(t *testing.T) {
	f := newRemote(t)
	r := Repositories{WorktreeRoot: t.TempDir()}
	repo, err := r.Prepare(context.Background(), "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	stale := git(t, repo, "rev-parse", "refs/remotes/origin/main")
	harnessGit(t, f.remote, "update-ref", "-d", "refs/heads/main")
	sha, err := r.FetchBase(context.Background(), "project-a", f.url, "main")
	if err == nil || sha != "" || git(t, repo, "rev-parse", "refs/remotes/origin/main") != stale {
		t.Fatalf("missing branch returned stale SHA=%s err=%v", sha, err)
	}
}

func TestFetchBaseValidatesBeforeTransportAndRedactsFailures(t *testing.T) {
	f := newRemote(t)
	r := Repositories{WorktreeRoot: t.TempDir()}
	repo, err := r.Prepare(context.Background(), "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.Transport = &recordingTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("credential=super-secret")
	}}
	for _, branch := range []string{"", "../main", "main:refs/heads/main", "main\nsecret", "main^{commit}", "main*", "main.lock"} {
		if sha, err := r.FetchBase(context.Background(), "project-a", f.url, branch); sha != "" || err == nil || calls != 0 {
			t.Fatalf("invalid branch=%q SHA=%s calls=%d err=%v", branch, sha, calls, err)
		}
	}
	if _, err := r.FetchBase(context.Background(), "project-a", f.url, "main"); err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatalf("unredacted transport error: %v calls=%d", err, calls)
	}
	// Revalidate destination overrides before this new network operation too.
	harnessGit(t, repo, "config", "url."+f.owner+".insteadOf", f.url)
	if _, err := r.FetchBase(context.Background(), "project-a", f.url, "main"); err == nil || calls != 1 {
		t.Fatalf("destination override reached transport: %v calls=%d", err, calls)
	}
}

func TestFetchBaseCancellationAndSafeFetchMapping(t *testing.T) {
	f := newRemote(t)
	r := Repositories{WorktreeRoot: t.TempDir()}
	repo, err := r.Prepare(context.Background(), "project-a", f.url)
	if err != nil {
		t.Fatal(err)
	}
	initial := git(t, repo, "rev-parse", "HEAD")
	harnessGit(t, repo, "config", "remote.origin.fetch", "+refs/heads/*:refs/heads/*")
	// A hook and execution setting must stay disabled on the fetch path.
	marker := filepath.Join(repo, "hook-ran")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, ".git", "hooks", "reference-transaction"), "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(filepath.Join(repo, ".git", "hooks", "reference-transaction"), 0700); err != nil {
		t.Fatal(err)
	}
	harnessGit(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'")
	write(t, filepath.Join(f.seed, "file.txt"), "advance\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "advance")
	git(t, f.remote, "fetch", "--", f.seed, "HEAD:refs/heads/main")
	r.Transport = &recordingTransport{run: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		return safegit.Output(ctx, dir, args...)
	}}
	sha, err := r.FetchBase(context.Background(), "project-a", f.url, "main")
	if err != nil || sha == initial || git(t, repo, "rev-parse", "HEAD") != initial {
		t.Fatalf("unsafe fetch mapping: sha=%s err=%v", sha, err)
	}
	if _, exists := snapshot(t, repo)["hook-ran"]; exists {
		t.Fatal("fetch ran a hook or execution setting")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sha, err := r.FetchBase(ctx, "project-a", f.url, "main"); sha != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch: sha=%s err=%v", sha, err)
	}
}
