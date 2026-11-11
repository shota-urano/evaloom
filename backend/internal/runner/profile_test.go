package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type profileFixture struct {
	t     *testing.T
	repo  string
	claim Claim
	def   contract.ProfileDefinition
}

func newProfileFixture(t *testing.T) *profileFixture {
	t.Helper()
	f := &profileFixture{t: t, repo: t.TempDir()}
	f.git("init", "-q")
	f.git("config", "user.name", "Evaloom Test")
	f.git("config", "user.email", "test@example.invalid")
	f.def = contract.ProfileDefinition{
		ContractVersion: "1", ProfileVersion: "1.0.0", Name: "test", AcceptanceCriteria: "optional",
		Params: &contract.ProfileParams{Schema: "schema.json"},
		Stages: contract.ProfileStages{
			Prepare: &contract.ProgramStage{Command: []string{"bin/prepare"}, TimeoutSec: 1},
			Work: contract.WorkStage{MicroHarness: model.AdapterCodex, Model: "test", TimeoutSec: 1,
				Instructions: []string{"work/instructions.md"}, Skills: []string{"work/skills/"}, Settings: []string{"work/settings.json"}},
			Check:   contract.ProgramStage{Command: []string{"bin/check"}, TimeoutSec: 1},
			Publish: contract.ProgramStage{Command: []string{"bin/publish"}, TimeoutSec: 1},
		},
	}
	for _, name := range []string{"prepare", "check", "publish"} {
		f.write("bin/"+name, "#!/bin/sh\nexit 0\n", 0700)
	}
	f.write("work/instructions.md", "committed instructions\n", 0600)
	f.write("work/skills/test/SKILL.md", "committed skill\n", 0600)
	f.write("work/settings.json", "{}", 0600)
	f.write("schema.json", "{}", 0600)
	f.write("evaloom/profile.yaml", "profileVersion: 1.0.0\n", 0600)
	f.write("other.txt", "committed data\n", 0600)
	f.commit()
	f.git("tag", "-a", "profile/v1.0.0", "-m", "profile")
	f.claim.Profile.RepoPath, f.claim.Profile.Version, f.claim.Profile.TagName = f.repo, "1.0.0", "profile/v1.0.0"
	f.definition()
	return f
}

func (f *profileFixture) definition() {
	f.t.Helper()
	data, err := json.Marshal(f.def)
	if err != nil {
		f.t.Fatal(err)
	}
	f.claim.Profile.Definition = data
	if _, issues := contract.DecodeProfile(data); len(issues) != 0 {
		f.t.Fatalf("invalid fixture definition: %v", issues)
	}
}

func (f *profileFixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.repo}, args...)...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *profileFixture) write(name, data string, mode fs.FileMode) {
	f.t.Helper()
	name = filepath.Join(f.repo, name)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *profileFixture) commit() {
	f.t.Helper()
	f.git("add", ".")
	f.git("commit", "-qm", "fixture", "--allow-empty")
	f.claim.Profile.CommitSHA = f.git("rev-parse", "HEAD")
}

