package harnessenv_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func file(name, body string) gitcontext.File {
	h := sha256.Sum256([]byte(body))
	return gitcontext.File{Path: name, Content: body, ContentHash: hex.EncodeToString(h[:]), SourceCommitSHA: strings.Repeat("b", 40)}
}

func config(t *testing.T) harnessenv.Config {
	t.Helper()
	return harnessenv.Config{Root: t.TempDir(), CommitSHA: strings.Repeat("b", 40), ConfigVariable: "CODEX_HOME",
		Files: []gitcontext.File{file("skills/check/SKILL.md", "check\r\n日本語\x00\xff"), file("config.toml", "model = 'fixture'\n")}}
}

func requireConfigError(t *testing.T, err error) {
	t.Helper()
	var failure *harnesscontext.ConfigError
	if !errors.As(err, &failure) || failure.Status() != model.ExecutionFailed || failure.ErrorClass() != model.ErrorConfig {
		t.Fatalf("expected failed(config_error), got %v", err)
	}
}

func TestDedicatedConfigurationsAreIsolatedAndExact(t *testing.T) {
	cfg := config(t)
	first, err := harnessenv.Create(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	cfg.ConfigVariable = "CLAUDE_CONFIG_DIR"
	second, err := harnessenv.Create(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if first.ConfigDir() == second.ConfigDir() || !reflect.DeepEqual(first.Env(), map[string]string{"CODEX_HOME": first.ConfigDir()}) ||
		!reflect.DeepEqual(second.Env(), map[string]string{"CLAUDE_CONFIG_DIR": second.ConfigDir()}) {
		t.Fatal("executions share configuration or inherit unrelated environment")
	}
	files, err := first.Files(context.Background())
	want := []gitcontext.File{cfg.Files[1], cfg.Files[0]}
	if err != nil || !reflect.DeepEqual(files, want) {
		t.Fatalf("disk manifest=%v err=%v; want=%v", files, err, want)
	}
	for _, f := range files {
		info, err := os.Stat(filepath.Join(first.ConfigDir(), filepath.FromSlash(f.Path)))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions: %v %v", info, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.ConfigDir()); !os.IsNotExist(err) {
		t.Fatal("cleanup left configuration behind")
	}
	if _, err := second.Files(context.Background()); err != nil {
		t.Fatal("cleanup affected another execution:", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal("cleanup not idempotent:", err)
	}
}

func TestRejectsInvalidPinsAndPathsWithoutPartialEnvironment(t *testing.T) {
	for name, edit := range map[string]func(*harnessenv.Config){
		"relative-root":      func(c *harnessenv.Config) { c.Root = "relative" },
		"moving-pin":         func(c *harnessenv.Config) { c.CommitSHA = "HEAD" },
		"wrong-pin":          func(c *harnessenv.Config) { c.Files[0].SourceCommitSHA = strings.Repeat("c", 40) },
		"wrong-hash":         func(c *harnessenv.Config) { c.Files[0].ContentHash = strings.Repeat("0", 64) },
		"ambient-home":       func(c *harnessenv.Config) { c.ConfigVariable = "HOME" },
		"duplicate":          func(c *harnessenv.Config) { c.Files = append(c.Files, c.Files[0]) },
		"traversal":          func(c *harnessenv.Config) { c.Files[0].Path = "../outside" },
		"absolute":           func(c *harnessenv.Config) { c.Files[0].Path = "/outside" },
		"backslash":          func(c *harnessenv.Config) { c.Files[0].Path = `skills\outside` },
		"dot":                func(c *harnessenv.Config) { c.Files[0].Path = "." },
		"directory-conflict": func(c *harnessenv.Config) { c.Files = []gitcontext.File{file("a", "a"), file("a/b", "b")} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config(t)
			root := cfg.Root
			edit(&cfg)
			env, err := harnessenv.Create(context.Background(), cfg)
			requireConfigError(t, err)
			entries, readErr := os.ReadDir(root)
			if env != nil || readErr != nil || len(entries) != 0 {
				t.Fatalf("partial environment returned or retained: %v %v %v", env, entries, readErr)
			}
		})
	}
}

func TestManifestRejectsUnrecordedChanges(t *testing.T) {
	for _, mode := range []string{"added", "removed", "changed", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config(t)
			env, err := harnessenv.Create(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = env.Close() })
			name := filepath.Join(env.ConfigDir(), "config.toml")
			switch mode {
			case "added":
				err = os.WriteFile(filepath.Join(env.ConfigDir(), "unrecorded"), []byte("extra"), 0600)
			case "removed":
				err = os.Remove(name)
			case "changed":
				err = os.WriteFile(name, []byte("changed"), 0600)
			case "symlink":
				err = os.Symlink(name, filepath.Join(env.ConfigDir(), "link"))
			}
			if err != nil {
				t.Fatal(err)
			}
			files, err := env.Files(context.Background())
			requireConfigError(t, err)
			if files != nil {
				t.Fatal("partial manifest returned")
			}
		})
	}
}

