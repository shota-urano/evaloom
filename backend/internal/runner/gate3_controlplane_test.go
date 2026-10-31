package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type gate3Fixture struct {
	*baselineHTTPFixture
	copies           []model.CopiedFile
	calls, published int
}

func (f *gate3Fixture) copyFiles(t *testing.T) {
	t.Helper()
	for _, path := range []string{"local.conf", "another config"} {
		gate2Write(t, filepath.Join(f.path, path), "copied")
		f.copies = append(f.copies, model.CopiedFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("copied")))})
	}
	body, err := json.Marshal(f.copies)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE worktrees SET copied_files=?`, string(body)); err != nil {
		t.Fatal(err)
	}
}

func newGate3Fixture(t *testing.T) *gate3Fixture {
	t.Helper()
	f := &gate3Fixture{baselineHTTPFixture: newBaselineHTTPFixture(t)}
	gate2Write(t, filepath.Join(f.path, ".gitignore"), "report.xml\ncalls\n")
	gate2Write(t, filepath.Join(f.path, "source"), "original")
	gate2Git(t, f.path, "add", ".")
	gate2Git(t, f.path, "commit", "--quiet", "-m", "fixture source and output policy")
	f.sha = gate2Git(t, f.path, "rev-parse", "HEAD")
	if _, err := f.db.Exec(`UPDATE worktrees SET base_sha=?`, f.sha); err != nil {
		t.Fatal(err)
	}
	return f
}

type gate3TestProcess struct {
	*baselineCommandProcess
	after func()
}

func (p *gate3TestProcess) Wait() error {
	err := p.baselineCommandProcess.Wait()
	if err == nil && p.after != nil {
		p.after()
	}
	return err
}

func (f *gate3Fixture) executor(t *testing.T, p *Runner, afterTest func(), prepare, resume StageFunc) *Executor {
	t.Helper()
	baseline := BaselineConfig{Sandbox: baselineRuntimeFunc(func(_ context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
		f.calls++
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = spec.Worktree, []string{"PATH=/usr/bin:/bin"}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		process := &gate3TestProcess{baselineCommandProcess: &baselineCommandProcess{cmd}}
		if f.calls == 2 {
			process.after = afterTest
		}
		return process, nil
	}), Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 20, Time: time.Minute}, Shell: "/bin/sh", JUnitFormat: "junit"}
	handlers := stubStageHandlers(func(id StageID, ctx context.Context, e *Execution) error {
		switch id {
		case StagePrepareWorktree:
			if prepare != nil {
				if err := prepare(ctx, e); err != nil {
					return err
				}
			}
			e.WorktreeID, e.WorktreePath, e.BaseSHA, e.CopiedFiles = f.worktreeID, f.path, f.sha, f.copies
		case StageHarness:
			gate2Git(t, f.path, "commit", "--quiet", "--allow-empty", "-m", "fixture harness")
		case StagePublish:
			var passes, runs int
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM gate_results WHERE execution_id=? AND gate=3 AND result='pass'),
				(SELECT count(*) FROM test_runs WHERE execution_id=? AND kind='verify')`, e.Claim.Execution.ID, e.Claim.Execution.ID).Scan(&passes, &runs); err != nil || passes == 0 || runs != 1 {
				t.Fatalf("publish before persisted Gate 3: passes=%d runs=%d err=%v", passes, runs, err)
			}
			f.published++
		}
		return nil
	})
	control := testExecutionControl()
	control.Reporter, control.CheckCancellation = p, func(context.Context) error { return nil }
	executor, err := p.NewVerificationExecutor(handlers, control, Gate2Config{BaselineConfig: baseline, Rules: gate2FixtureRules()}, Gate3Config{MismatchRule: "fixture_mismatch", PrepareResume: resume})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestGate3HTTPAcceptance(t *testing.T) {
	for _, name := range []string{"Clean", "CommitAfterTests", "Untracked", "Modified", "Staged", "UnchangedCopies", "ChangedCopy", "ReportNotExcluded", "ZeroExecuted"} {
		t.Run(name, func(t *testing.T) {
			f := newGate3Fixture(t)
			if name == "UnchangedCopies" || name == "ChangedCopy" {
				f.copyFiles(t)
			}
			if name == "ReportNotExcluded" {
				gate2Write(t, filepath.Join(f.path, ".gitignore"), "calls\n")
				gate2Git(t, f.path, "add", ".gitignore")
				gate2Git(t, f.path, "commit", "--quiet", "-m", "fixture report policy")
				f.sha = gate2Git(t, f.path, "rev-parse", "HEAD")
				if _, err := f.db.Exec(`UPDATE worktrees SET base_sha=?`, f.sha); err != nil {
					t.Fatal(err)
				}
			}
			if name == "ZeroExecuted" {
				f.command = `printf '%s' '<testsuite tests="0"/>' > report.xml`
				if _, err := f.db.Exec(`UPDATE projects SET test_command=?`, f.command); err != nil {
					t.Fatal(err)
				}
			}
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			claim, err := p.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v %v", claim, err)
			}
			after := func() {
				switch name {
				case "CommitAfterTests":
					gate2Git(t, f.path, "commit", "--quiet", "--allow-empty", "-m", "after test")
				case "Untracked":
					gate2Write(t, filepath.Join(f.path, "unexpected"), "x")
				case "Modified", "Staged":
					gate2Write(t, filepath.Join(f.path, "source"), "changed")
					if name == "Staged" {
						gate2Git(t, f.path, "add", "source")
					}
				case "ChangedCopy":
					gate2Write(t, filepath.Join(f.path, "local.conf"), "changed")
				}
			}
			err = f.executor(t, p, after, nil, nil).Execute(ctx, *claim)
			pass := name == "Clean" || name == "UnchangedCopies"
			if pass && err != nil || !pass && !errors.Is(err, ErrGate3Stopped) || f.calls != 2 || (f.published == 1) != pass {
				t.Fatalf("err=%v calls=%d published=%d", err, f.calls, f.published)
			}
			var status, result, target, tested string
			if err := f.db.QueryRow(`SELECT e.status,g.result,g.target_sha,e.verified_sha FROM executions e JOIN gate_results g ON g.execution_id=e.id AND g.gate=3 WHERE e.id=?`, claim.Execution.ID).Scan(&status, &result, &target, &tested); err != nil {
				t.Fatal(err)
			}
			if pass {
				if status != "succeeded" || result != "pass" || target != tested {
					t.Fatalf("status=%s result=%s", status, result)
				}
				return
			}
			var cause, blockedHead string
			if err := f.db.QueryRow(`SELECT cause,head_sha FROM execution_blocks WHERE execution_id=? AND gate_result_id IN (SELECT id FROM gate_results WHERE gate=3)`, claim.Execution.ID).Scan(&cause, &blockedHead); err != nil {
				t.Fatal(err)
			}
			if status != "blocked" || result != "blocked" || cause != "gate3_mismatch" || blockedHead != target || target != gate2Git(t, f.path, "rev-parse", "HEAD") || name == "CommitAfterTests" && target == tested {
				t.Fatalf("status=%s result=%s cause=%s head=%s tested=%s", status, result, cause, blockedHead, tested)
			}
			var gates, blocks, notifications int
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM execution_events WHERE type='gate_result'),
				(SELECT count(*) FROM execution_events WHERE type='blocked'),(SELECT count(*) FROM notification_requests WHERE status='blocked')`).Scan(&gates, &blocks, &notifications); err != nil || gates != 2 || blocks != 1 || notifications != 1 {
				t.Fatalf("events=%d blocks=%d notifications=%d err=%v", gates, blocks, notifications, err)
			}
		})
	}
}

func (f *gate3Fixture) queueResume(t *testing.T) {
	t.Helper()
	// Owner continuation is covered by API tests. This fixture advances only
	// the queue, keeping all measured SHAs, copies and test_runs unchanged.
	if _, err := f.db.Exec(`UPDATE executions SET status='pending',resume_point='gate3'; UPDATE tasks SET status='queued'`); err != nil {
		t.Fatal(err)
	}
}

func TestGate3HTTPResumeRechecksSavedVerification(t *testing.T) {
	for _, name := range []string{"StillDirty", "Repaired", "Cleaned", "Copies", "MissingVerifiedSHA", "MissingRun"} {
		t.Run(name, func(t *testing.T) {
			f := newGate3Fixture(t)
			if name == "Copies" {
				f.copyFiles(t)
			}
			repo := f.path
			if name == "Cleaned" {
				f.path = filepath.Join(t.TempDir(), "restored")
				gate2Git(t, repo, "worktree", "add", "-b", "task", f.path, f.sha)
				if _, err := f.db.Exec(`UPDATE worktrees SET path=?`, f.path); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			first, err := p.claim(ctx)
			if err != nil || first == nil {
				t.Fatalf("claim=%+v %v", first, err)
			}
			err = f.executor(t, p, func() { gate2Write(t, filepath.Join(f.path, "unexpected"), "x") }, nil, nil).Execute(ctx, *first)
			if !errors.Is(err, ErrGate3Stopped) {
				t.Fatal(err)
			}
			if name != "StillDirty" {
				if err := os.Remove(filepath.Join(f.path, "unexpected")); err != nil {
					t.Fatal(err)
				}
			}
			if name == "Cleaned" {
				gate2Git(t, repo, "worktree", "remove", f.path)
				if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
					t.Fatal(err)
				}
			}
			if name == "MissingVerifiedSHA" {
				if _, err := f.db.Exec(`UPDATE executions SET verified_sha=NULL`); err != nil {
					t.Fatal(err)
				}
			}
			if name == "MissingRun" {
				if _, err := f.db.Exec(`DELETE FROM test_runs WHERE kind='verify'`); err != nil {
					t.Fatal(err)
				}
			}
			f.queueResume(t)
			p, err = New(f.config) // No local Execution or measurement survives.
			if err != nil {
				t.Fatal(err)
			}
			claim, err := p.claim(ctx)
			if err != nil || claim == nil || claim.Worktree == nil || claim.Execution.ID != first.Execution.ID {
				t.Fatalf("resume claim=%+v %v", claim, err)
			}
			if name != "MissingRun" && (claim.Execution.VerifyRun == nil || claim.Execution.VerifyRun.ExecutionID != first.Execution.ID) {
				t.Fatal("claim lost saved verification")
			}
			if name == "Copies" && len(claim.Worktree.CopiedFiles) != 2 {
				t.Fatal("claim lost copied file hashes")
			}
			restored := false
			var prepare StageFunc
			if name == "Cleaned" {
				prepare = func(_ context.Context, e *Execution) error {
					gate2Git(t, repo, "worktree", "add", f.path, e.Claim.Worktree.BranchName)
					restored = true
					_, err := f.db.Exec(`UPDATE worktrees SET status='active',cleaned_at=NULL`)
					return err
				}
			}
			err = f.executor(t, p, nil, nil, prepare).Execute(ctx, *claim)
			pass := name == "Repaired" || name == "Cleaned" || name == "Copies"
			if pass && err != nil || !pass && !errors.Is(err, ErrGate3Stopped) || f.calls != 2 || (f.published == 1) != pass || name == "Cleaned" && !restored {
				t.Fatalf("resume=%v tests=%d published=%d restored=%v", err, f.calls, f.published, restored)
			}
			var checks int
			if err := f.db.QueryRow(`SELECT count(*) FROM gate_results WHERE gate=3`).Scan(&checks); err != nil || checks != 2 {
				t.Fatalf("checks=%d %v", checks, err)
			}
		})
	}
}

func TestGate3HTTPContinueUsesNewVerificationAfterCleanup(t *testing.T) {
	for _, cleaned := range []bool{false, true} {
		t.Run(fmt.Sprint(cleaned), func(t *testing.T) {
			f := newGate3Fixture(t)
			repo := f.path
			if cleaned {
				f.path = filepath.Join(t.TempDir(), "continued")
				gate2Git(t, repo, "worktree", "add", "-b", "task", f.path, f.sha)
				if _, err := f.db.Exec(`UPDATE worktrees SET path=?`, f.path); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			first, err := p.claim(ctx)
			if err != nil || first == nil {
				t.Fatalf("claim=%+v %v", first, err)
			}
			if err := f.executor(t, p, nil, nil, nil).Execute(ctx, *first); err != nil {
				t.Fatal(err)
			}
			if cleaned {
				gate2Git(t, repo, "worktree", "remove", f.path)
				if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
					t.Fatal(err)
				}
			}
			id := ulid.Make().String()
			if _, err := f.db.Exec(`INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES (?,'task-001',2,'continue','pending','now'); UPDATE tasks SET status='queued'`, id); err != nil {
				t.Fatal(err)
			}
			p, err = New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := p.claim(ctx)
			if err != nil || claim == nil || claim.Execution.VerifyRun != nil || claim.Execution.VerifiedSHA != "" || claim.Worktree == nil || claim.Worktree.Baseline == nil {
				t.Fatalf("continue claim=%+v %v", claim, err)
			}
			var prepare StageFunc
			if cleaned {
				prepare = func(context.Context, *Execution) error {
					gate2Git(t, repo, "worktree", "add", f.path, "task")
					_, err := f.db.Exec(`UPDATE worktrees SET status='active',cleaned_at=NULL`)
					return err
				}
			}
			if err := f.executor(t, p, nil, prepare, nil).Execute(ctx, *claim); err != nil {
				t.Fatal(err)
			}
			var baseline, verify int
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE kind='baseline'),(SELECT count(*) FROM test_runs WHERE kind='verify')`).Scan(&baseline, &verify); err != nil || baseline != 1 || verify != 2 || f.calls != 3 || f.published != 2 {
				t.Fatalf("baseline=%d verify=%d calls=%d published=%d %v", baseline, verify, f.calls, f.published, err)
			}
		})
	}
}
