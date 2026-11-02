package worktree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"golang.org/x/sys/unix"
)

func copyFixture(t *testing.T) (Repositories, string, string) {
	t.Helper()
	root := t.TempDir()
	source, destination := filepath.Join(root, "owner"), filepath.Join(root, "managed", "task-001")
	for _, dir := range []string{source, destination} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return Repositories{WorktreeRoot: filepath.Dir(destination)}, source, destination
}

func copyWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func noMissingCopy(t *testing.T) func(context.Context, string) error {
	t.Helper()
	return func(_ context.Context, path string) error {
		t.Errorf("unexpected missing source: %s", path)
		return nil
	}
}

func TestCopyFilesAllowlistOnlyAndSHA256(t *testing.T) {
	r, source, destination := copyFixture(t)
	files := map[string]string{".env": "unlisted dummy secret", "config/test.json": "dummy\x00bytes\n", "empty": "", "directory/unlisted": "not copied"}
	for path, contents := range files {
		copyWrite(t, filepath.Join(source, path), contents)
	}
	var warnings []string
	copied, err := r.CopyFiles(context.Background(), source, destination,
		[]string{"config/test.json", "empty", "./config/test.json", "directory", "missing/file"},
		func(_ context.Context, path string) error { warnings = append(warnings, path); return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []model.CopiedFile{
		{Path: "config/test.json", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(files["config/test.json"])))},
		{Path: "empty", SHA256: fmt.Sprintf("%x", sha256.Sum256(nil))},
	}
	if !reflect.DeepEqual(copied, want) || !reflect.DeepEqual(warnings, []string{"missing/file"}) {
		t.Fatalf("copied=%+v warnings=%v", copied, warnings)
	}
	for _, file := range copied {
		body, err := os.ReadFile(filepath.Join(destination, file.Path))
		if err != nil || string(body) != files[file.Path] || fmt.Sprintf("%x", sha256.Sum256(body)) != file.SHA256 {
			t.Fatalf("destination %s contents=%q err=%v", file.Path, body, err)
		}
	}
	for _, path := range []string{".env", "directory", "missing"} {
		if _, err := os.Lstat(filepath.Join(destination, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("excluded entry %s exists: %v", path, err)
		}
	}
}

func TestCopyFilesSkipsFileAndDirectorySymlinks(t *testing.T) {
	r, source, destination := copyFixture(t)
	outside := t.TempDir()
	copyWrite(t, filepath.Join(source, "regular", "file"), "inside")
	copyWrite(t, filepath.Join(outside, "file"), "outside")
	links := map[string]string{
		"file-internal": "regular/file", "file-external": filepath.Join(outside, "file"),
		"dir-internal": "regular", "dir-external": outside,
		"dangling-file": "absent", "dangling-dir": filepath.Join(outside, "absent"),
	}
	allowlist := []string{"regular/file"}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
		allowlist = append(allowlist, name, name+"/file")
	}
	if err := unix.Mkfifo(filepath.Join(source, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	allowlist = append(allowlist, "pipe")
	copied, err := r.CopyFiles(context.Background(), source, destination, allowlist, noMissingCopy(t))
	if err != nil || len(copied) != 1 || copied[0].Path != "regular/file" {
		t.Fatalf("copied=%+v err=%v", copied, err)
	}
	for name := range links {
		if _, err := os.Lstat(filepath.Join(destination, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink %s copied: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(destination, "pipe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("special file copied: %v", err)
	}
}

func TestCopyFilesRejectsDestinationSymlinksAndExistingFiles(t *testing.T) {
	for _, kind := range []string{"file-internal", "file-external", "dir-internal", "dir-external", "regular", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			r, source, destination := copyFixture(t)
			outside := t.TempDir()
			copyWrite(t, filepath.Join(outside, "file"), "original")
			copyWrite(t, filepath.Join(destination, "internal", "file"), "original")
			name, target := "copy", filepath.Join(outside, "file")
			switch kind {
			case "regular":
				copyWrite(t, filepath.Join(destination, name), "original")
			case "hardlink":
				if err := os.Link(target, filepath.Join(destination, name)); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "file-internal" {
					target = "internal/file"
				}
				if kind == "dir-internal" {
					target = "internal"
				}
				if kind == "dir-external" {
					target = outside
				}
				if err := os.Symlink(target, filepath.Join(destination, name)); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "dir-internal" || kind == "dir-external" {
				name += "/file"
			}
			copyWrite(t, filepath.Join(source, name), "replacement")
			copied, err := r.CopyFiles(context.Background(), source, destination, []string{name}, noMissingCopy(t))
			if err == nil || len(copied) != 0 {
				t.Fatalf("copied=%+v err=%v", copied, err)
			}
			for _, path := range []string{filepath.Join(destination, name), filepath.Join(outside, "file"), filepath.Join(destination, "internal", "file")} {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "original" {
					t.Fatalf("existing target changed %s: %q %v", path, body, err)
				}
			}
		})
	}
}

func TestCopyFilesRejectsInvalidPathsBeforeWriting(t *testing.T) {
	for _, path := range []string{"", ".", "..", "a/../file", "/absolute", "*.env", "a?", "a[b]", ".git", "a/.GiT/config", "a\\b", "a\x00b", "a\nb"} {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			r, source, destination := copyFixture(t)
			copyWrite(t, filepath.Join(source, "valid"), "allowed")
			_, err := r.CopyFiles(context.Background(), source, destination, []string{"valid", path}, noMissingCopy(t))
			entries, readErr := os.ReadDir(destination)
			if err == nil || readErr != nil || len(entries) != 0 {
				t.Fatalf("invalid path side effect: entries=%v err=%v read=%v", entries, err, readErr)
			}
		})
	}
}

func TestCopyFilesExplicitEnvAndCancellationAndWarningFailure(t *testing.T) {
	r, source, destination := copyFixture(t)
	copyWrite(t, filepath.Join(source, ".env"), "dummy-only=true")
	if files, err := r.CopyFiles(context.Background(), source, destination, []string{".env"}, noMissingCopy(t)); err != nil || len(files) != 1 {
		t.Fatalf("explicitly allowed dummy .env: %v %v", files, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.CopyFiles(ctx, source, destination, nil, noMissingCopy(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := r.CopyFiles(context.Background(), source, destination, []string{"missing", "next/file"},
		func(context.Context, string) error { return errors.New("warning delivery failed") }); err == nil {
		t.Fatal("lost warning must prevent success")
	}
	if _, err := r.CopyFiles(context.Background(), source, t.TempDir(), nil, noMissingCopy(t)); !errors.Is(err, ErrWorktreePath) {
		t.Fatalf("outside Worktree Root: %v", err)
	}
}
