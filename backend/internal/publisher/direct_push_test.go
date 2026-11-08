package publisher_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

// Spec 17 3.7.2: with direct_push the verified commit is pushed to the Base
// Branch of a real bare remote, the remote SHA is verified_sha, pushed_sha is
// recorded and no PR is searched or created.
func TestPublishDirectPushUpdatesBaseBranchWithoutPR(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, steps := publishConfig(f)
	checks := 0
	config.DirectPushCheck = func(context.Context) error { checks++; return nil }
	config.Push.CheckCancellation = func(context.Context) error {
		t.Error("direct push must use the publish check")
		return nil
	}
	publication, err := publisher.Publish(context.Background(), f.target(), config)
	if err != nil || publication.PushedSHA != f.head || publication.PR != nil || publication.Created {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != f.head {
		t.Fatalf("remote main=%s verified=%s", got, f.head)
	}
	if refs := f.git(f.remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Fatalf("other refs pushed: %s", refs)
	}
	if checks != 1 || prs.created != 0 || prs.commented != 0 || prs.head != "" {
		t.Fatalf("checks=%d PR operations=%+v", checks, prs)
	}
	// The result API binds pushed reports to the Execution's own branch.
	if recorder.pushedSHA != f.head || recorder.branch != f.target().BranchName {
		t.Fatalf("recorded: %+v", recorder)
	}
	if !reflect.DeepEqual(*steps, []string{"record_push"}) {
		t.Fatalf("steps=%v", *steps)
	}
}

// 3.7.1 step 2: a policy changed to require_approval right before the push
// takes the PR path; the Base Branch is not pushed.
func TestPublishDirectPushPolicyChangedCreatesPR(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, steps := publishConfig(f)
	heartbeat := 0
	config.Push.CheckCancellation = func(context.Context) error { heartbeat++; return nil }
	config.DirectPushCheck = func(context.Context) error { return publisher.ErrPolicyChanged }
	publication, err := publisher.Publish(context.Background(), f.target(), config)
	if err != nil || publication.PushedSHA != f.head || !publication.Created || publication.PR == nil {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != f.parent {
		t.Fatalf("remote main moved to %s", got)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/"+f.target().BranchName); got != f.head {
		t.Fatalf("task branch=%s", got)
	}
	if heartbeat != 1 || prs.created != 1 || prs.base != "main" || recorder.branch != f.target().BranchName {
		t.Fatalf("heartbeat=%d PR=%+v recorded=%+v", heartbeat, prs, recorder)
	}
	if !reflect.DeepEqual(*steps, []string{"record_push", "find", "create", "record_pr"}) {
		t.Fatalf("steps=%v", *steps)
	}
}

// 3.7.1 (Fail Closed): when publish-check cannot reach the Control Plane,
// neither the Base Branch nor the task branch is pushed and no PR is made.
func TestPublishDirectPushUnreachableControlPlaneDoesNotPush(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, steps := publishConfig(f)
	config.DirectPushCheck = func(context.Context) error { return errors.New("connection refused " + secret) }
	publication, err := publisher.Publish(context.Background(), f.target(), config)
	if !errors.Is(err, publisher.ErrControlPlaneUnavailable) || publisher.ErrorClass(err) != model.ErrorPublishFailed || publication.PushedSHA != "" {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	if refs := f.git(f.remote, "for-each-ref", "--format=%(objectname) %(refname)"); refs != f.parent+" refs/heads/main" {
		t.Fatalf("remote changed: %s", refs)
	}
	if prs.created != 0 || prs.head != "" || recorder.pushedSHA != "" || len(*steps) != 0 {
		t.Fatalf("PR=%+v recorded=%+v steps=%v", prs, recorder, *steps)
	}
}

// 3.7.1 step 4: besides evaloom/ branches, only the exact Base Branch is
// allowed, and only when the approval policy is direct_push.
func TestPushRefRuleAllowsBaseBranchOnlyForDirectPush(t *testing.T) {
	for _, c := range []struct {
		name, branch, directBase string
		allowed                  bool
	}{
		{"base without direct_push", "main", "", false},
		{"other branch with direct_push", "develop", "main", false},
		{"qualified base name", "refs/heads/main", "main", false},
		{"base prefix", "main2", "main", false},
		{"base with direct_push", "main", "main", true},
		{"evaloom branch with direct_push", "evaloom/task-1-short", "main", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPushFixture(t)
			target := f.target()
			target.BranchName = c.branch
			f.config.DirectPushBase = c.directBase
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pushed, err := publisher.Push(ctx, target, f.config)
			refs := f.git(f.remote, "for-each-ref", "--format=%(objectname) %(refname)")
			if !c.allowed {
				if pushed != "" || !errors.Is(err, publisher.ErrBranchName) {
					t.Fatalf("pushed=%q err=%v", pushed, err)
				}
				if refs != f.parent+" refs/heads/main" {
					t.Fatalf("rejected ref updated remote: %s", refs)
				}
				return
			}
			if err != nil || pushed != f.head {
				t.Fatalf("pushed=%q err=%v", pushed, err)
			}
			if got := f.git(f.remote, "rev-parse", "refs/heads/"+c.branch); got != f.head {
				t.Fatalf("remote %s=%s", c.branch, got)
			}
		})
	}
}