func newProfileRunner(t *testing.T) *Runner {
	t.Helper()
	r := &Runner{dataDir: filepath.Join(t.TempDir(), "data")}
	t.Cleanup(func() {
		// Go's TempDir cleanup cannot unlink children from read-only directories.
		_ = filepath.WalkDir(r.dataDir, func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	return r
}

func loadTestProfile(t *testing.T, runner *Runner, claim Claim) LoadedProfile {
	t.Helper()
	var result LoadedProfile
	if err := runner.WithProfile(context.Background(), claim, func(_ context.Context, p LoadedProfile) error {
		result = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result.Path == "" {
		t.Fatal("profile continuation was not called")
	}
	return result
}

func assertProfileConfigError(t *testing.T, runner *Runner, claim Claim) {
	t.Helper()
	started := false
	err := runner.WithProfile(context.Background(), claim, func(context.Context, LoadedProfile) error {
		started = true
		return nil
	})
	var config *ProfileConfigError
	if started || !errors.As(err, &config) || config.Status() != model.ExecutionFailed || config.ErrorClass() != model.ErrorConfig {
		t.Fatalf("stage started=%v, error=%v; want failed/config_error before starting", started, err)
	}
}

func TestProfileReadsOnlyFixedCommitAndIsReadOnly(t *testing.T) {
	f, runner := newProfileFixture(t), newProfileRunner(t)
	fixed := f.claim
	// HEAD and the index both differ from the fixed commit; neither is an input.
	f.write("other.txt", "new HEAD\n", 0600)
	f.commit()
	f.write("work/instructions.md", "staged instructions\n", 0600)
	f.git("add", "work/instructions.md")
	f.write("work/instructions.md", "unstaged instructions\n", 0600)
	f.write("untracked.txt", "untracked\n", 0600)
	f.write(".gitattributes", "other.txt export-ignore\n", 0600)
	before := f.git("status", "--porcelain=v1")
	loaded := loadTestProfile(t, runner, fixed)
	if loaded.Path != filepath.Join(runner.dataDir, "profiles", fixed.Profile.CommitSHA) || loaded.Definition.Stages.Check.Command[0] != "bin/check" {
		t.Fatalf("wrong loaded Profile: %+v", loaded)
	}
	for name, want := range map[string]string{
		"other.txt": "committed data\n", "work/instructions.md": "committed instructions\n",
	} {
		got, err := os.ReadFile(filepath.Join(loaded.Path, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(loaded.Path, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked file was extracted: %v", err)
	}
	if err := filepath.WalkDir(loaded.Path, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0222 != 0 {
			t.Errorf("writable extracted entry: %s %v", name, info.Mode())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(loaded.Path, "bin/check")
	info, err := os.Stat(command)
	if err != nil || info.Mode().Perm()&0100 == 0 {
		t.Fatalf("executable bit was lost: %v", err)
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(command, []byte("changed"), 0600); !os.IsPermission(err) {
			t.Fatalf("read-only command accepted a write: %v", err)
		}
		if err := os.WriteFile(filepath.Join(loaded.Path, "new.txt"), nil, 0600); !os.IsPermission(err) {
			t.Fatalf("read-only tree accepted a new file: %v", err)
		}
	}
	again := loadTestProfile(t, runner, fixed)
	after, err := os.Stat(command)
	if err != nil || again.Path != loaded.Path || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) {
		t.Fatal("cached content was replaced or rewritten")
	}
	if after := f.git("status", "--porcelain=v1"); after != before {
		t.Fatalf("repository changed: before=%q after=%q", before, after)
	}
}

func TestProfileRetargetedTagPreventsStartEvenWithCache(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncached", true: "cached"}[cached], func(t *testing.T) {
			f, runner := newProfileFixture(t), newProfileRunner(t)
			claim := f.claim
			if cached {
				loadTestProfile(t, runner, claim)
			}
			f.write("other.txt", "different commit", 0600)
			f.commit()
			f.git("tag", "-f", claim.Profile.TagName)
			assertProfileConfigError(t, runner, claim)
		})
	}
}

func TestProfileIgnoresCoreAttributesFile(t *testing.T) {
	testProfileIgnoresExternalAttributes(t, func(f *profileFixture) {
		f.write("untracked.attributes", "work/instructions.md export-subst\nother.txt export-ignore\n", 0600)
		f.git("config", "core.attributesFile", filepath.Join(f.repo, "untracked.attributes"))
	})
}

func TestProfileIgnoresInfoAttributes(t *testing.T) {
	testProfileIgnoresExternalAttributes(t, func(f *profileFixture) {
		name := f.git("rev-parse", "--path-format=absolute", "--git-path", "info/attributes")
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("work/instructions.md export-subst\nother.txt export-ignore\n"), 0600); err != nil {
			f.t.Fatal(err)
		}
	})
}

func testProfileIgnoresExternalAttributes(t *testing.T, configure func(*profileFixture)) {
	t.Helper()
	for _, kind := range []string{"regular", "linked", "bare"} {
		t.Run(kind, func(t *testing.T) {
			f, runner := newProfileFixture(t), newProfileRunner(t)
			const instructions = "$Format:%H$\n"
			f.write("work/instructions.md", instructions, 0600)
			f.commit()
			f.git("tag", "-f", f.claim.Profile.TagName)
			if kind != "regular" {
				repo := filepath.Join(t.TempDir(), kind)
				if kind == "linked" {
					f.git("worktree", "add", "--detach", repo, f.claim.Profile.CommitSHA)
				} else {
					f.git("clone", "--bare", f.repo, repo)
				}
				f.repo, f.claim.Profile.RepoPath = repo, repo
			}
			configure(f)
			loaded := loadTestProfile(t, runner, f.claim)
			for name, want := range map[string]string{
				"work/instructions.md": instructions, "other.txt": "committed data\n",
			} {
				got, err := os.ReadFile(filepath.Join(loaded.Path, name))
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want commit content %q", name, got, err, want)
				}
			}
		})
	}
}

func TestProfileDeletedTagContinuesButInvalidTagOrMissingCommitStops(t *testing.T) {
	f, runner := newProfileFixture(t), newProfileRunner(t)
	f.git("tag", "-d", f.claim.Profile.TagName)
	loadTestProfile(t, runner, f.claim)
	missing := f.claim
	missing.Profile.CommitSHA = strings.Repeat("f", 40)
	assertProfileConfigError(t, runner, missing)
	f.git("tag", f.claim.Profile.TagName, f.git("rev-parse", "HEAD^{tree}"))
	assertProfileConfigError(t, runner, f.claim)
}

func TestProfileInvalidReferencesPreventStart(t *testing.T) {
	for _, test := range []string{"prepare link", "check link", "publish link", "instructions link", "skills link", "settings link", "schema link", "directory descendant link", "ancestor link", "missing", "submodule", "export ignored"} {
		t.Run(test, func(t *testing.T) {
			f, runner := newProfileFixture(t), newProfileRunner(t)
			var link string
			switch test {
			case "prepare link":
				link, f.def.Stages.Prepare.Command[0] = "linked", "linked"
			case "check link":
				link, f.def.Stages.Check.Command[0] = "linked", "linked"
			case "publish link":
				link, f.def.Stages.Publish.Command[0] = "linked", "linked"
			case "instructions link":
				link, f.def.Stages.Work.Instructions[0] = "linked", "linked"
			case "skills link":
				link, f.def.Stages.Work.Skills[0] = "linked", "linked"
			case "settings link":
				link, f.def.Stages.Work.Settings[0] = "linked", "linked"
			case "schema link":
				link, f.def.Params.Schema = "linked", "linked"
			case "directory descendant link":
				link = "work/skills/linked"
			case "ancestor link":
				link, f.def.Stages.Check.Command[0] = "linked", "linked/check"
			case "missing":
				f.def.Stages.Check.Command[0] = "missing"
			case "submodule":
				f.git("update-index", "--add", "--cacheinfo", "160000,"+f.claim.Profile.CommitSHA+",submodule")
				f.def.Stages.Work.Skills[0] = "submodule"
			case "export ignored":
				f.write(".gitattributes", "bin/check export-ignore\n", 0600)
			}
			if link != "" {
				if err := os.Symlink("bin", filepath.Join(f.repo, link)); err != nil {
					t.Fatal(err)
				}
			}
			if test == "submodule" {
				f.git("commit", "-qm", "submodule")
				f.claim.Profile.CommitSHA = f.git("rev-parse", "HEAD")
			} else {
				f.commit()
			}
			f.git("tag", "-f", f.claim.Profile.TagName)
			f.definition()
			assertProfileConfigError(t, runner, f.claim)
			entries, err := os.ReadDir(filepath.Join(runner.dataDir, "profiles"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal("failed extraction left a cache or temporary directory")
			}
		})
	}
}

func TestProfileCachedReferencesAreRevalidatedWithoutFollowingLinks(t *testing.T) {
	for _, test := range []string{"file link", "ancestor link", "descendant link", "writable", "unreadable", "command directory", "cache link"} {
		t.Run(test, func(t *testing.T) {
			f, runner := newProfileFixture(t), newProfileRunner(t)
			loaded := loadTestProfile(t, runner, f.claim)
			if test == "writable" || test == "unreadable" {
				mode := fs.FileMode(0700)
				if test == "unreadable" {
					mode = 0
				}
				if err := os.Chmod(filepath.Join(loaded.Path, "bin/check"), mode); err != nil {
					t.Fatal(err)
				}
			} else if test == "cache link" {
				renamed := loaded.Path + ".original"
				if err := os.Rename(loaded.Path, renamed); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(renamed, loaded.Path); err != nil {
					t.Fatal(err)
				}
			} else {
				name, target := "bin/check", "publish"
				if test == "ancestor link" {
					name, target = "work", "bin"
				} else if test == "descendant link" {
					name, target = "work/skills/linked", "../instructions.md"
				}
				location := filepath.Join(loaded.Path, name)
				parent := filepath.Dir(location)
				if err := os.Chmod(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if test != "descendant link" {
					if err := os.Rename(location, location+".original"); err != nil {
						t.Fatal(err)
					}
				}
				if test == "command directory" {
					if err := os.Mkdir(location, 0500); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(target, location); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chmod(parent, 0500); err != nil {
					t.Fatal(err)
				}
			}
			assertProfileConfigError(t, runner, f.claim)
		})
	}
}

func TestProfileUnreferencedSymlinkDoesNotRedirectExtraction(t *testing.T) {
	f, runner := newProfileFixture(t), newProfileRunner(t)
	outside := t.TempDir()
	before, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.repo, "unreferenced")); err != nil {
		t.Fatal(err)
	}
	f.commit()
	f.git("tag", "-f", f.claim.Profile.TagName)
	loaded := loadTestProfile(t, runner, f.claim)
	if target, err := os.Readlink(filepath.Join(loaded.Path, "unreferenced")); err != nil || target != outside {
		t.Fatalf("unreferenced symlink changed: %q, %v", target, err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode() != before.Mode() {
		t.Fatalf("extraction changed the symlink target: %v", err)
	}
}

func TestProfileConcurrentLoadsAndCallbackErrors(t *testing.T) {
	f, runner := newProfileFixture(t), newProfileRunner(t)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { loadTestProfile(t, runner, f.claim) })
	}
	workers.Wait()
	want := errors.New("stage error")
	if err := runner.WithProfile(context.Background(), f.claim, func(context.Context, LoadedProfile) error { return want }); err != want {
		t.Fatalf("callback error changed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.WithProfile(ctx, f.claim, func(context.Context, LoadedProfile) error {
		t.Error("cancelled Profile started")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation classified as configuration error: %v", err)
	}
}

func TestProfileArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, header := range []tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/escape", Typeflag: tar.TypeReg},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "../escape"},
	} {
		t.Run(header.Name, func(t *testing.T) {
			var data bytes.Buffer
			writer := tar.NewWriter(&data)
			if err := writer.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := extractProfile(context.Background(), root, data.Bytes()); err == nil {
				t.Fatal("unsafe archive entry accepted")
			}
		})
	}
}
