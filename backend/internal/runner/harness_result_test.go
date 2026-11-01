package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func reportedHarnessResult() *adapter.HarnessResult {
	return &adapter.HarnessResult{
		ContractVersion: adapter.ContractVersion, Status: adapter.ResultSucceeded,
		SessionID: "cli-session", HeadSHA: strings.Repeat("f", 40),
		Commits: []string{strings.Repeat("e", 40)}, DiffArtifactID: "cli-diff",
		Summary: "CLI final summary", LogArtifactIDs: []string{}, ArtifactIDs: []string{},
	}
}

// Real linked worktree, two committed edits, and uncommitted/staged/untracked
// changes. Only the base..HEAD content belongs in the result's diff artifact.
func harnessResultWorktree(t *testing.T) (path, base, head, previous string) {
	t.Helper()
	repo := t.TempDir()
	gate2Git(t, repo, "init", "--quiet")
	gate2Write(t, filepath.Join(repo, "tracked.txt"), "base\n")
	gate2Git(t, repo, "add", ".")
	gate2Git(t, repo, "commit", "--quiet", "-m", "base")
	base = gate2Git(t, repo, "rev-parse", "HEAD")
	path = filepath.Join(t.TempDir(), "worktree")
	gate2Git(t, repo, "worktree", "add", "--quiet", "-b", "harness", path, base)
	gate2Write(t, filepath.Join(path, "tracked.txt"), "first\n")
	gate2Git(t, path, "add", ".")
	gate2Git(t, path, "commit", "--quiet", "-m", "first")
	previous = gate2Git(t, path, "rev-parse", "HEAD")
	gate2Write(t, filepath.Join(path, "tracked.txt"), "committed result\n")
	gate2Write(t, filepath.Join(path, "added.txt"), "committed new file\n")
	gate2Git(t, path, "add", ".")
	gate2Git(t, path, "commit", "--quiet", "-m", "second")
	head = gate2Git(t, path, "rev-parse", "HEAD")
	gate2Write(t, filepath.Join(path, "tracked.txt"), "UNCOMMITTED\n")
	gate2Write(t, filepath.Join(path, "staged.txt"), "STAGED\n")
	gate2Git(t, path, "add", "staged.txt")
	gate2Write(t, filepath.Join(path, "untracked.txt"), "UNTRACKED\n")
	return
}

