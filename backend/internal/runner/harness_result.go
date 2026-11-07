package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

// Keep one closed file and identity per Execution, including across delivery
// attempts. A retry must not reread a changed worktree or allocate another ID.
type harnessSnapshot struct {
	head, base, worktree string
	commits              []string
	diff                 LogArtifact
}

var ErrHarnessFailed = errors.New("runner: harness failed")

// HarnessResultHandler runs at StageHarnessHead after the harness stage has
// stopped ALL Sandbox writers and supplied HarnessResult. It fills the trusted
// Git fields before Gate 2 and retains the adapter's summary, status and usage.
func (runner *Runner) HarnessResultHandler() StageFunc {
	return func(ctx context.Context, e *Execution) error {
		if err := runner.assembleHarnessResult(ctx, e); err != nil {
			return err
		}
		if e.HarnessResult.Status == adapter.ResultFailed {
			class := model.ErrorExecutionFailed
			if e.HarnessResult.ErrorClass != nil {
				class = *e.HarnessResult.ErrorClass
			}
			if e.control != nil {
				return errors.Join(ErrHarnessFailed, e.report(model.ExecutionFailed, class))
			}
			return ErrHarnessFailed
		}
		return nil
	}
}

func (runner *Runner) assembleHarnessResult(ctx context.Context, e *Execution) error {
	if e == nil || e.HarnessResult == nil || e.WorktreePath == "" || !harnessSHA(e.BaseSHA) || runner.dataDir == "" {
		return fmt.Errorf("runner: harness result and prepared worktree are required")
	}
	// Discard CLI claims even when Git or artifact delivery subsequently fails.
	e.HarnessResult.HeadSHA, e.HarnessResult.Commits, e.HarnessResult.DiffArtifactID = "", []string{}, ""
	if err := e.HarnessResult.Validate(); err != nil {
		return fmt.Errorf("runner: invalid harness result")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.harnessSnapshot == nil {
		snapshot, err := runner.snapshotHarness(ctx, e)
		if err != nil {
			return err
		}
		e.harnessSnapshot = snapshot
	}
	snapshot := e.harnessSnapshot
	if snapshot.base != e.BaseSHA || snapshot.worktree != e.WorktreePath || snapshot.diff.ExecutionID != e.Claim.Execution.ID {
		return fmt.Errorf("runner: harness snapshot scope changed")
	}
	if err := runner.deliverArtifact(ctx, snapshot.diff); err != nil {
		return err
	}
	e.HarnessHeadSHA, e.HarnessResult.HeadSHA = snapshot.head, snapshot.head
	e.HarnessResult.Commits = append([]string{}, snapshot.commits...)
	e.HarnessResult.DiffArtifactID = snapshot.diff.ID
	return nil
}

func (runner *Runner) snapshotHarness(ctx context.Context, e *Execution) (*harnessSnapshot, error) {
	if id, err := ulid.ParseStrict(e.Claim.Execution.ID); err != nil || id.String() != e.Claim.Execution.ID {
		return nil, fmt.Errorf("runner: invalid harness execution ID")
	}
	git := func(args ...string) ([]byte, error) {
		out, err := safegit.Output(ctx, e.WorktreePath, args...)
		if err != nil {
			// Git diagnostics can contain untrusted paths and secrets.
			return nil, errors.Join(fmt.Errorf("runner: cannot collect harness Git result"), ctx.Err())
		}
		return out, nil
	}
	headOut, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	head := strings.TrimSpace(string(headOut))
	if !harnessSHA(head) {
		return nil, fmt.Errorf("runner: invalid worktree HEAD")
	}
	// Pin every command to the observed HEAD, so the three fields describe
	// the same commit. BaseSHA comes from trusted worktree preparation.
	rangeSpec := e.BaseSHA + ".." + head
	commitOut, err := git("rev-list", rangeSpec, "--")
	if err != nil {
		return nil, err
	}
	commits := strings.Fields(string(commitOut))
	for _, sha := range commits {
		if !harnessSHA(sha) {
			return nil, fmt.Errorf("runner: invalid worktree commit")
		}
	}
	diff, err := git("diff", rangeSpec, "--")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(runner.dataDir, "artifacts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("runner: create local diff directory failed")
	}
	id := ulid.Make().String()
	path := filepath.Join(dir, id+".diff")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("runner: create local diff failed")
	}
	_, writeErr := file.Write(diff)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		// This is our own incomplete file, never eligible for delivery.
		_ = os.Remove(path)
		return nil, fmt.Errorf("runner: save local diff failed")
	}
	return &harnessSnapshot{head: head, base: e.BaseSHA, worktree: e.WorktreePath, commits: commits,
		diff: LogArtifact{ID: id, ExecutionID: e.Claim.Execution.ID, Kind: "diff", Path: path}}, nil
}

func harnessSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
