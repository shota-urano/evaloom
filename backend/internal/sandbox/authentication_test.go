package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

func TestAuthenticationArchiveRejectsUnsafeFiles(t *testing.T) {
	for _, mode := range []string{"valid", "malformed", "null", "oversize", "symlink", "hardlink", "directory", "path", "duplicate", "extra", "corrupt", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			data := []byte(`{"token":"private-volume-token"}`)
			header := &tar.Header{Name: "auth.json", Mode: 0600, Typeflag: tar.TypeReg, Size: int64(len(data))}
			switch mode {
			case "malformed":
				data = []byte(`{"private-token":`)
				header.Size = int64(len(data))
			case "null":
				data = []byte(`null`)
				header.Size = int64(len(data))
			case "oversize":
				data = bytes.Repeat([]byte("x"), harnessenv.AuthenticationLimit+1)
				header.Size = int64(len(data))
			case "symlink":
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "/outside"
				header.Size = 0
				data = nil
			case "hardlink":
				header.Typeflag = tar.TypeLink
				header.Linkname = "/outside"
				header.Size = 0
				data = nil
			case "directory":
				header.Typeflag = tar.TypeDir
				header.Size = 0
				data = nil
			case "path":
				header.Name = "../auth.json"
			}
			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write(data)
			if mode == "duplicate" || mode == "extra" {
				if mode == "extra" {
					header.Name = "runtime-state"
				}
				_ = tw.WriteHeader(header)
				_, _ = tw.Write(data)
			}
			_ = tw.Close()
			archive := buf.Bytes()
			if mode == "corrupt" {
				archive = []byte("private-error-data")
			}
			if mode == "truncated" {
				archive = archive[:512+len(data)-1]
			}
			got, err := authenticationArchive(bytes.NewReader(archive), "auth.json")
			if mode == "valid" {
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("valid archive rejected", err)
				}
			} else {
				if err == nil || strings.Contains(err.Error(), "private-") {
					t.Fatal("unsafe archive accepted or credential in error")
				}
			}
		})
	}
}

func TestAuthenticationIsSeparateFromConfigurationManifest(t *testing.T) {
	original := workingSettings(t, t.TempDir())
	working, err := original.WorkingCopy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer working.Close()
	store := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(store, []byte(`{"token":"private-initial"}`), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := harnessenv.AcquireAuthentication(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.Install(working); err != nil {
		t.Fatal(err)
	}
	ctx, closeConfig, err := WithWorkingConfiguration(context.Background(), working)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConfig()
	w := ctx.Value(configurationKey{}).(*workingConfiguration)
	if _, found := w.expected["auth.json"]; found {
		t.Fatal("authentication hash entered pinned manifest")
	}
	if err := w.verifyArchive(w.archive); err != nil {
		t.Fatal(err)
	}
	// Contents are validated as credentials, never compared against a pinned hash.
	tr := tar.NewReader(bytes.NewReader(w.archive))
	var replaced bytes.Buffer
	tw := tar.NewWriter(&replaced)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		if h.Name == "auth.json" {
			data = []byte(`{"token":"different-valid-token"}`)
			h.Size = int64(len(data))
		}
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	if err := w.verifyArchive(replaced.Bytes()); err != nil {
		t.Fatal("credential contents used as configuration hash", err)
	}
	// The host-copy fallback observes validation failures without changing result.
	if err := os.WriteFile(filepath.Join(working.ConfigDir(), "auth.json"), []byte(`{"token":"private-refreshed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := WriteBackAuthentication(cancelled); err != nil {
		t.Fatal("writeback lost on cancellation", err)
	}
	got, err := os.ReadFile(store)
	if err != nil || string(got) != `{"token":"private-refreshed"}` {
		t.Fatal("host fallback not written back", err)
	}
}
