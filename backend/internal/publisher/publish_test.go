package publisher_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

type fakePRs struct {
	open                                *publisher.PullRequest
	findErr, createErr, commentErr      error
	created, commented                  int
	repository, head, base, title, body string
	steps                               *[]string
	afterCreate                         func()
}

func (f *fakePRs) FindOpen(_ context.Context, repository, head string) (*publisher.PullRequest, error) {
	*f.steps = append(*f.steps, "find")
	f.repository, f.head = repository, head
	return f.open, f.findErr
}

func (f *fakePRs) Create(_ context.Context, repository, head, base, title, body string) (publisher.PullRequest, error) {
	*f.steps = append(*f.steps, "create")
	f.created++
	f.repository, f.head, f.base, f.title, f.body = repository, head, base, title, body
	if f.createErr != nil {
		return publisher.PullRequest{}, f.createErr
	}
	f.open = &publisher.PullRequest{Number: 17, URL: "https://github.com/owner/repo/pull/17"}
	if f.afterCreate != nil {
		f.afterCreate()
	}
	return *f.open, nil
}

func (f *fakePRs) Comment(_ context.Context, repository string, number int, body string) error {
	*f.steps = append(*f.steps, "comment")
	f.commented++
	f.repository, f.body = repository, body
	if f.open == nil || f.open.Number != number {
		return errors.New("wrong PR")
	}
	return f.commentErr
}

// Stands in for durable executions metadata and events, bound to one execution.
type fakePublicationRecorder struct {
	pushedSHA, branch string
	pr                publisher.PullRequest
	created           bool
	pushErr, prErr    error
	steps             *[]string
	afterPush         func()
}

func (r *fakePublicationRecorder) RecordPushed(ctx context.Context, sha, branch string) error {
	*r.steps = append(*r.steps, "record_push")
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.pushErr != nil {
		return r.pushErr
	}
	r.pushedSHA, r.branch = sha, branch
	if r.afterPush != nil {
		r.afterPush()
	}
	return nil
}

func (r *fakePublicationRecorder) RecordPR(ctx context.Context, pr publisher.PullRequest, created bool) error {
	*r.steps = append(*r.steps, "record_pr")
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.prErr != nil {
		return r.prErr
	}
	r.pr, r.created = pr, created
	return nil
}

func executionReport(sha string) publisher.Report {
	total, skipped := 8, 2
	return publisher.Report{
		ExecutionID: "execution-1", TaskID: "task-1", Title: "PR を作成する",
		AcceptanceCriteria: "- URL を記録する\n- 既存 PR にコメントする", Summary: "PR の作成とコメントを実装した。",
		HarnessID: "harness-1", HarnessVersion: "1.2.3", ProfileVersion: "v3", ProfileCommitSHA: strings.Repeat("a", 40),
		Gate2Result: "pass", Gate3Result: "pass",
		VerifyRun: model.TestRun{ExecutionID: "execution-1", Kind: "verify", TargetSHA: sha, Total: &total, Skipped: &skipped},
	}
}

func publishConfig(f *pushFixture) (publisher.PublishConfig, *fakePRs, *fakePublicationRecorder, *[]string) {
	steps := []string{}
	prs := &fakePRs{steps: &steps}
	recorder := &fakePublicationRecorder{steps: &steps}
	return publisher.PublishConfig{Push: f.config, BaseBranch: "main", PullRequests: prs, Recorder: recorder,
		ReportContext: context.Background(), Report: executionReport(f.head)}, prs, recorder, &steps
}

