package harnessenv

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"golang.org/x/sys/unix"
)

func authCopy(t *testing.T, variable string) (*Environment, *Authentication, string) {
	t.Helper()
	root := t.TempDir()
	store := filepath.Join(root, "credential")
	if err := os.WriteFile(store, []byte(`{"token":"private-original"}`), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := Create(context.Background(), Config{Root: root, CommitSHA: strings.Repeat("a", 40), ConfigVariable: variable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Close() })
	working, err := env.WorkingCopy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = working.Close() })
	auth, err := AcquireAuthentication(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auth.Close() })
	if err := auth.Install(working); err != nil {
		t.Fatal(err)
	}
	return working, auth, store
}

func TestAuthenticationValidationAndPreservation(t *testing.T) {
	for _, mode := range []string{"malformed", "array", "null", "scalar", "trailing", "oversize", "symlink", "directory", "fifo", "missing", "valid", "limit", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			e, _, store := authCopy(t, "CODEX_HOME")
			target := filepath.Join(e.ConfigDir(), "auth.json")
			original, _ := os.ReadFile(store)
			before, _ := os.Stat(store)
			data := []byte(`{"token":"private-updated"}`)
			switch mode {
			case "malformed":
				data = []byte(`{"token":"private-parser-error"`)
			case "array":
				data = []byte(`[]`)
			case "null":
				data = []byte(`null`)
			case "scalar":
				data = []byte(`"private-scalar"`)
			case "trailing":
				data = []byte(`{} {}`)
			case "oversize":
				data = []byte(`{"token":"` + strings.Repeat("x", AuthenticationLimit) + `"}`)
			case "limit":
				data = []byte(`{"token":"` + strings.Repeat("x", AuthenticationLimit-len(`{"token":""}`)) + `"}`)
			case "unchanged":
				data = original
			}
			switch mode {
			case "symlink", "directory", "fifo", "missing":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				var err error
				if mode == "symlink" {
					err = os.Symlink(store, target)
				}
				if mode == "directory" {
					err = os.Mkdir(target, 0700)
				}
				if mode == "fifo" {
					err = unix.Mkfifo(target, 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := e.WriteBackAuthentication(nil)
			got, readErr := os.ReadFile(store)
			if readErr != nil {
				t.Fatal(readErr)
			}
			valid := mode == "valid" || mode == "limit" || mode == "unchanged"
			if valid {
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("valid writeback failed: %v", err)
				}
			} else {
				if err == nil || !bytes.Equal(got, original) {
					t.Fatal("invalid credential changed store")
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("credential in diagnostic")
				}
			}
			after, _ := os.Stat(store)
			if after.Mode().Perm() != 0600 {
				t.Fatal("unsafe stored permissions")
			}
			if mode == "unchanged" && !os.SameFile(before, after) {
				t.Fatal("unchanged credential replaced")
			}
		})
	}
}

func TestAuthenticationAtomicWriteFailures(t *testing.T) {
	for _, mode := range []string{"partial", "sync", "rename"} {
		t.Run(mode, func(t *testing.T) {
			_, a, store := authCopy(t, "CODEX_HOME")
			before, _ := os.ReadFile(store)
			name := a.name
			if mode == "rename" {
				name = "absent/credential"
			}
			err := replaceAuthentication(a.root, name, []byte(`{"token":"private-new"}`), func(f *os.File, data []byte) error {
				if mode == "partial" {
					_, _ = f.Write(data[:5])
					return io.ErrShortWrite
				}
				if _, err := f.Write(data); err != nil {
					return err
				}
				if mode == "sync" {
					return errors.New("private-device-error")
				}
				return f.Sync()
			})
			if err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("write failure missing or leaked")
			}
			after, err := os.ReadFile(store)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("original unreadable after failed replacement")
			}
			entries, _ := os.ReadDir(filepath.Dir(store))
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".evaloom-auth-") {
					t.Fatal("temporary credential left behind")
				}
			}
		})
	}
}

func TestAuthenticationNeverEntersPinnedFiles(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Run(variable, func(t *testing.T) {
			e, _, _ := authCopy(t, variable)
			files, err := e.Files(context.Background())
			if err != nil || len(files) != 0 {
				t.Fatal("authentication entered pinned Files", err)
			}
			name, data, err := e.AuthenticationFile()
			if err != nil || name != authenticationName(variable) || len(data) == 0 {
				t.Fatal("authentication missing from transfer")
			}
		})
	}
	for _, name := range []string{"auth.json", ".credentials.json"} {
		data := `{"token":"private-profile"}`
		_, err := Create(context.Background(), Config{Root: t.TempDir(), CommitSHA: strings.Repeat("a", 40), ConfigVariable: "CODEX_HOME", Files: []gitcontext.File{{Path: name, Content: data, ContentHash: hash(data), SourceCommitSHA: strings.Repeat("a", 40)}}})
		if err == nil || strings.Contains(err.Error(), data) {
			t.Fatal("Profile authentication permitted or leaked")
		}
	}
}

func TestAuthenticationLockHelper(t *testing.T) {
	store := os.Getenv("EVALOOM_AUTH_LOCK_TEST")
	if store == "" {
		return
	}
	// Parent changed the stored contents to invalid JSON while holding the lock.
	// A read before acquiring flock would fail immediately, rather than time out.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	a, err := AcquireAuthentication(ctx, store)
	if a != nil {
		a.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		os.Exit(51)
	}
	os.Exit(0)
}

func TestAuthenticationOSLockPrecedesReadAndIsCancellable(t *testing.T) {
	e, a, store := authCopy(t, "CODEX_HOME")
	if err := os.WriteFile(store, []byte("invalid while locked"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestAuthenticationLockHelper$")
	cmd.Env = append(os.Environ(), "EVALOOM_AUTH_LOCK_TEST="+store, "GORACE=atexit_sleep_ms=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock failed: %v %s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() {
		lease, err := AcquireAuthentication(ctx, store)
		if lease != nil {
			lease.Close()
		}
		waiting <- err
	}()
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock wait ignored cancellation")
	}
	fresh := []byte(`{"token":"private-refreshed"}`)
	if err := e.WriteBackAuthentication(fresh); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireAuthentication(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if !bytes.Equal(next.initial, fresh) {
		t.Fatal("next lease read stale credentials")
	}
}
