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
	executor, err := p.NewVerificationExecutor(handlers, control, Gate2Config{BaselineConfig: baseline}, Gate3Config{PrepareResume: resume})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestGate3HTTPAcceptance(t *testing.T) {
	for _, name := range []string{"Clean", "CommitAfterTests", "Untracked", "Modified", "Staged", "UnchangedCopies", "ChangedCopy", "ReportNotExcluded", "ZeroExecuted", "CorruptIndex", "CorruptHEAD"} {
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
				case "CorruptHEAD":
					gate2Write(t, filepath.Join(f.path, ".git", "HEAD"), "ref: refs/heads/missing\n")
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
			wantHead := tested
			if name == "CorruptHEAD" {
				if _, err := exec.Command("git", "-C", f.path, "rev-parse", "--verify", "HEAD^{commit}").Output(); err == nil {
					t.Fatal("fixture HEAD must be unreadable")
				}
				if tested == f.sha {
					t.Fatal("expected the last observed harness commit, not the base commit")
				}
			} else {
				wantHead = gate2Git(t, f.path, "rev-parse", "HEAD")
			}
			if status != "blocked" || result != "blocked" || cause != "gate3_mismatch" || blockedHead != target || target != wantHead || name == "CommitAfterTests" && target == tested {
				t.Fatalf("status=%s result=%s cause=%s head=%s tested=%s", status, result, cause, blockedHead, tested)
			}
			var encoded string
			if err := f.db.QueryRow(`SELECT findings FROM gate_results WHERE execution_id=? AND gate=3`, claim.Execution.ID).Scan(&encoded); err != nil {
				t.Fatal(err)
			}
			var findings []model.Finding
			if err := json.Unmarshal([]byte(encoded), &findings); err != nil || len(findings) != 1 || findings[0].Rule != "changed_after_test" {
				t.Fatalf("persisted Gate 3 rule=%s err=%v", encoded, err)
			}
			if name == "CorruptIndex" || name == "CorruptHEAD" {
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
				if err := json.Unmarshal([]byte(findings), &saved); err != nil || len(saved) != 1 || saved[0].Rule != "changed_after_test" || saved[0].Message == "" {
					t.Fatalf("inspection failure reason=%s err=%v", findings, err)
				}
				if name == "CorruptHEAD" && saved[0].Message != "現在のHEADを取得できない。記録したSHAはハーネス終了時に確認したSHAであり、現在のHEADは未確認" {
					t.Fatalf("last observed SHA must be distinguished from the current HEAD: %s", findings)
				}
				var taskStatus, worktreeStatus, harnessSHA string
				if err := f.db.QueryRow(`SELECT t.status,w.status,e.harness_head_sha FROM executions e
					JOIN tasks t ON t.id=e.task_id JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, claim.Execution.ID).Scan(&taskStatus, &worktreeStatus, &harnessSHA); err != nil ||
					taskStatus != "blocked" || worktreeStatus != "retained" || harnessSHA != tested {
					t.Fatalf("task=%s worktree=%s harness=%s tested=%s err=%v", taskStatus, worktreeStatus, harnessSHA, tested, err)
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
	for _, tc := range []struct {
		name, command string
		timedOut      bool
	}{
		{"Cancelled", "status", false}, {"TimedOut", "status", true},
		{"HEADCancelled", "rev-parse", false}, {"HEADTimedOut", "rev-parse", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGate3Fixture(t)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			started := filepath.Join(bin, "inspection-started")
			armed := filepath.Join(bin, "gate3-started")
			// Hold only the selected Git command during Gate 3. Gate 2, HTTP
			// reporting and persistence use their real implementations.
			script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "$EVALOOM_TEST_INSPECTION_COMMAND" ] && [ -f "$EVALOOM_TEST_GATE3_STARTED" ]; then
    printf started > "$EVALOOM_TEST_INSPECTION_STARTED"
    exec /bin/sleep 30
  fi
done
exec "$EVALOOM_TEST_REAL_GIT" "$@"
`
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("EVALOOM_TEST_REAL_GIT", realGit)
			t.Setenv("EVALOOM_TEST_INSPECTION_COMMAND", tc.command)
			t.Setenv("EVALOOM_TEST_INSPECTION_STARTED", started)
			t.Setenv("EVALOOM_TEST_GATE3_STARTED", armed)
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
					if err := os.WriteFile(armed, []byte("started"), 0600); err != nil {
						t.Fatal(err)
					}
					if tc.timedOut {
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
			if tc.timedOut {
				wantErr, wantStatus, wantClass = context.DeadlineExceeded, "timed_out", "timeout"
			}
			err = executor.Execute(ctx, *claim)
			if !errors.Is(err, wantErr) || errors.Is(err, ErrGate3Stopped) || f.calls != 2 || f.published != 0 {
				t.Fatalf("err=%v tests=%d published=%d", err, f.calls, f.published)
			}
			if _, err := os.Stat(started); err != nil {
				t.Fatalf("Git %s was not reached: %v", tc.command, err)
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
	f.continueBlocked(t)
}

func TestGate3HTTPResumeRechecksSavedVerification(t *testing.T) {
	for _, name := range []string{"StillDirty", "Repaired", "Cleaned", "Copies", "MissingVerifiedSHA", "MissingRun", "CorruptIndex", "RepairedIndex", "CorruptHEAD", "RepairedHEAD", "MissingHarnessHeadSHA"} {
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
			indexFailure := name == "CorruptIndex" || name == "RepairedIndex"
			headFailure := name == "CorruptHEAD" || name == "RepairedHEAD" || name == "MissingHarnessHeadSHA"
			var originalGitFile []byte
			gitPath, invalid := filepath.Join(f.path, ".git", "index"), "invalid index"
			if headFailure {
				gitPath, invalid = filepath.Join(f.path, ".git", "HEAD"), "ref: refs/heads/missing\n"
			}
			err = f.executor(t, p, func() {
				if indexFailure || headFailure {
					var err error
					originalGitFile, err = os.ReadFile(gitPath)
					if err != nil {
						t.Error(err)
						return
					}
					gate2Write(t, gitPath, invalid)
					return
				}
				gate2Write(t, filepath.Join(f.path, "unexpected"), "x")
			}, nil, nil).Execute(ctx, *first)
			if !errors.Is(err, ErrGate3Stopped) {
				t.Fatal(err)
			}
			if name == "RepairedIndex" || name == "RepairedHEAD" {
				gate2Write(t, gitPath, string(originalGitFile))
			} else if name != "StillDirty" && !indexFailure && !headFailure {
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
			if name == "MissingHarnessHeadSHA" {
				if _, err := f.db.Exec(`UPDATE executions SET harness_head_sha=NULL`); err != nil {
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
			pass := name == "Repaired" || name == "Cleaned" || name == "Copies" || name == "RepairedIndex" || name == "RepairedHEAD"
			if pass && err != nil || !pass && !errors.Is(err, ErrGate3Stopped) || f.calls != 2 || (f.published == 1) != pass || name == "Cleaned" && !restored {
				t.Fatalf("resume=%v tests=%d published=%d restored=%v", err, f.calls, f.published, restored)
			}
			var checks int
			if err := f.db.QueryRow(`SELECT count(*) FROM gate_results WHERE gate=3`).Scan(&checks); err != nil || checks != 2 {
				t.Fatalf("checks=%d %v", checks, err)
			}
			if headFailure && !pass {
				var status, target, blockedHead, findings string
				if err := f.db.QueryRow(`SELECT e.status,g.target_sha,b.head_sha,g.findings FROM executions e
					JOIN gate_results g ON g.execution_id=e.id JOIN execution_blocks b ON b.gate_result_id=g.id
					WHERE e.id=? AND g.gate=3 ORDER BY g.rowid DESC LIMIT 1`, first.Execution.ID).Scan(&status, &target, &blockedHead, &findings); err != nil {
					t.Fatal(err)
				}
				wantSHA := claim.Execution.HarnessHeadSHA
				wantReason := "現在のHEADを取得できない。記録したSHAはハーネス終了時に確認したSHAであり、現在のHEADは未確認"
				if name == "MissingHarnessHeadSHA" {
					wantSHA = f.sha
					wantReason = "現在のHEADを取得できない。記録したSHAは開始点のSHAであり、現在のHEADは未確認"
				}
				var saved []model.Finding
				if err := json.Unmarshal([]byte(findings), &saved); err != nil || len(saved) != 1 || saved[0].Message != wantReason ||
					status != "blocked" || target != wantSHA || blockedHead != wantSHA {
					t.Fatalf("resumed HEAD failure: status=%s target=%s block=%s want=%s findings=%s err=%v", status, target, blockedHead, wantSHA, findings, err)
				}
			}
		})
	}
}

func TestGate3HTTPContinueUsesNewVerificationAfterCleanup(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		cleaned, corruptIndex, corruptHEAD bool
	}{
		{"Retained", false, false, false}, {"Cleaned", true, false, false},
		{"CorruptIndex", false, true, false}, {"CleanedCorruptIndex", true, true, false},
		{"CorruptHEAD", false, false, true}, {"CleanedCorruptHEAD", true, false, true},
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
			var lastHead string
			inspectionFailure := tc.corruptIndex || tc.corruptHEAD
			if inspectionFailure {
				// Resolve Git metadata for retained and restored linked worktrees.
				afterTest = func() {
					lastHead = gate2Git(t, f.path, "rev-parse", "HEAD")
					name, invalid := "index", "invalid index"
					if tc.corruptHEAD {
						name, invalid = "HEAD", "ref: refs/heads/missing\n"
					}
					gitPath := gate2Git(t, f.path, "rev-parse", "--git-path", name)
					if !filepath.IsAbs(gitPath) {
						gitPath = filepath.Join(f.path, gitPath)
					}
					gate2Write(t, gitPath, invalid)
				}
			}
			err = f.executor(t, p, afterTest, prepare, nil).Execute(ctx, *claim)
			if !inspectionFailure && err != nil || inspectionFailure && !errors.Is(err, ErrGate3Stopped) {
				t.Fatalf("continued execution: %v", err)
			}
			var baseline, verify int
			wantPublished := 2
			if inspectionFailure {
				wantPublished = 1
				var status, cause, head, tested string
				if err := f.db.QueryRow(`SELECT e.status,b.cause,b.head_sha,e.verified_sha FROM executions e
					JOIN execution_blocks b ON b.execution_id=e.id WHERE e.id=?`, id).Scan(&status, &cause, &head, &tested); err != nil ||
					status != "blocked" || cause != "gate3_mismatch" || head != tested || head != lastHead {
					t.Fatalf("continue: status=%s cause=%s head=%s tested=%s err=%v", status, cause, head, tested, err)
				}
			}
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE kind='baseline'),(SELECT count(*) FROM test_runs WHERE kind='verify')`).Scan(&baseline, &verify); err != nil || baseline != 1 || verify != 2 || f.calls != 3 || f.published != wantPublished {
				t.Fatalf("baseline=%d verify=%d calls=%d published=%d %v", baseline, verify, f.calls, f.published, err)
			}
		})
	}
}
