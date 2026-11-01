package publisher_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

type pushFixture struct {
	*fixture
	remote string
	config publisher.PushConfig
}

// The remote is a real local bare repository. The daemon supplies a network
// transport because safegit deliberately prohibits local receive-pack, whose
// hooks would otherwise run on the Trusted Runner host.
func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	f := &pushFixture{fixture: newFixture(t)}
	root := filepath.Dir(f.worktree)
	f.remote = filepath.Join(root, "remote.git")
	f.git(root, "init", "-q", "--bare", f.remote)
	f.git(f.worktree, "push", "-q", f.remote, f.parent+":refs/heads/main")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "git", "daemon", "--reuseaddr", "--listen=127.0.0.1", "--port="+port,
		"--export-all", "--enable=receive-pack", "--base-path="+root, root)
	cmd.Env = f.env
	// git starts git-daemon as a child; stop the whole group so Wait cannot
	// hang on inherited output pipes when the fixture is cleaned up.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git daemon did not listen within 5 seconds")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.config = publisher.PushConfig{
		RepositoryURL:     "git://" + address + "/remote.git",
		CheckCancellation: func(context.Context) error { return nil },
		RecordRemoteMismatch: func(context.Context, string, string) error {
			t.Error("unexpected remote SHA mismatch")
			return nil
		},
	}
	return f
}

func (f *pushFixture) push() (string, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return publisher.Push(ctx, f.target(), f.config)
}

func TestPushVerifiedSHAAndReadRemote(t *testing.T) {
	for _, state := range []string{"new", "fast-forward", "already-pushed", "detached-head"} {
		t.Run(state, func(t *testing.T) {
			f := newPushFixture(t)
			ref := "refs/heads/" + f.target().BranchName
			switch state {
			case "fast-forward":
				f.git(f.worktree, "push", f.remote, f.parent+":"+ref)
			case "already-pushed":
				f.git(f.worktree, "push", f.remote, f.head+":"+ref)
			case "detached-head":
				f.git(f.worktree, "checkout", "--detach", f.head)
			}
			// Local defaults must not publish this tag or delete remote main.
			f.git(f.worktree, "tag", "-a", "-m", "not published", "private-tag", f.head)
			f.git(f.worktree, "config", "push.followTags", "true")
			pushed, err := f.push()
			if err != nil || pushed != f.head || publisher.ErrorClass(err) != "" {
				t.Fatalf("pushed=%q err=%v", pushed, err)
			}
			if got := f.git(f.remote, "rev-parse", ref); got != f.head {
				t.Fatalf("remote=%s want verified=%s", got, f.head)
			}
			refs := f.git(f.remote, "for-each-ref", "--format=%(refname)")
			if refs != ref+"\nrefs/heads/main" {
				t.Fatalf("unexpected remote refs: %s", refs)
			}
		})
	}
}

func TestPushUsesExplicitVerifiedSHARatherThanResolvingHEAD(t *testing.T) {
	f := newPushFixture(t)
	// Simulate HEAD changing after the local check. Even then the refspec
	// must identify the immutable tested commit, rather than resolve HEAD.
	f.config.CheckCancellation = func(context.Context) error {
		f.git(f.worktree, "checkout", "--detach", f.parent)
		return nil
	}
	pushed, err := f.push()
	if err != nil || pushed != f.head {
		t.Fatalf("pushed=%q err=%v", pushed, err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/"+f.target().BranchName); got != f.head {
		t.Fatalf("remote=%s want verified=%s", got, f.head)
	}
}

func TestPushNonFastForwardFailsWithoutForce(t *testing.T) {
	for _, state := range []string{"diverged", "remote-ahead"} {
		t.Run(state, func(t *testing.T) {
			f := newPushFixture(t)
			ref := "refs/heads/" + f.target().BranchName
			if state == "diverged" {
				f.git(f.worktree, "checkout", "--detach", f.parent)
			}
			f.write(filepath.Join(f.worktree, "file.txt"), "another publisher\n")
			f.git(f.worktree, "commit", "-qam", "remote change")
			other := f.git(f.worktree, "rev-parse", "HEAD")
			f.git(f.worktree, "push", f.remote, other+":"+ref)
			f.git(f.worktree, "checkout", "--detach", f.head)
			// The receiver allows non-FF updates; only a non-forced sender
			// prevents overwriting the other commit in this test.
			f.git(f.remote, "config", "receive.denyNonFastForwards", "false")
			pushed, err := f.push()
			if pushed != "" || !errors.Is(err, publisher.ErrPush) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
				t.Fatalf("pushed=%q class=%s err=%v", pushed, publisher.ErrorClass(err), err)
			}
			if got := f.git(f.remote, "rev-parse", ref); got != other {
				t.Fatalf("non-FF push overwrote remote: got=%s want=%s", got, other)
			}
		})
	}
}

func TestPushChecksRemoteSHAAndAuditsMismatch(t *testing.T) {
	for _, state := range []string{"changed", "deleted", "audit-error", "cancelled-audit"} {
		t.Run(state, func(t *testing.T) {
			f := newPushFixture(t)
			ref := "refs/heads/" + f.target().BranchName
			// Trusted receiver hook changes the branch after a successful
			// receive, exercising the actual post-push read over the network.
			command := "git update-ref '" + ref + "' '" + f.parent + "'"
			wantObserved := f.parent
			if state == "deleted" {
				command = "git update-ref -d '" + ref + "'"
				wantObserved = ""
			}
			hook := filepath.Join(f.remote, "hooks", "post-receive")
			f.write(hook, "#!/bin/sh\n"+command+"\n")
			if err := os.Chmod(hook, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			audited := false
			f.config.RecordRemoteMismatch = func(auditCtx context.Context, verified, observed string) error {
				audited = true
				if state == "cancelled-audit" {
					cancel()
					if err := auditCtx.Err(); err != nil {
						t.Errorf("push cancellation interrupted mismatch audit: %v", err)
					}
				}
				if verified != f.head || observed != wantObserved {
					t.Errorf("audit verified=%q observed=%q", verified, observed)
				}
				if state == "audit-error" {
					return errors.New(secret)
				}
				return nil
			}
			pushed, err := publisher.Push(ctx, f.target(), f.config)
			if pushed != "" || !audited || !errors.Is(err, publisher.ErrRemoteSHA) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
				t.Fatalf("pushed=%q audited=%v err=%v", pushed, audited, err)
			}
			if strings.Contains(err.Error(), secret) || (state == "audit-error" && !errors.Is(err, publisher.ErrAudit)) {
				t.Fatalf("unsafe or missing audit error: %v", err)
			}
		})
	}
}