func TestPublishCreatesOnePRAndRecordsURLAfterConfirmedPush(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, steps := publishConfig(f)
	publication, err := publisher.Publish(context.Background(), f.target(), config)
	if err != nil || publication.PushedSHA != f.head || !publication.Created || publication.PR == nil {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	if prs.created != 1 || prs.commented != 0 || prs.repository != f.config.RepositoryURL || prs.head != f.target().BranchName || prs.base != "main" || prs.title != "task-1: PR を作成する" {
		t.Fatalf("PR operations: %+v", prs)
	}
	if recorder.pushedSHA != f.head || recorder.branch != f.target().BranchName || recorder.pr != *publication.PR || !recorder.created {
		t.Fatalf("recorded: %+v", recorder)
	}
	if !reflect.DeepEqual(*steps, []string{"record_push", "find", "create", "record_pr"}) {
		t.Fatalf("steps=%v", *steps)
	}
	if remote := f.git(f.remote, "rev-parse", "refs/heads/"+f.target().BranchName); remote != recorder.pushedSHA {
		t.Fatalf("remote=%s recorded=%s", remote, recorder.pushedSHA)
	}
	assertReportBody(t, prs.body, f.head, "ID による照合")
}

func assertReportBody(t *testing.T, body, sha, method string) {
	t.Helper()
	for _, want := range []string{"task-1", "PR を作成する", "URL を記録する", "PR の作成とコメントを実装した。",
		"execution-1", "harness-1", "1.2.3", "v3", strings.Repeat("a", 40), sha,
		"Gate 2: ", "実行数: 6", "skip 数: 2", "確認方法: " + method, "Gate 3: pass",
		"ハーネスが書いたテストの成功だけを要件充足の証明としない"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestPublishContinuesOpenPRWithResultComment(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, steps := publishConfig(f)
	if _, err := publisher.Publish(context.Background(), f.target(), config); err != nil {
		t.Fatal(err)
	}
	// A later execution pushes an additional tested commit to the same branch.
	f.write(filepath.Join(f.worktree, "file.txt"), "next execution\n")
	f.git(f.worktree, "commit", "-qam", "next execution")
	f.head = f.git(f.worktree, "rev-parse", "HEAD")
	config.Report = executionReport(f.head)
	config.Report.ExecutionID = "execution-2"
	config.Report.VerifyRun.ExecutionID = "execution-2"
	config.Report.Gate2Findings = []model.Finding{{Rule: "count_only_check", Message: "件数による確認"}}
	*steps = nil
	publication, err := publisher.Publish(context.Background(), f.target(), config)
	if err != nil || publication.Created || publication.PR == nil || prs.created != 1 || prs.commented != 1 {
		t.Fatalf("publication=%+v created=%d commented=%d err=%v", publication, prs.created, prs.commented, err)
	}
	if recorder.pr != *publication.PR || recorder.created || !strings.Contains(prs.body, "execution-2") || !strings.Contains(prs.body, "確認方法: 件数による確認") || !strings.Contains(prs.body, f.head) {
		t.Fatalf("recorded=%+v body=%s", recorder, prs.body)
	}
	if !reflect.DeepEqual(*steps, []string{"record_push", "find", "comment", "record_pr"}) {
		t.Fatalf("steps=%v", *steps)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/"+f.target().BranchName); got != f.head {
		t.Fatalf("remote=%s", got)
	}
}

func TestPublishIncludesApprovedBlockContentsAndCountOnlyFinding(t *testing.T) {
	f := newPushFixture(t)
	config, prs, _, _ := publishConfig(f)
	config.Report.Gate2Result = "blocked"
	config.Report.Gate2Findings = []model.Finding{{Rule: "count_only_check", Message: "件数による確認"}, {Rule: "existing_test_not_run", Message: "skip 数が増加した"}}
	config.Report.ContinuedBlocks = []string{"gate2_missing_tests: skip 数の増加を本人が確認して続行した"}
	if _, err := publisher.Publish(context.Background(), f.target(), config); err != nil {
		t.Fatal(err)
	}
	assertReportBody(t, prs.body, f.head, "件数による確認")
	for _, want := range []string{"Gate 2: blocked", "existing_test_not_run: skip 数が増加した", "本人が続行を選んだ Block", config.Report.ContinuedBlocks[0]} {
		if !strings.Contains(prs.body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestPublishFailurePreservesPushAndDoesNotRepeatCreate(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, _ := publishConfig(f)
	prs.createErr = errors.New(secret)
	publication, err := publisher.Publish(context.Background(), f.target(), config)
	if publication.PushedSHA != f.head || recorder.pushedSHA != f.head || publication.PR != nil || publisher.ErrorClass(err) != model.ErrorPublishFailed || strings.Contains(err.Error(), secret) {
		t.Fatalf("publication=%+v recorder=%+v err=%v", publication, recorder, err)
	}
	prs.createErr = nil
	// Simulate losing the result acknowledgement after GitHub accepted creation.
	recorder.prErr = errors.New(secret)
	publication, err = publisher.Publish(context.Background(), f.target(), config)
	if !errors.Is(err, publisher.ErrPublication) || !publication.Created || publication.PR == nil || recorder.pr.Number != 0 {
		t.Fatalf("publication=%+v recorded=%+v err=%v", publication, recorder, err)
	}
	recorder.prErr = nil
	publication, err = publisher.Publish(context.Background(), f.target(), config)
	if err != nil || publication.Created || recorder.pr.Number != 17 || recorder.created {
		t.Fatalf("publication=%+v recorded=%+v err=%v", publication, recorder, err)
	}
	if prs.created != 2 || prs.commented != 1 {
		t.Fatalf("created=%d commented=%d", prs.created, prs.commented)
	}
}

func TestPublishRecordsCreatedPRAfterExecutionCancellation(t *testing.T) {
	f := newPushFixture(t)
	config, prs, recorder, _ := publishConfig(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prs.afterCreate = cancel
	publication, err := publisher.Publish(ctx, f.target(), config)
	if !errors.Is(err, context.Canceled) || publication.PR == nil || !publication.Created ||
		recorder.pr != *publication.PR || !recorder.created || recorder.pushedSHA != f.head {
		t.Fatalf("publication=%+v recorded=%+v err=%v", publication, recorder, err)
	}
}

func TestPublishStopsAtFailedBoundary(t *testing.T) {
	for _, failure := range []string{"push", "remote_sha", "record_push", "find", "multiple", "comment", "record_pr", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			f := newPushFixture(t)
			config, prs, recorder, steps := publishConfig(f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := publisher.ErrPullRequest
			switch failure {
			case "push":
				f.write(filepath.Join(f.worktree, "file.txt"), "dirty\n")
				want = publisher.ErrDirtyWorktree
			case "remote_sha":
				hook := filepath.Join(f.remote, "hooks", "post-receive")
				f.write(hook, "#!/bin/sh\ngit update-ref 'refs/heads/"+f.target().BranchName+"' '"+f.parent+"'\n")
				if err := os.Chmod(hook, 0700); err != nil {
					t.Fatal(err)
				}
				config.Push.RecordRemoteMismatch = func(context.Context, string, string) error { return nil }
				want = publisher.ErrRemoteSHA
			case "record_push":
				recorder.pushErr, want = errors.New(secret), publisher.ErrPublication
			case "find":
				prs.findErr = errors.New(secret)
			case "multiple":
				prs.findErr, want = publisher.ErrMultiplePRs, publisher.ErrMultiplePRs
			case "comment":
				prs.open = &publisher.PullRequest{Number: 8, URL: "https://github.com/owner/repo/pull/8"}
				prs.commentErr = errors.New(secret)
			case "record_pr":
				recorder.prErr, want = errors.New(secret), publisher.ErrPublication
			case "cancelled":
				recorder.afterPush, want = cancel, context.Canceled
			}
			publication, err := publisher.Publish(ctx, f.target(), config)
			if !errors.Is(err, want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("err=%v want=%v", err, want)
			}
			if failure == "push" || failure == "remote_sha" {
				if publication.PushedSHA != "" || len(*steps) != 0 {
					t.Fatalf("publication=%+v steps=%v", publication, *steps)
				}
			} else if publication.PushedSHA != f.head {
				t.Fatalf("lost confirmed push: %+v", publication)
			}
			if failure != "record_pr" && prs.created != 0 {
				t.Fatalf("unexpected create at %s", failure)
			}
			if failure == "comment" && (recorder.pr.Number != 8 || recorder.created) {
				t.Fatalf("lost existing PR: %+v", recorder)
			}
		})
	}
}

func TestPublishRejectsIncompleteReportBeforePush(t *testing.T) {
	f := newPushFixture(t)
	for _, change := range []func(*publisher.PublishConfig){
		func(c *publisher.PublishConfig) { c.BaseBranch = "" },
		func(c *publisher.PublishConfig) { c.PullRequests = nil },
		func(c *publisher.PublishConfig) { c.Recorder = nil },
		func(c *publisher.PublishConfig) { c.ReportContext = nil },
		func(c *publisher.PublishConfig) { c.Report.HarnessVersion = "" },
		func(c *publisher.PublishConfig) { c.Report.VerifyRun.TargetSHA = f.parent },
		func(c *publisher.PublishConfig) { c.Report.VerifyRun.Total = nil },
		func(c *publisher.PublishConfig) { c.Report.Gate3Result = "blocked" },
		func(c *publisher.PublishConfig) { c.Report.Gate2Result = "blocked" },
	} {
		config, _, _, steps := publishConfig(f)
		change(&config)
		publication, err := publisher.Publish(context.Background(), f.target(), config)
		if err == nil || publication.PushedSHA != "" || len(*steps) != 0 {
			t.Fatalf("publication=%+v steps=%v err=%v", publication, *steps, err)
		}
	}
	if refs := f.git(f.remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Fatalf("unexpected refs: %s", refs)
	}
}
