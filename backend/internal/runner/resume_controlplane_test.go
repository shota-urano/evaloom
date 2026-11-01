package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Exercise the owner decision and Runner reclaim over their real HTTP routes.
// SQL reads assert evidence; they do not simulate continuation state changes.
func (f *baselineHTTPFixture) continueBlocked(t *testing.T) string {
	t.Helper()
	var input struct {
		BlockID  string `json:"blockId"`
		Decision string `json:"decision"`
		HeadSHA  string `json:"headSha"`
	}
	if err := f.db.QueryRow(`SELECT id,head_sha FROM execution_blocks WHERE execution_id=? AND decision IS NULL ORDER BY rowid DESC LIMIT 1`, logExecution).Scan(&input.BlockID, &input.HeadSHA); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("GET", f.server.URL+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(f.cookie)
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var session struct{ CSRFToken string }
	err = json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || session.CSRFToken == "" {
		t.Fatalf("session status=%d err=%v", response.StatusCode, err)
	}
	input.Decision = "continue"
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest("POST", f.server.URL+"/api/tasks/task-001/resume", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(f.cookie)
	request.Header.Set("Origin", f.server.URL)
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	request.Header.Set("Content-Type", "application/json")
	response, err = f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("continue status=%d body=%s err=%v", response.StatusCode, body, err)
	}
	return input.HeadSHA
}

func TestBlockedResumeHTTPGate2StartsAtGate3(t *testing.T) {
	for _, name := range []string{"MissingTests", "Cleaned", "UnverifiableRun", "ZeroExecuted"} {
		t.Run(name, func(t *testing.T) {
			f := newGate3Fixture(t)
			baseline, verify := `<testsuite tests="3" skipped="1"/>`, `<testsuite tests="2" skipped="1"/>`
			if name == "UnverifiableRun" {
				verify = ""
			}
			if name == "ZeroExecuted" {
				verify = `<testsuite tests="1" skipped="1"/>`
			}
			f.command = `if test -f calls; then report='` + verify + `'; else report='` + baseline + `'; fi; if test -n "$report"; then printf '%s' "$report" > report.xml; fi; printf x >> calls`
			if _, err := f.db.Exec(`UPDATE projects SET test_command=?`, f.command); err != nil {
				t.Fatal(err)
			}
			repo := f.path
			if name == "Cleaned" {
				f.path = filepath.Join(t.TempDir(), "resumed")
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
				t.Fatalf("claim=%+v err=%v", first, err)
			}
			if err := f.executor(t, p, nil, nil, nil).Execute(ctx, *first); !errors.Is(err, ErrGate2Stopped) || f.published != 0 {
				t.Fatalf("initial execution=%v published=%d", err, f.published)
			}
			var cause string
			if err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE execution_id=?`, first.Execution.ID).Scan(&cause); err != nil {
				t.Fatal(err)
			}
			wantCause := "gate2_missing_tests"
			if name == "UnverifiableRun" {
				wantCause = "gate2_unverifiable"
			}
			if cause != wantCause {
				t.Fatalf("cause=%s want=%s", cause, wantCause)
			}
			if name == "Cleaned" {
				gate2Git(t, repo, "worktree", "remove", "--force", f.path)
				if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
					t.Fatal(err)
				}
			}
			approved := f.continueBlocked(t)
			// Recreate both processes so only persisted evidence survives.
			f.stop()
			f.start()
			p, err = New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := p.claim(ctx)
			if err != nil || claim == nil || claim.Execution.ID != first.Execution.ID || claim.Execution.ResumePoint == nil || *claim.Execution.ResumePoint != "gate3" || claim.Execution.ResumeHeadSHA != approved || claim.Execution.VerifyRun == nil {
				t.Fatalf("reclaim=%+v err=%v", claim, err)
			}
			var prepare StageFunc
			if name == "Cleaned" {
				prepare = func(_ context.Context, e *Execution) error {
					gate2Git(t, repo, "worktree", "add", e.WorktreePath, e.Claim.Worktree.BranchName)
					_, err := f.db.Exec(`UPDATE worktrees SET status='active',cleaned_at=NULL`)
					return err
				}
			}
			executor := f.executor(t, p, nil, nil, prepare)
			var stages []StageID
			for i := range executor.stages {
				stage := executor.stages[i]
				executor.stages[i].run = func(ctx context.Context, e *Execution) error {
					stages = append(stages, stage.ID)
					return stage.run(ctx, e)
				}
			}
			err = executor.Execute(ctx, *claim)
			pass := name != "UnverifiableRun" && name != "ZeroExecuted"
			wantStages := []StageID{StageGate3}
			if pass {
				wantStages = append(wantStages, StagePublish, StageComplete)
			}
			if pass && err != nil || !pass && !errors.Is(err, ErrGate3Stopped) || f.calls != 2 || (f.published == 1) != pass || !reflect.DeepEqual(stages, wantStages) {
				t.Fatalf("resume=%v tests=%d published=%d stages=%v", err, f.calls, f.published, stages)
			}
			var baselines, verifies, gate2, gate3, decisions, audits int
			if err := f.db.QueryRow(`SELECT
				(SELECT count(*) FROM test_runs WHERE kind='baseline'),(SELECT count(*) FROM test_runs WHERE kind='verify'),
				(SELECT count(*) FROM gate_results WHERE gate=2),(SELECT count(*) FROM gate_results WHERE gate=3),
				(SELECT count(*) FROM execution_blocks WHERE decision='continue'),(SELECT count(*) FROM audit_logs WHERE action='task.resume')`).Scan(&baselines, &verifies, &gate2, &gate3, &decisions, &audits); err != nil || baselines != 1 || verifies != 1 || gate2 != 1 || gate3 != 1 || decisions != 1 || audits != 1 {
				t.Fatalf("saved runs=%d/%d gates=%d/%d decisions=%d audits=%d err=%v", baselines, verifies, gate2, gate3, decisions, audits, err)
			}
			if !pass {
				if err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE decision IS NULL`).Scan(&cause); err != nil || cause != "gate3_mismatch" {
					t.Fatalf("unmeasured run allowed through Gate 3: cause=%s err=%v", cause, err)
				}
			}
		})
	}
}

