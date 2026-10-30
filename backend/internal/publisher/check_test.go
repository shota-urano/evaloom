package publisher_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

const secret = "ghs_secretTokenValue"

type fixture struct {
	t        *testing.T
	worktree string
	parent   string
	head     string
	env      []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, worktree: filepath.Join(root, "worktree")}
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			f.env = append(f.env, variable)
		}
	}
	f.env = append(f.env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	clone := filepath.Join(root, "clone")
	f.git(root, "init", "-q", "--initial-branch=main", clone)
	f.git(clone, "config", "user.name", "Evaloom Test")
	f.git(clone, "config", "user.email", "test@example.invalid")
	f.write(filepath.Join(clone, "file.txt"), "initial\n")
	f.write(filepath.Join(clone, ".gitignore"), "build/\n")
	f.git(clone, "add", ".")
	f.git(clone, "commit", "-qm", "initial")
	f.parent = f.git(clone, "rev-parse", "HEAD")
	f.git(clone, "worktree", "add", "-qb", "evaloom/task-1-short", f.worktree)
	f.write(filepath.Join(f.worktree, "file.txt"), "changed by harness\n")
	f.git(f.worktree, "commit", "-qam", "harness")
	f.head = f.git(f.worktree, "rev-parse", "HEAD")
	return f
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) target() publisher.Target {
	return publisher.Target{Worktree: f.worktree, VerifiedSHA: f.head, BranchName: "evaloom/task-1-short"}
}

// publish stands in for the publish stage: it pushes only when the check passes.
func publish(ctx context.Context, target publisher.Target, check func(context.Context) error) (pushed bool, err error) {
	if err := publisher.CheckBeforePush(ctx, target, check); err != nil {
		return false, err
	}
	return true, nil
}

func TestCheckBeforePushAllowsVerifiedCleanEvaloomBranch(t *testing.T) {
	f := newFixture(t)
	// Ignored build output is not an uncommitted change.
	f.write(filepath.Join(f.worktree, "build", "out.bin"), "artifact\n")
	checked := false
	pushed, err := publish(context.Background(), f.target(), func(context.Context) error {
		checked = true
		return nil
	})
	if err != nil || !pushed || !checked {
		t.Fatalf("pushed=%v checked=%v err=%v", pushed, checked, err)
	}
}

func TestCheckBeforePushDoesNotPushWhenCancellationIsRequested(t *testing.T) {
	for _, checkFails := range []bool{false, true} {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		// Runner.Heartbeat cancels the execution context when the reply lists it.
		pushed, err := publish(ctx, f.target(), func(context.Context) error {
			cancel()
			if checkFails {
				return errors.New("late transport error")
			}
			return nil
		})
		cancel()
		if pushed || !errors.Is(err, context.Canceled) || publisher.ErrorClass(err) != model.ErrorCancelled {
			t.Fatalf("checkFails=%v pushed=%v err=%v", checkFails, pushed, err)
		}
	}
}

func TestCheckBeforePushDoesNotPushWhenControlPlaneIsUnreachable(t *testing.T) {
	f := newFixture(t)
	pushed, err := publish(context.Background(), f.target(), func(context.Context) error {
		return errors.New("POST https://control.invalid/?token=" + secret + ": connection refused")
	})
	if pushed || !errors.Is(err, publisher.ErrControlPlaneUnavailable) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
		t.Fatalf("pushed=%v err=%v", pushed, err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "control.invalid") {
		t.Fatalf("error exposes check details: %v", err)
	}
}

func TestCheckBeforePushRejectsBranchesOutsideEvaloomNamespace(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{
		"main", "feature/x", "evaloom", "evaloom/", "Evaloom/x", "refs/heads/evaloom/x", "xevaloom/x",
		"evaloom/../main", "evaloom/a..b", "evaloom/x.lock", "evaloom/a b", "evaloom/x@{1}", "evaloom//x", "evaloom/x/",
	} {
		target := f.target()
		target.BranchName = name
		checked := false
		pushed, err := publish(context.Background(), target, func(context.Context) error {
			checked = true
			return nil
		})
		if pushed || checked || !errors.Is(err, publisher.ErrBranchName) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
			t.Fatalf("%q: pushed=%v checked=%v err=%v", name, pushed, checked, err)
		}
		if err.Error() != publisher.ErrBranchName.Error() {
			t.Fatalf("%q: error exposes branch name", name)
		}
	}
}

func TestCheckBeforePushRejectsHeadOtherThanVerifiedSHA(t *testing.T) {
	f := newFixture(t)
	for _, sha := range []string{f.parent, strings.ToUpper(f.head), f.head[:12], "", "HEAD", f.head + "\n"} {
		target := f.target()
		target.VerifiedSHA = sha
		pushed, err := publish(context.Background(), target, func(context.Context) error { return nil })
		if pushed || !(errors.Is(err, publisher.ErrHeadMismatch) || errors.Is(err, publisher.ErrVerifiedSHA)) {
			t.Fatalf("%q: pushed=%v err=%v", sha, pushed, err)
		}
	}
	// HEAD moved after verification.
	f.write(filepath.Join(f.worktree, "file.txt"), "later\n")
	f.git(f.worktree, "commit", "-qam", "after gate")
	pushed, err := publish(context.Background(), f.target(), func(context.Context) error { return nil })
	if pushed || !errors.Is(err, publisher.ErrHeadMismatch) {
		t.Fatalf("pushed=%v err=%v", pushed, err)
	}
}

func TestCheckBeforePushRejectsUncommittedChanges(t *testing.T) {
	for name, change := range map[string]func(f *fixture){
		"modified": func(f *fixture) { f.write(filepath.Join(f.worktree, "file.txt"), "uncommitted\n") },
		"staged": func(f *fixture) {
			f.write(filepath.Join(f.worktree, "file.txt"), "staged\n")
			f.git(f.worktree, "add", "file.txt")
		},
		"deleted":   func(f *fixture) { os.Remove(filepath.Join(f.worktree, "file.txt")) },
		"untracked": func(f *fixture) { f.write(filepath.Join(f.worktree, "nested", "new.txt"), "new\n") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f)
			checked := false
			pushed, err := publish(context.Background(), f.target(), func(context.Context) error {
				checked = true
				return nil
			})
			if pushed || checked || !errors.Is(err, publisher.ErrDirtyWorktree) || publisher.ErrorClass(err) != model.ErrorPublishFailed {
				t.Fatalf("pushed=%v checked=%v err=%v", pushed, checked, err)
			}
		})
	}
}

func TestCheckBeforePushStopsBeforeAnyCheckWhenAlreadyCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checked := false
	pushed, err := publish(ctx, f.target(), func(context.Context) error {
		checked = true
		return nil
	})
	if pushed || checked || !errors.Is(err, context.Canceled) {
		t.Fatalf("pushed=%v checked=%v err=%v", pushed, checked, err)
	}
	if _, err := publish(context.Background(), f.target(), nil); err == nil {
		t.Fatal("missing cancellation check was accepted")
	}
}