func TestPushPreflightFailuresDoNotUpdateRemote(t *testing.T) {
	for _, reason := range []string{"cancelled", "unreachable", "dirty", "head", "branch"} {
		t.Run(reason, func(t *testing.T) {
			f := newPushFixture(t)
			target := f.target()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			want := model.ErrorPublishFailed
			switch reason {
			case "cancelled":
				f.config.CheckCancellation = func(context.Context) error { cancel(); return nil }
				want = model.ErrorCancelled
			case "unreachable":
				f.config.CheckCancellation = func(context.Context) error { return errors.New(secret) }
			case "dirty":
				f.write(filepath.Join(f.worktree, "file.txt"), "uncommitted\n")
			case "head":
				target.VerifiedSHA = f.parent
			case "branch":
				target.BranchName = "main"
			}
			pushed, err := publisher.Push(ctx, target, f.config)
			if pushed != "" || publisher.ErrorClass(err) != want {
				t.Fatalf("pushed=%q err=%v", pushed, err)
			}
			if refs := f.git(f.remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
				t.Fatalf("preflight failure updated remote: %s", refs)
			}
		})
	}
}

func TestPushRemoteInspectionFailureDoesNotReturnPushedSHA(t *testing.T) {
	f := newPushFixture(t)
	// Push succeeds, but the receiver refuses the subsequent upload-pack used
	// by ls-remote. The presence of the commit alone must not imply success.
	hook := filepath.Join(f.remote, "hooks", "post-receive")
	f.write(hook, "#!/bin/sh\ngit config daemon.uploadpack false\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	pushed, err := f.push()
	if pushed != "" || !errors.Is(err, publisher.ErrRemoteInspection) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
		t.Fatalf("pushed=%q err=%v", pushed, err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/"+f.target().BranchName); got != f.head {
		t.Fatalf("remote=%q want=%q", got, f.head)
	}
}

func TestPushRejectionDoesNotExposeGitOutput(t *testing.T) {
	f := newPushFixture(t)
	hook := filepath.Join(f.remote, "hooks", "pre-receive")
	f.write(hook, "#!/bin/sh\necho '"+secret+"' >&2\nexit 1\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	pushed, err := f.push()
	if pushed != "" || !errors.Is(err, publisher.ErrPush) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
		t.Fatalf("pushed=%q err=%v", pushed, err)
	}
	if err.Error() != publisher.ErrPush.Error() {
		t.Fatalf("push error exposes Git output: %v", err)
	}
}

func TestPushRejectsDestinationOverrides(t *testing.T) {
	for _, setting := range []string{"insteadOf", "pushInsteadOf", "remote-name", "remotes", "branches"} {
		t.Run(setting, func(t *testing.T) {
			f := newPushFixture(t)
			if setting == "remotes" || setting == "branches" {
				legacy := f.git(f.worktree, "rev-parse", "--path-format=absolute", "--git-path", setting)
				f.write(filepath.Join(legacy, f.config.RepositoryURL), "URL: "+f.config.RepositoryURL+"\n")
			} else if setting == "remote-name" {
				f.git(f.worktree, "config", "remote."+f.config.RepositoryURL+".url", f.config.RepositoryURL)
				f.git(f.worktree, "config", "remote."+f.config.RepositoryURL+".mirror", "true")
			} else {
				f.git(f.worktree, "config", "url."+f.config.RepositoryURL+"."+setting, f.config.RepositoryURL)
			}
			pushed, err := f.push()
			if pushed != "" || !errors.Is(err, publisher.ErrPushDestination) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
				t.Fatalf("pushed=%q err=%v", pushed, err)
			}
			if refs := f.git(f.remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
				t.Fatalf("destination override updated remote: %s", refs)
			}
		})
	}
}

func TestPushDisablesSenderHooks(t *testing.T) {
	f := newPushFixture(t)
	hooks := filepath.Join(filepath.Dir(f.worktree), "sender-hooks")
	marker := filepath.Join(hooks, "ran")
	hook := filepath.Join(hooks, "pre-push")
	f.write(hook, "#!/bin/sh\ntouch '"+marker+"'\nexit 1\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(f.worktree, "config", "core.hooksPath", hooks)
	pushed, err := f.push()
	if err != nil || pushed != f.head {
		t.Fatalf("pushed=%q err=%v", pushed, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sender hook ran: stat=%v", err)
	}
}

func TestPushRequiresConfiguration(t *testing.T) {
	f := newFixture(t)
	for _, config := range []publisher.PushConfig{
		{},
		{RepositoryURL: "https://example.invalid/project", CheckCancellation: func(context.Context) error { return nil }},
	} {
		pushed, err := publisher.Push(context.Background(), f.target(), config)
		if pushed != "" || !errors.Is(err, publisher.ErrPushConfiguration) {
			t.Fatalf("pushed=%q err=%v", pushed, err)
		}
	}
}