func TestBlockedResumeHTTPGate3RechecksUntilRepaired(t *testing.T) {
	f := newGate3Fixture(t)
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
	dirty := filepath.Join(f.path, "unexpected")
	if err := f.executor(t, p, func() { gate2Write(t, dirty, "x") }, nil, nil).Execute(ctx, *claim); !errors.Is(err, ErrGate3Stopped) {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			if err := os.Remove(dirty); err != nil {
				t.Fatal(err)
			}
		}
		approved := f.continueBlocked(t)
		p, err = New(f.config)
		if err != nil {
			t.Fatal(err)
		}
		claim, err = p.claim(ctx)
		if err != nil || claim == nil || claim.Execution.ResumeHeadSHA != approved {
			t.Fatalf("claim=%+v err=%v", claim, err)
		}
		err = f.executor(t, p, nil, nil, nil).Execute(ctx, *claim)
		if attempt == 0 && (!errors.Is(err, ErrGate3Stopped) || f.published != 0) || attempt == 1 && (err != nil || f.published != 1) || f.calls != 2 {
			t.Fatalf("attempt=%d err=%v published=%d tests=%d", attempt, err, f.published, f.calls)
		}
	}
	var checks, blocks int
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM gate_results WHERE gate=3),(SELECT count(*) FROM execution_blocks WHERE decision='continue')`).Scan(&checks, &blocks); err != nil || checks != 3 || blocks != 2 {
		t.Fatalf("checks=%d blocks=%d err=%v", checks, blocks, err)
	}
}

func TestBlockedResumeHTTPChangedHEADStopsWithGitState(t *testing.T) {
	for _, changedAfterDecision := range []bool{false, true} {
		name := "UntestedHEADApproved"
		if changedAfterDecision {
			name = "HEADChangedAfterDecision"
		}
		t.Run(name, func(t *testing.T) {
			f := newGate3Fixture(t)
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
			if err := f.executor(t, p, func() {
				if changedAfterDecision {
					gate2Write(t, filepath.Join(f.path, "unexpected"), "x")
				} else {
					gate2Git(t, f.path, "commit", "--quiet", "--allow-empty", "-m", "untested commit")
				}
			}, nil, nil).Execute(ctx, *claim); !errors.Is(err, ErrGate3Stopped) {
				t.Fatal(err)
			}
			approved := f.continueBlocked(t)
			if changedAfterDecision {
				gate2Git(t, f.path, "commit", "--quiet", "--allow-empty", "-m", "changed after approval")
			}
			current := gate2Git(t, f.path, "rev-parse", "HEAD")
			if changedAfterDecision && current == approved {
				t.Fatal("fixture did not change HEAD")
			}
			claim, err = p.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			if err := f.executor(t, p, nil, nil, nil).Execute(ctx, *claim); !errors.Is(err, ErrGate3Stopped) || f.calls != 2 || f.published != 0 {
				t.Fatalf("resume=%v calls=%d published=%d", err, f.calls, f.published)
			}
			var cause, head, tested string
			wantCause := "gate3_mismatch"
			if changedAfterDecision {
				wantCause = "git_state"
			}
			if err := f.db.QueryRow(`SELECT b.cause,b.head_sha,e.verified_sha FROM execution_blocks b JOIN executions e ON e.id=b.execution_id WHERE b.decision IS NULL`).Scan(&cause, &head, &tested); err != nil || cause != wantCause || head != current || tested != claim.Execution.VerifiedSHA || tested == current {
				t.Fatalf("cause=%s head=%s tested=%s err=%v", cause, head, tested, err)
			}
		})
	}
}