func TestEmptySelectionAndCancellation(t *testing.T) {
	cfg := config(t)
	cfg.Files = nil
	env, err := harnessenv.Create(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Close() })
	files, err := env.Files(context.Background())
	if err != nil || files == nil || len(files) != 0 {
		t.Fatalf("empty selection=%v %v", files, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.Files(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("manifest lost cancellation:", err)
	}
	if result, err := harnessenv.Create(ctx, cfg); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("creation lost cancellation:", err)
	}
}

func TestWorkingCopyIsIndependentAndUsesRecordedManifest(t *testing.T) {
	ctx := context.Background()
	cfg := config(t)
	original, err := harnessenv.Create(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })
	first, err := original.WorkingCopy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := original.WorkingCopy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if first.ConfigDir() == original.ConfigDir() || first.ConfigDir() == second.ConfigDir() ||
		filepath.Dir(first.ConfigDir()) != cfg.Root || first.Env()["CODEX_HOME"] != first.ConfigDir() {
		t.Fatal("copy is not an execution-local sibling")
	}
	want, err := original.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := first.Files(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("copy manifest=%v, err=%v; want=%v", got, err, want)
	}
	for _, file := range want {
		source, err := os.Stat(filepath.Join(original.ConfigDir(), file.Path))
		if err != nil {
			t.Fatal(err)
		}
		copy, err := os.Stat(filepath.Join(first.ConfigDir(), file.Path))
		if err != nil || os.SameFile(source, copy) || copy.Mode().Perm() != 0600 {
			t.Fatalf("copy shares inode or has unsafe permissions: %v %v", copy, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first.ConfigDir(), "config.toml"), []byte("CLI state"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Files(ctx); err == nil {
		t.Fatal("copy accepted changed bytes against recorded hashes")
	}
	for _, env := range []*harnessenv.Environment{original, second} {
		got, err := env.Files(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("copy mutation changed another environment: %v %v", got, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.ConfigDir()); !os.IsNotExist(err) {
		t.Fatal("copy survived Close")
	}
	if _, err := original.Files(ctx); err != nil {
		t.Fatal("closing copy affected original:", err)
	}
}

func TestWorkingCopyRejectsChangedSourceWithoutLeavingCopy(t *testing.T) {
	cfg := config(t)
	original, err := harnessenv.Create(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })
	if err := os.WriteFile(filepath.Join(original.ConfigDir(), "config.toml"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	copy, err := original.WorkingCopy(context.Background())
	requireConfigError(t, err)
	entries, readErr := os.ReadDir(cfg.Root)
	if copy != nil || readErr != nil || len(entries) != 1 {
		t.Fatalf("failed copy was retained: %v %v %v", copy, entries, readErr)
	}
}

func TestEnvironmentRejectsReplacementAndCleansMovedIdentity(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink", "missing"} {
		t.Run(replacement, func(t *testing.T) {
			cfg := config(t)
			original, err := harnessenv.Create(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := original.Close(); err != nil {
					t.Error(err)
				}
			})
			copy, err := original.WorkingCopy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := copy.Close(); err != nil {
					t.Error(err)
				}
			})
			nested := filepath.Join(cfg.Root, "moved")
			if err := os.Mkdir(nested, 0700); err != nil {
				t.Fatal(err)
			}
			displaced := filepath.Join(nested, "copy")
			if err := os.Rename(copy.ConfigDir(), displaced); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "directory":
				if err := os.Mkdir(copy.ConfigDir(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(copy.ConfigDir(), "unrelated"), []byte("retain"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(original.ConfigDir(), copy.ConfigDir()); err != nil {
					t.Fatal(err)
				}
			}
			_, err = copy.Files(context.Background())
			requireConfigError(t, err)
			if err := copy.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(displaced); !os.IsNotExist(err) {
				t.Fatalf("generated inode retained after rename: %v", err)
			}
			if replacement == "directory" {
				if data, err := os.ReadFile(filepath.Join(copy.ConfigDir(), "unrelated")); err != nil || string(data) != "retain" {
					t.Fatalf("deleted unrelated replacement: %s %v", data, err)
				}
			}
			if _, err := original.Files(context.Background()); err != nil {
				t.Fatal("cleanup followed symlink or changed original:", err)
			}
		})
	}
}
