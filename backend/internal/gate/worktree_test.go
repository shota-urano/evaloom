package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func gateGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", path, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func TestCheckWorktreeChangesAndCopyExceptions(t *testing.T) {
	for _, name := range []string{"Clean", "Untracked", "NestedUntracked", "Modified", "Staged", "Deleted", "Renamed", "UnchangedCopy", "ChangedCopy", "StagedCopy", "CopyAndUntracked", "CopySymlink", "CopyParentSymlink", "Report", "QuotedCopy"} {
		t.Run(name, func(t *testing.T) {
			path := t.TempDir()
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			gateGit(t, path, "init", "--quiet")
			write("tracked", "original")
			gateGit(t, path, "add", ".")
			gateGit(t, path, "commit", "--quiet", "-m", "fixture")
			sha := gateGit(t, path, "rev-parse", "HEAD")
			var copied []model.CopiedFile
			copyName := "local.conf"
			if name == "QuotedCopy" {
				copyName = "space and\nnewline.conf"
			}
			if strings.Contains(name, "Copy") || name == "QuotedCopy" {
				write(copyName, "copied")
				copied = []model.CopiedFile{{Path: copyName, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("copied")))}}
			}
			switch name {
			case "Untracked", "CopyAndUntracked":
				write("unexpected", "x")
			case "NestedUntracked":
				if err := os.Mkdir(filepath.Join(path, "newdir"), 0700); err != nil {
					t.Fatal(err)
				}
				write("newdir/file", "x")
			case "Modified", "Staged":
				write("tracked", "changed")
				if name == "Staged" {
					gateGit(t, path, "add", "tracked")
				}
			case "Deleted":
				if err := os.Remove(filepath.Join(path, "tracked")); err != nil {
					t.Fatal(err)
				}
			case "Renamed":
				if err := os.Rename(filepath.Join(path, "tracked"), filepath.Join(path, "renamed")); err != nil {
					t.Fatal(err)
				}
				gateGit(t, path, "add", ".")
			case "ChangedCopy":
				write(copyName, "changed")
			case "StagedCopy":
				gateGit(t, path, "add", copyName)
			case "CopySymlink":
				write("tracked", "copied")
				gateGit(t, path, "add", "tracked")
				gateGit(t, path, "commit", "--quiet", "-m", "fixture copy target")
				sha = gateGit(t, path, "rev-parse", "HEAD")
				if err := os.Remove(filepath.Join(path, copyName)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("tracked", filepath.Join(path, copyName)); err != nil {
					t.Fatal(err)
				}
			case "CopyParentSymlink":
				if err := os.Symlink(".", filepath.Join(path, "alias")); err != nil {
					t.Fatal(err)
				}
				copied[0].Path = "alias/" + copyName
			case "Report":
				write("report.xml", "test output")
			}
			head, clean, err := CheckWorktree(context.Background(), path, copied)
			want := name == "Clean" || name == "UnchangedCopy" || name == "QuotedCopy"
			if err != nil || head != sha || clean != want {
				t.Fatalf("head=%s clean=%v want=%v err=%v", head, clean, want, err)
			}
		})
	}
}

func TestCheckCommitRequiresExecutedGate2AtVerifiedSHA(t *testing.T) {
	for _, name := range []string{"Valid", "NoSHA", "ChangedHEAD", "NoRun", "WrongSHA", "WrongExecution", "Baseline", "UnknownTotal", "UnknownSkip", "Zero", "AllSkipped", "NegativeSkip"} {
		t.Run(name, func(t *testing.T) {
			head, verified := "tested", "tested"
			run := &model.TestRun{ExecutionID: "execution", Kind: "verify", TargetSHA: "tested", Total: new(3), Skipped: new(1)}
			switch name {
			case "NoSHA":
				verified = ""
			case "ChangedHEAD":
				head = "newcommit"
			case "NoRun":
				run = nil
			case "WrongSHA":
				run.TargetSHA = "other"
			case "WrongExecution":
				run.ExecutionID = "predecessor"
			case "Baseline":
				run.Kind = "baseline"
			case "UnknownTotal":
				run.Total = nil
			case "UnknownSkip":
				run.Skipped = nil
			case "Zero":
				run.Total, run.Skipped = new(0), new(0)
			case "AllSkipped":
				run.Skipped = new(3)
			case "NegativeSkip":
				run.Skipped = new(-1)
			}
			if got := CheckCommit("execution", head, verified, run); got != (name == "Valid") {
				t.Fatalf("accepted=%v", got)
			}
		})
	}
}