func TestHarnessResultUsesWorktreeInsteadOfCLIAndPersistsDiff(t *testing.T) {
	for _, status := range []adapter.ResultStatus{adapter.ResultSucceeded, adapter.ResultFailed} {
		t.Run(string(status), func(t *testing.T) {
			f := newLogControlPlane(t)
			r, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			path, base, head, previous := harnessResultWorktree(t)
			claim := reportedHarnessResult()
			claim.Status = status
			if status == adapter.ResultFailed {
				class := adapter.ErrorConfig
				claim.ErrorClass = &class
				claim.Summary = "" // Unavailable CLI final output stays empty.
			}
			tokens := int64(73)
			claim.Usage.InputTokens = &tokens
			body, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			// A CLI stub actually emits false SHA/commits/artifact claims. The
			// trusted Runner must not obtain Git metadata from this output.
			cli := exec.Command("/bin/sh", "-c", `printf '%s' "$1"`, "test-cli", string(body))
			output, err := cli.Output()
			if err != nil {
				t.Fatal(err)
			}
			var result adapter.HarnessResult
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
			e := &Execution{WorktreePath: path, BaseSHA: base, HarnessResult: &result}
			e.Claim.Execution.ID = logExecution
			err = r.HarnessResultHandler()(context.Background(), e)
			if status == adapter.ResultFailed {
				if !errors.Is(err, ErrHarnessFailed) {
					t.Fatalf("failed result error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.HeadSHA != head || e.HarnessHeadSHA != head || !reflect.DeepEqual(result.Commits, []string{head, previous}) {
				t.Fatalf("trusted Git result: head=%s recorded=%s commits=%v", result.HeadSHA, e.HarnessHeadSHA, result.Commits)
			}
			if result.Status != claim.Status || result.Summary != claim.Summary || !reflect.DeepEqual(result.ErrorClass, claim.ErrorClass) || !reflect.DeepEqual(result.Usage, claim.Usage) || result.SessionID != claim.SessionID {
				t.Fatalf("adapter metadata changed: %+v", result)
			}
			want := exec.Command("git", "-C", path, "diff", base+".."+head, "--")
			diff, err := want.Output()
			if err != nil || len(diff) == 0 {
				t.Fatalf("expected diff=%q err=%v", diff, err)
			}
			assertHarnessDiff(t, f, e, diff)
		})
	}
}

func assertHarnessDiff(t *testing.T, f *logControlPlane, e *Execution, want []byte) {
	t.Helper()
	var path, kind, hash string
	var size int64
	var redacted, count int
	id := e.HarnessResult.DiffArtifactID
	err := f.db.QueryRow(`SELECT storage_path,kind,sha256,size_bytes,redacted FROM artifacts WHERE id=? AND execution_id=?`, id, e.Claim.Execution.ID).
		Scan(&path, &kind, &hash, &size, &redacted)
	if err != nil || kind != "diff" || !filepath.IsLocal(path) || redacted != 0 {
		t.Fatalf("artifact id=%s path=%s kind=%s redacted=%d err=%v", id, path, kind, redacted, err)
	}
	stored, err := os.ReadFile(filepath.Join(f.dataDir, path))
	sum := sha256.Sum256(want)
	if err != nil || string(stored) != string(want) || hash != hex.EncodeToString(sum[:]) || size != int64(len(want)) {
		t.Fatalf("stored diff=%q hash=%s size=%d err=%v; want=%q", stored, hash, size, err, want)
	}
	local, err := os.ReadFile(e.harnessSnapshot.diff.Path)
	if err != nil || string(local) != string(want) {
		t.Fatalf("local diff=%q err=%v", local, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='diff'`, e.Claim.Execution.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("diff rows=%d err=%v", count, err)
	}
}

func TestHarnessResultEmptyRangePersistsEmptyDiff(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	path, _, head, _ := harnessResultWorktree(t)
	e := &Execution{WorktreePath: path, BaseSHA: head, HarnessResult: reportedHarnessResult()}
	e.Claim.Execution.ID = logExecution
	if err := r.HarnessResultHandler()(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.HarnessResult.HeadSHA != head || e.HarnessResult.Commits == nil || len(e.HarnessResult.Commits) != 0 {
		t.Fatalf("empty range result=%+v", e.HarnessResult)
	}
	assertHarnessDiff(t, f, e, []byte{})
}

func TestHarnessResultGitFailureDiscardsCLIClaimsAndNeverUploads(t *testing.T) {
	for _, invalid := range []string{"base", "worktree", "storage", "cancelled"} {
		t.Run(invalid, func(t *testing.T) {
			f := newLogControlPlane(t)
			r, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			path, base, _, _ := harnessResultWorktree(t)
			e := &Execution{WorktreePath: path, BaseSHA: base, HarnessResult: reportedHarnessResult()}
			e.Claim.Execution.ID = logExecution
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch invalid {
			case "base":
				e.BaseSHA = strings.Repeat("0", 40) // Valid shape, nonexistent object.
			case "worktree":
				e.WorktreePath = t.TempDir()
			case "storage":
				r.dataDir = filepath.Join(t.TempDir(), "file")
				gate2Write(t, r.dataDir, "not a directory")
			case "cancelled":
				cancel()
			}
			err = r.HarnessResultHandler()(ctx, e)
			if err == nil || invalid == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			if e.HarnessResult.HeadSHA != "" || e.HarnessResult.DiffArtifactID != "" || len(e.HarnessResult.Commits) != 0 || e.HarnessHeadSHA != "" {
				t.Fatalf("untrusted result retained: %+v", e.HarnessResult)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("artifacts=%d err=%v", count, err)
			}
		})
	}
}

func TestHarnessResultDiffRetryKeepsFileIDAndContentAfterLostAck(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	path, base, head, _ := harnessResultWorktree(t)
	e := &Execution{WorktreePath: path, BaseSHA: base, HarnessResult: reportedHarnessResult()}
	e.Claim.Execution.ID = logExecution
	// Store the first upload, lose its acknowledgement, then keep the retry
	// pending until cancellation. No artifact reference may escape before ack.
	var requests atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if requests.Add(1) == 1 {
			f.server.Config.Handler.ServeHTTP(httptest.NewRecorder(), req)
		} else {
			cancel()
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer proxy.Close()
	r.controlPlaneURL, r.pollInterval = proxy.URL, time.Millisecond
	if err := r.HarnessResultHandler()(ctx, e); !errors.Is(err, context.Canceled) {
		t.Fatalf("delivery failure=%v", err)
	}
	if e.HarnessResult.DiffArtifactID != "" || e.harnessSnapshot == nil {
		t.Fatalf("result published before ack or snapshot lost: %+v", e)
	}
	identity := e.harnessSnapshot.diff
	diff, err := os.ReadFile(identity.Path)
	if err != nil || len(diff) == 0 {
		t.Fatalf("pending file=%q err=%v", diff, err)
	}
	// Mutate HEAD between attempts. Delivery must replay the original file,
	// without running Git again or changing the result's snapshot commit.
	gate2Git(t, path, "commit", "--quiet", "--allow-empty", "-m", "later")
	r.controlPlaneURL = f.server.URL
	if err := r.HarnessResultHandler()(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.HarnessResult.HeadSHA != head || e.HarnessResult.DiffArtifactID != identity.ID || e.harnessSnapshot.diff.Path != identity.Path {
		t.Fatalf("retry changed snapshot: %+v", e.HarnessResult)
	}
	assertHarnessDiff(t, f, e, diff)
}

func TestHarnessResultWiredIntoGate2Executor(t *testing.T) {
	f := newGate2Fixture(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	claim, err := r.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	var observed *Execution
	var head string
	calls := 0
	executor := f.gate2Executor(t, r, &calls, func(_ context.Context, e *Execution) error {
		gate2Write(t, filepath.Join(f.path, "change.txt"), "harness commit\n")
		gate2Git(t, f.path, "add", "change.txt")
		gate2Git(t, f.path, "commit", "--quiet", "-m", "harness")
		head = gate2Git(t, f.path, "rev-parse", "HEAD")
		observed = e
		return nil
	}, nil, nil)
	if err := executor.Execute(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	var saved, artifact string
	err = f.db.QueryRow(`SELECT e.harness_head_sha,a.id FROM executions e JOIN artifacts a ON a.execution_id=e.id AND a.kind='diff' WHERE e.id=?`, claim.Execution.ID).Scan(&saved, &artifact)
	if err != nil || saved != head || observed.HarnessResult.HeadSHA != head || artifact != observed.HarnessResult.DiffArtifactID || calls != 2 {
		t.Fatalf("saved=%s artifact=%s result=%+v calls=%d err=%v", saved, artifact, observed.HarnessResult, calls, err)
	}
}

func TestHarnessResultFailurePersistsDiffAndStopsBeforeGate2(t *testing.T) {
	f := newGate2Fixture(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := r.claim(context.Background())
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	calls := 0
	executor := f.gate2Executor(t, r, &calls, func(_ context.Context, e *Execution) error {
		e.HarnessResult.Status = adapter.ResultFailed
		class := adapter.ErrorConfig
		e.HarnessResult.ErrorClass = &class
		return nil
	}, nil, nil)
	if err := executor.Execute(context.Background(), *claim); !errors.Is(err, ErrHarnessFailed) {
		t.Fatalf("harness failure=%v", err)
	}
	var status, class string
	var diffs, verifies int
	err = f.db.QueryRow(`SELECT e.status,e.error_class,
		(SELECT count(*) FROM artifacts WHERE execution_id=e.id AND kind='diff'),
		(SELECT count(*) FROM test_runs WHERE execution_id=e.id AND kind='verify')
		FROM executions e WHERE id=?`, claim.Execution.ID).Scan(&status, &class, &diffs, &verifies)
	if err != nil || status != "failed" || class != "config_error" || diffs != 1 || verifies != 0 || calls != 1 {
		t.Fatalf("status=%s class=%s diffs=%d verifies=%d calls=%d err=%v", status, class, diffs, verifies, calls, err)
	}
}
