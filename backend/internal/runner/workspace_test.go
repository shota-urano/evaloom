package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func requireWorkspaceError(t *testing.T, reporter *stageReportStub, want string) {
	t.Helper()
	if len(reporter.outcomes) != 1 || reporter.outcomes[0].RunnerError == nil || *reporter.outcomes[0].RunnerError != want ||
		len(reporter.started) != 1 || len(reporter.finished) != 1 {
		t.Fatalf("want one %s report, got starts=%v finishes=%v outcomes=%+v", want, reporter.started, reporter.finished, reporter.outcomes)
	}
}

func TestExecuteStagesCreatesEmptyPrivateWorkspace(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, false)
	// Also exercise creating workspaceRoot when it does not yet exist.
	r.workspaceRoot = filepath.Join(t.TempDir(), "workspaces")
	claim.Workspace.Path = filepath.Join(r.workspaceRoot, claim.Workspace.ID)
	claim.Workspace.Created = true
	calls := 0
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		calls++
		info, err := os.Stat(in.Workspace)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("workspace must be a directory with mode 0700: %v, %v", info, err)
		}
		entries, err := os.ReadDir(in.Workspace)
		if err != nil || len(entries) != 0 {
			t.Fatalf("new workspace must be empty: %v, %v", entries, err)
		}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(reporter.outcomes) != 3 {
		t.Fatalf("workspace was not reused across stages: calls=%d outcomes=%+v", calls, reporter.outcomes)
	}
}

func TestExecuteStagesRejectsWorkspaceEscape(t *testing.T) {
	for _, created := range []bool{false, true} {
		for _, missingTarget := range []bool{false, true} {
			t.Run(fmt.Sprintf("created=%v/missing-target=%v", created, missingTarget), func(t *testing.T) {
				r, claim, reporter, services := stageFixture(t, false)
				outside := t.TempDir()
				if missingTarget {
					outside = filepath.Join(outside, "not-created")
				}
				claim.Workspace.Path = filepath.Join(r.workspaceRoot, "escape")
				claim.Workspace.ID, claim.Workspace.Created = "escape", created
				if err := os.Symlink(outside, claim.Workspace.Path); err != nil {
					t.Fatal(err)
				}
				services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
					t.Fatal("escaping workspace must not run any stage")
					return StageOutcome{}
				})
				if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
					t.Fatal(err)
				}
				requireWorkspaceError(t, reporter, "config_error")
				if missingTarget {
					if _, err := os.Stat(outside); !os.IsNotExist(err) {
						t.Fatalf("created an outside target: %v", err)
					}
				} else if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
					t.Fatalf("changed outside directory: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestExecuteStagesMissingInheritedWorkspace(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%v", resume), func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, false)
			claim.Workspace.ID = "missing"
			claim.Workspace.Path = filepath.Join(r.workspaceRoot, claim.Workspace.ID)
			if resume {
				setStageResume(t, &claim, stageResume{Stage: model.StageWork})
			}
			services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
				t.Fatal("missing inherited workspace must not run any stage")
				return StageOutcome{}
			})
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			requireWorkspaceError(t, reporter, "workspace_missing")
			if _, err := os.Stat(claim.Workspace.Path); !os.IsNotExist(err) {
				t.Fatalf("recreated an inherited workspace: %v", err)
			}
		})
	}
}

func TestExecuteStagesNewWorkspaceDoesNotReuseExistingContents(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, false)
	claim.Workspace.Created = true
	services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
		t.Fatal("new workspace claim must not reuse an existing directory")
		return StageOutcome{}
	})
	if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
	requireWorkspaceError(t, reporter, "config_error")
	if data, err := os.ReadFile(filepath.Join(claim.Workspace.Path, "keep")); err != nil || string(data) != "workspace survives" {
		t.Fatalf("existing contents changed: %q, %v", data, err)
	}
}

func TestExecuteStagesRechecksWorkspaceBeforeEachStage(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, false)
	outside := t.TempDir()
	calls := 0
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		calls++
		if in.Stage != model.StageWork {
			t.Fatal("stage used workspace after it escaped")
		}
		// Move our fixture aside to keep its contents, then replace its path.
		if err := os.Rename(in.Workspace, in.Workspace+"-saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, in.Workspace); err != nil {
			t.Fatal(err)
		}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(reporter.outcomes) != 2 || reporter.outcomes[1].RunnerError == nil || *reporter.outcomes[1].RunnerError != "config_error" {
		t.Fatalf("did not recheck workspace: calls=%d outcomes=%+v", calls, reporter.outcomes)
	}
}

func TestExecuteStagesAllowsWorkspaceSymlinkInsideRoot(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, false)
	claim.Workspace.ID = "alias"
	path := filepath.Join(r.workspaceRoot, claim.Workspace.ID)
	if err := os.Symlink(claim.Workspace.Path, path); err != nil {
		t.Fatal(err)
	}
	claim.Workspace.Path = path
	if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
	if len(reporter.outcomes) != 3 || reporter.outcomes[0].RunnerError != nil {
		t.Fatalf("rejected a workspace inside root: %+v", reporter.outcomes)
	}
}

func TestWorkspaceRejectsInvalidStorageBeforeCreation(t *testing.T) {
	for _, kind := range []string{"data-overlap", "profile-repository", "outside-path", "invalid-id"} {
		t.Run(kind, func(t *testing.T) {
			r, claim, _, _ := stageFixture(t, false)
			switch kind {
			case "data-overlap":
				r.workspaceRoot = filepath.Join(r.dataDir, "workspaces")
			case "profile-repository":
				// Resolve a symlinked ancestor even for a not-yet-created root.
				alias := filepath.Join(t.TempDir(), "profile")
				if err := os.Symlink(claim.Profile.RepoPath, alias); err != nil {
					t.Fatal(err)
				}
				r.workspaceRoot = filepath.Join(alias, "workspaces")
			case "invalid-id":
				claim.Workspace.ID = "../escape"
			}
			claim.Workspace.Path = filepath.Join(r.workspaceRoot, claim.Workspace.ID)
			if kind == "outside-path" {
				claim.Workspace.Path = filepath.Join(t.TempDir(), "outside")
			}
			claim.Workspace.Created = true
			if err := r.prepareWorkspace(claim); err == nil {
				t.Fatal("accepted an invalid workspace storage path")
			}
			if _, err := os.Lstat(claim.Workspace.Path); !os.IsNotExist(err) {
				t.Fatalf("created workspace before validating storage: %v", err)
			}
		})
	}
}
