package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

func workingSettings(t *testing.T, root string) *harnessenv.Environment {
	t.Helper()
	commit := strings.Repeat("a", 40)
	var files []gitcontext.File
	for _, pair := range [][2]string{{"config.toml", "pinned auth"}, {"skills/check/SKILL.md", "日本語\x00\xff\r\n"}} {
		files = append(files, gitcontext.File{Path: pair[0], Content: pair[1], ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(pair[1]))), SourceCommitSHA: commit})
	}
	env, err := harnessenv.Create(context.Background(), harnessenv.Config{Root: root, CommitSHA: commit, ConfigVariable: "CODEX_HOME", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Close(); err != nil {
			t.Error(err)
		}
	})
	return env
}

func TestWorkingConfigurationTransfersOnlyVerifiedBytes(t *testing.T) {
	env := workingSettings(t, t.TempDir())
	ctx, closeConfig, err := WithWorkingConfiguration(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeConfig(); err != nil {
			t.Error(err)
		}
	}()
	w := ctx.Value(configurationKey{}).(*workingConfiguration)
	want := map[string]string{"config.toml": "pinned auth", "skills/check/SKILL.md": "日本語\x00\xff\r\n"}
	got := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(w.archive))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Mode != 0700 {
				t.Fatal("unsafe directory permissions")
			}
		case tar.TypeReg:
			if header.Mode != 0600 {
				t.Fatal("unsafe file permissions")
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			got[header.Name] = string(data)
		default:
			t.Fatalf("unexpected archive member: %+v", header)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transfer=%v want=%v", got, want)
	}
	pinned := bytes.Clone(w.archive)
	if err := os.Chmod(filepath.Join(env.ConfigDir(), "config.toml"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.ConfigDir(), "config.toml"), []byte("unrecorded bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.archive, pinned) {
		t.Fatal("host mutation changed verified transfer")
	}
	if err := w.verify(ctx, SandboxSpec{Kind: Harness, ConfigDir: env.ConfigDir(), ConfigWritable: true}); err == nil {
		t.Fatal("changed staging config accepted")
	}
}

func TestWorkingConfigurationPreservesMountSafetyChecks(t *testing.T) {
	for _, protected := range []string{"home", "data", "token", "outside-root", "overlap-worktree"} {
		t.Run(protected, func(t *testing.T) {
			f := newFixture(t, "normal")
			env := workingSettings(t, f.config.ConfigDirRoot)
			ctx, closeConfig, err := WithWorkingConfiguration(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := closeConfig(); err != nil {
					t.Error(err)
				}
			}()
			f.spec.ConfigDir, f.spec.ConfigWritable = env.ConfigDir(), true
			switch protected {
			case "home":
				f.config.HostHome = env.ConfigDir()
			case "data":
				f.config.DataDir = env.ConfigDir()
			case "token":
				f.config.RunnerTokenFile = filepath.Join(env.ConfigDir(), "config.toml")
			case "outside-root":
				f.config.ConfigDirRoot = f.config.WorktreeRoot
			case "overlap-worktree":
				f.spec.Worktree = env.ConfigDir()
				f.config.WorktreeRoot = f.config.ConfigDirRoot
			}
			p, err := f.sandbox(t).RunControlled(ctx, f.spec, []string{"/bin/true"})
			if err == nil || p != nil || len(f.calls(t)) != 0 {
				t.Fatalf("unsafe config reached runtime: p=%v err=%v calls=%v", p, err, f.calls(t))
			}
		})
	}
}

func TestWritableConfigurationRequiresVerifiedCapability(t *testing.T) {
	f := newFixture(t, "normal")
	f.spec.ConfigWritable = true
	p, err := f.sandbox(t).RunControlled(context.Background(), f.spec, []string{"/bin/true"})
	if err == nil || p != nil || len(f.calls(t)) != 0 {
		t.Fatalf("unverified writable config reached runtime: p=%v err=%v calls=%v", p, err, f.calls(t))
	}
}

func TestMountedConfigurationRejectsManifestMismatch(t *testing.T) {
	env := workingSettings(t, t.TempDir())
	ctx, closeConfig, err := WithWorkingConfiguration(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeConfig(); err != nil {
			t.Error(err)
		}
	}()
	w := ctx.Value(configurationKey{}).(*workingConfiguration)
	if err := w.verifyArchive(w.archive); err != nil {
		t.Fatal("valid manifest rejected:", err)
	}
	for _, mode := range []string{"changed", "missing", "extra", "symlink", "hardlink", "duplicate", "traversal", "invalid_archive"} {
		t.Run(mode, func(t *testing.T) {
			files, err := env.Files(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var data bytes.Buffer
			tw := tar.NewWriter(&data)
			if mode == "extra" || mode == "traversal" {
				files = append(files, gitcontext.File{Path: "unselected", Content: "extra"})
			}
			if mode == "duplicate" {
				files = append(files, files[0])
			}
			for i, file := range files {
				if i == 0 && mode == "missing" {
					continue
				}
				if i == 0 && mode == "changed" {
					file.Content = "unrecorded"
				}
				if mode == "traversal" && i == len(files)-1 {
					file.Path = "../outside"
				}
				header := &tar.Header{Name: file.Path, Mode: 0600, Typeflag: tar.TypeReg, Size: int64(len(file.Content))}
				if i == 0 && (mode == "symlink" || mode == "hardlink") {
					header.Typeflag, header.Size, header.Linkname, file.Content = tar.TypeSymlink, 0, "outside", ""
					if mode == "hardlink" {
						header.Typeflag = tar.TypeLink
					}
				}
				if err := tw.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write([]byte(file.Content)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			payload := data.Bytes()
			if mode == "invalid_archive" {
				payload = []byte("not a tar archive")
			}
			var failure *harnesscontext.ConfigError
			if err := w.verifyArchive(payload); !errors.As(err, &failure) {
				t.Fatalf("want config_error, got %v", err)
			}
		})
	}
}
