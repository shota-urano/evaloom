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
		if f.calls > 1 {
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
	for _, name := range []string{"Clean", "CommitAfterTests", "Untracked", "Modified", "Staged", "UnchangedCopies", "ChangedCopy", "ReportNotExcluded", "ZeroExecuted", "CorruptIndex"} {
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
				case "CorruptIndex":
					gate2Write(t, filepath.Join(f.path, ".git", "index"), "invalid index")
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
			if name == "CorruptIndex" {
				// Gate 2 has not reported its successful measurement before Gate 3.
				// Inspection failures must flush it together with the Block.
				var runSHA, gate2SHA, findings string
				var total, passed, skipped int
				if err := f.db.QueryRow(`SELECT r.target_sha,r.total,r.passed,r.skipped,g.target_sha
					FROM test_runs r JOIN gate_results g ON g.execution_id=r.execution_id AND g.gate=2 AND g.result='pass'
					WHERE r.execution_id=? AND r.kind='verify'`, claim.Execution.ID).Scan(&runSHA, &total, &passed, &skipped, &gate2SHA); err != nil ||
					runSHA != tested || gate2SHA != tested || tested != target || total != 3 || passed != 2 || skipped != 1 {
					t.Fatalf("saved Gate 2: run=%s gate=%s tested=%s head=%s total=%d passed=%d skipped=%d err=%v", runSHA, gate2SHA, tested, target, total, passed, skipped, err)
				}
				if err := f.db.QueryRow(`SELECT findings FROM gate_results WHERE execution_id=? AND gate=3`, claim.Execution.ID).Scan(&findings); err != nil {
					t.Fatal(err)
				}
				var saved []model.Finding
				if err := json.Unmarshal([]byte(findings), &saved); err != nil || len(saved) != 1 || saved[0].Rule != "fixture_mismatch" || saved[0].Message == "" {
					t.Fatalf("inspection failure reason=%s err=%v", findings, err)
				}
			}
			var gates, blocks, notifications int
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM execution_events WHERE type='gate_result'),
				(SELECT count(*) FROM execution_events WHERE type='blocked'),(SELECT count(*) FROM notification_requests WHERE status='blocked')`).Scan(&gates, &blocks, &notifications); err != nil || gates != 2 || blocks != 1 || notifications != 1 {
				t.Fatalf("events=%d blocks=%d notifications=%d err=%v", gates, blocks, notifications, err)
			}
		})
	}
}

func TestGate3HTTPInspectionCancellationAndTimeout(t *testing.T) {
	for _, name := range []string{"Cancelled", "TimedOut"} {
		t.Run(name, func(t *testing.T) {
			f := newGate3Fixture(t)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			started := filepath.Join(bin, "status-started")
			// Only the Git status subprocess is held. HEAD, configuration reads,
			// Gate 2, HTTP reporting and persistence use their real implementations.
			script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = status ]; then
    printf started > "$EVALOOM_TEST_STATUS_STARTED"
    exec /bin/sleep 30
  fi
done
exec "$EVALOOM_TEST_REAL_GIT" "$@"
`
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("EVALOOM_TEST_REAL_GIT", realGit)
			t.Setenv("EVALOOM_TEST_STATUS_STARTED", started)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := p.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			executor := f.executor(t, p, nil, nil, nil)
			for i := range executor.stages {
				if executor.stages[i].ID != StageGate3 {
					continue
				}
				run := executor.stages[i].run
				executor.stages[i].run = func(ctx context.Context, e *Execution) error {
					if name == "TimedOut" {
						ctx, stop := context.WithTimeout(ctx, 2*time.Second)
						defer stop()
						return run(ctx, e)
					}
					ctx, stop := context.WithCancel(ctx)
					done := make(chan struct{})
					defer func() { stop(); <-done }()
					go func() {
						defer close(done)
						ticker := time.NewTicker(time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-ticker.C:
								if _, err := os.Stat(started); err == nil {
									stop()
									return
								}
							}
						}
					}()
					return run(ctx, e)
				}
			}
			wantErr, wantStatus, wantClass := context.Canceled, "cancelled", "cancelled"
			if name == "TimedOut" {
				wantErr, wantStatus, wantClass = context.DeadlineExceeded, "timed_out", "timeout"
			}
			err = executor.Execute(ctx, *claim)
			if !errors.Is(err, wantErr) || errors.Is(err, ErrGate3Stopped) || f.calls != 2 || f.published != 0 {
				t.Fatalf("err=%v tests=%d published=%d", err, f.calls, f.published)
			}
			if _, err := os.Stat(started); err != nil {
				t.Fatalf("Git status was not reached: %v", err)
			}
			var status, class string
			var runs, gate2, gate3, blocks, notifications int
			if err := f.db.QueryRow(`SELECT e.status,e.error_class,
				(SELECT count(*) FROM test_runs WHERE execution_id=e.id AND kind='verify' AND target_sha=e.verified_sha AND passed=2),
				(SELECT count(*) FROM gate_results WHERE execution_id=e.id AND gate=2 AND result='pass'),
				(SELECT count(*) FROM gate_results WHERE execution_id=e.id AND gate=3),
				(SELECT count(*) FROM execution_blocks WHERE execution_id=e.id),
				(SELECT count(*) FROM notification_requests WHERE execution_id=e.id AND status='blocked')
				FROM executions e WHERE e.id=?`, claim.Execution.ID).Scan(&status, &class, &runs, &gate2, &gate3, &blocks, &notifications); err != nil ||
				status != wantStatus || class != wantClass || runs != 1 || gate2 != 1 || gate3 != 0 || blocks != 0 || notifications != 0 {
				t.Fatalf("status=%s class=%s runs=%d gates=%d/%d blocks=%d notifications=%d err=%v", status, class, runs, gate2, gate3, blocks, notifications, err)
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
	for _, name := range []string{"StillDirty", "Repaired", "Cleaned", "Copies", "MissingVerifiedSHA", "MissingRun", "CorruptIndex", "RepairedIndex"} {
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
			var originalIndex []byte
			indexPath := filepath.Join(f.path, ".git", "index")
			err = f.executor(t, p, func() {
				if name == "CorruptIndex" || name == "RepairedIndex" {
					var err error
					originalIndex, err = os.ReadFile(indexPath)
					if err != nil {
						t.Error(err)
						return
					}
					gate2Write(t, indexPath, "invalid index")
					return
				}
				gate2Write(t, filepath.Join(f.path, "unexpected"), "x")
			}, nil, nil).Execute(ctx, *first)
			if !errors.Is(err, ErrGate3Stopped) {
				t.Fatal(err)
			}
			if name == "RepairedIndex" {
				gate2Write(t, indexPath, string(originalIndex))
			} else if name != "StillDirty" && name != "CorruptIndex" {
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
			pass := name == "Repaired" || name == "Cleaned" || name == "Copies" || name == "RepairedIndex"
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
	for _, tc := range []struct {
		name                  string
		cleaned, corruptIndex bool
	}{
		{"Retained", false, false}, {"Cleaned", true, false},
		{"CorruptIndex", false, true}, {"CleanedCorruptIndex", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleaned := tc.cleaned
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
			var afterTest func()
			if tc.corruptIndex {
				// Resolve the actual index for retained and restored linked worktrees.
				afterTest = func() {
					indexPath := gate2Git(t, f.path, "rev-parse", "--git-path", "index")
					if !filepath.IsAbs(indexPath) {
						indexPath = filepath.Join(f.path, indexPath)
					}
					gate2Write(t, indexPath, "invalid index")
				}
			}
			err = f.executor(t, p, afterTest, prepare, nil).Execute(ctx, *claim)
			if !tc.corruptIndex && err != nil || tc.corruptIndex && !errors.Is(err, ErrGate3Stopped) {
				t.Fatalf("continued execution: %v", err)
			}
			var baseline, verify int
			wantPublished := 2
			if tc.corruptIndex {
				wantPublished = 1
				var status, cause, head, tested string
				if err := f.db.QueryRow(`SELECT e.status,b.cause,b.head_sha,e.verified_sha FROM executions e
					JOIN execution_blocks b ON b.execution_id=e.id WHERE e.id=?`, id).Scan(&status, &cause, &head, &tested); err != nil ||
					status != "blocked" || cause != "gate3_mismatch" || head != tested || head != gate2Git(t, f.path, "rev-parse", "HEAD") {
					t.Fatalf("continue: status=%s cause=%s head=%s tested=%s err=%v", status, cause, head, tested, err)
				}
			}
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE kind='baseline'),(SELECT count(*) FROM test_runs WHERE kind='verify')`).Scan(&baseline, &verify); err != nil || baseline != 1 || verify != 2 || f.calls != 3 || f.published != wantPublished {
				t.Fatalf("baseline=%d verify=%d calls=%d published=%d %v", baseline, verify, f.calls, f.published, err)
			}
		})
	}
}
