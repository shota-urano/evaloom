package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileRejectsUnreferencedEscapingLinks(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, kind := range []string{"absolute", "parent", "chain", "dangling", "cycle"} {
			name := kind
			if cached {
				name += "/cached"
			}
			t.Run(name, func(t *testing.T) {
				f, runner := newProfileFixture(t), newProfileRunner(t)
				root := f.repo
				if cached {
					root = loadTestProfile(t, runner, f.claim).Path
					if err := os.Chmod(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
				target := "../outside"
				switch kind {
				case "absolute":
					target = t.TempDir()
				case "chain":
					if err := os.Symlink("../outside", filepath.Join(root, "intermediate")); err != nil {
						t.Fatal(err)
					}
					target = "intermediate/file"
				case "dangling":
					target = "not-in-commit"
				case "cycle":
					target = "unreferenced"
				}
				if err := os.Symlink(target, filepath.Join(root, "unreferenced")); err != nil {
					t.Fatal(err)
				}
				if cached {
					if err := os.Chmod(root, 0500); err != nil {
						t.Fatal(err)
					}
				} else {
					f.commit()
					f.git("tag", "-f", f.claim.Profile.TagName)
				}
				assertProfileConfigError(t, runner, f.claim)
			})
		}
	}
}

func TestProfileRejectsUnreferencedSubmodule(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		name := "visible"
		if hidden {
			name = "export-ignored"
		}
		t.Run(name, func(t *testing.T) {
			f, runner := newProfileFixture(t), newProfileRunner(t)
			if hidden {
				f.write(".gitattributes", "unreferenced export-ignore\n", 0600)
				f.git("add", ".gitattributes")
			}
			f.git("update-index", "--add", "--cacheinfo", "160000,"+f.claim.Profile.CommitSHA+",unreferenced")
			f.git("commit", "-qm", "unreferenced submodule")
			f.claim.Profile.CommitSHA = f.git("rev-parse", "HEAD")
			f.git("tag", "-f", f.claim.Profile.TagName)
			assertProfileConfigError(t, runner, f.claim)
		})
	}
}

func TestProfileReadDisablesRepositoryExecutables(t *testing.T) {
	for _, kind := range []string{"regular", "linked", "bare"} {
		t.Run(kind, func(t *testing.T) {
			f, runner := newProfileFixture(t), newProfileRunner(t)
			f.write(".gitattributes", "* filter=attack diff=attack merge=attack\n", 0600)
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
			payload := t.TempDir()
			marker, script := filepath.Join(payload, "ran"), filepath.Join(payload, "attack")
			body := "#!/bin/sh\nprintf ran >> '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\n"
			if err := os.WriteFile(script, []byte(body+"cat\n"), 0700); err != nil {
				t.Fatal(err)
			}
			monitor, monitorMarker := filepath.Join(payload, "fsmonitor"), filepath.Join(payload, "fsmonitor.ran")
			monitorBody := "#!/bin/sh\nprintf ran >> '" + strings.ReplaceAll(monitorMarker, "'", "'\\''") + "'\nprintf 'token\\000'\n"
			if err := os.WriteFile(monitor, []byte(monitorBody), 0700); err != nil {
				t.Fatal(err)
			}
			// Exercise both the default hook directory and core.hooksPath. The
			// include makes named executable settings pass through config discovery.
			hooks := f.git("rev-parse", "--path-format=absolute", "--git-path", "hooks")
			if kind != "bare" {
				hooks = filepath.Join(payload, "hooks")
				f.git("config", "core.hooksPath", hooks)
			}
			if err := os.MkdirAll(hooks, 0700); err != nil {
				t.Fatal(err)
			}
			for _, hook := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
				if err := os.WriteFile(filepath.Join(hooks, hook), []byte(body+"exit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			include := filepath.Join(payload, "config")
			for _, key := range []string{"core.sshCommand", "credential.helper", "core.pager", "gpg.program",
				"filter.attack.clean", "filter.attack.smudge", "filter.attack.process", "diff.attack.command", "diff.attack.textconv", "merge.attack.driver"} {
				f.git("config", "--file", include, key, script)
			}
			f.git("config", "--file", include, "core.fsmonitor", monitor)
			f.git("config", "include.path", include)
			// Ambient Git overrides must not redirect the Runner to another tree
			// or enable an external program, either on first load or a cache hit.
			t.Setenv("GIT_DIR", filepath.Join(payload, "not-a-repository"))
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
			t.Setenv("GIT_CONFIG_VALUE_0", monitor)
			if kind != "bare" {
				f.write("work/instructions.md", "uncommitted instructions", 0600)
				f.write("bin/check", "uncommitted program", 0700)
			}
			for range 2 {
				loaded := loadTestProfile(t, runner, f.claim)
				for name, want := range map[string]string{
					"work/instructions.md": "committed instructions\n", "bin/check": "#!/bin/sh\nexit 0\n",
				} {
					got, err := os.ReadFile(filepath.Join(loaded.Path, name))
					if err != nil || string(got) != want {
						t.Fatalf("%s = %q, %v; want fixed commit %q", name, got, err, want)
					}
				}
				for _, path := range []string{marker, monitorMarker} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("repository executable ran during Profile read (%s): %v", path, err)
					}
				}
			}
			// Positive control: these same hooks execute with ordinary Git.
			// Disable filters/fsmonitor only for this control to avoid speaking
			// their protocols; hook execution alone must create the marker.
			worktree := f.repo
			if kind == "bare" {
				worktree = t.TempDir()
			}
			f.git("--work-tree="+worktree, "-c", "core.bare=false", "-c", "core.fsmonitor=false", "-c", "filter.attack.process=",
				"-c", "filter.attack.smudge=", "checkout", "--detach", "--force", f.claim.Profile.CommitSHA)
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("positive control did not execute a hook: %v", err)
			}
			f.git("--work-tree="+worktree, "-c", "core.bare=false", "-c", "filter.attack.process=", "-c", "filter.attack.clean=", "status", "--porcelain")
			if _, err := os.Stat(monitorMarker); err != nil {
				t.Fatalf("positive control did not execute fsmonitor: %v", err)
			}
		})
	}
}
