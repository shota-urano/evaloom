package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestArtifactFilesRoundTripOutsideGitTracking(t *testing.T) {
	// Reproduce local use with this repository's actual ignore rules, rather
	// than assuming that an untracked file cannot accidentally be added to Git.
	repo := t.TempDir()
	ignore, err := os.ReadFile(filepath.Join("..", "..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), ignore, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	dataDir := filepath.Join(repo, "data")
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "evaloom.db")
	db := openRegistry(t, path)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	files := openArtifactStore(t, db, dataDir)
	ctx := context.Background()
	content := "setup output\n[REDACTED]\n日本語\x00"
	artifact, err := files.Save(ctx, "01K8ACW0000000000000000001", "execution-1", "setup_log", true, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(content))
	if artifact.SHA256 != hex.EncodeToString(hash[:]) || artifact.SizeBytes != int64(len(content)) || !artifact.Redacted {
		t.Fatalf("wrong artifact metadata: %#v", artifact)
	}
	if _, err := time.Parse(time.RFC3339, artifact.CreatedAt); err != nil || !strings.HasSuffix(artifact.CreatedAt, "Z") {
		t.Fatalf("artifact timestamp = %q, err = %v", artifact.CreatedAt, err)
	}
	if !filepath.IsLocal(artifact.StoragePath) || filepath.Dir(artifact.StoragePath) != "artifacts" {
		t.Fatalf("artifact path must be local to data/artifacts: %q", artifact.StoragePath)
	}
	actualPath := filepath.Join(dataDir, artifact.StoragePath)
	info, err := os.Stat(actualPath)
	if err != nil || info.Size() != artifact.SizeBytes || info.Mode().Perm() != 0600 {
		t.Fatalf("artifact file info = %v, err = %v", info, err)
	}
	for _, args := range [][]string{
		{"-C", repo, "check-ignore", "--no-index", "--", actualPath},
		{"-C", repo, "status", "--porcelain", "--untracked-files=all", "--", "data"},
	} {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		if args[2] == "status" && len(out) != 0 {
			t.Fatalf("runtime data appeared in git status: %s", out)
		}
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	files = openArtifactStore(t, db, dataDir)
	got, file, err := files.Open(ctx, "execution-1", artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	bytes, err := io.ReadAll(file)
	if err != nil || string(bytes) != content || !reflect.DeepEqual(got, artifact) {
		t.Fatalf("reopened artifact = %#v, content = %q, err = %v", got, bytes, err)
	}
	if _, file, err := files.Open(ctx, "other-execution", artifact.ID); !errors.Is(err, sql.ErrNoRows) || file != nil {
		t.Fatalf("lookup for different execution = %v, %v; want nil, ErrNoRows", file, err)
	}
}

func TestArtifactSaveFailureLeavesNoPartialData(t *testing.T) {
	db := openTaskTables(t)
	dataDir := t.TempDir()
	files := openArtifactStore(t, db, dataDir)
	ctx := context.Background()
	first, err := files.Save(ctx, "artifact-1", "execution-1", "diff", false, strings.NewReader("original"))
	if err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("reader failed")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, test := range []struct {
		name, id, execution, kind string
		ctx                       context.Context
		reader                    io.Reader
		wantErr                   error
	}{
		{"duplicate ID", first.ID, "execution-1", "diff", ctx, strings.NewReader("replacement"), nil},
		{"missing execution", "missing", "no-such-execution", "diff", ctx, strings.NewReader("bad reference"), nil},
		{"unknown kind", "bad-kind", "execution-1", "unknown", ctx, strings.NewReader("invalid kind"), nil},
		{"partial read", "read-error", "execution-1", "test_log", ctx, io.MultiReader(strings.NewReader("partial"), failingArtifactReader{readErr}), readErr},
		{"cancelled", "cancelled", "execution-1", "diff", cancelled, strings.NewReader("cancelled"), context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := files.Save(test.ctx, test.id, test.execution, test.kind, false, test.reader)
			if err == nil || got != (Artifact{}) || (test.wantErr != nil && !errors.Is(err, test.wantErr)) {
				t.Fatalf("Save = %#v, %v; want zero artifact and error %v", got, err, test.wantErr)
			}
			entries, err := os.ReadDir(filepath.Join(dataDir, "artifacts"))
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(first.StoragePath) {
				t.Fatalf("files after failed save = %v, err = %v; want original only", entries, err)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM artifacts").Scan(&count); err != nil || count != 1 {
				t.Fatalf("rows after failed save = %d, err = %v; want 1", count, err)
			}
		})
	}
	bytes, err := os.ReadFile(filepath.Join(dataDir, first.StoragePath))
	if err != nil || string(bytes) != "original" {
		t.Fatalf("duplicate ID overwrote original: %q, err = %v", bytes, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Save(ctx, "closed-db", "execution-1", "diff", false, strings.NewReader("data")); err == nil {
		t.Fatal("Save succeeded with closed database")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "artifacts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("files after database failure = %v, err = %v", entries, err)
	}
}

func TestArtifactOpenRejectsPathsOutsideDataDirectory(t *testing.T) {
	db := openTaskTables(t)
	root := t.TempDir()
	files := openArtifactStore(t, db, filepath.Join(root, "data"))
	artifact, err := files.Save(context.Background(), "artifact-1", "execution-1", "diff", false, strings.NewReader("safe"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "private")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "data", "artifacts", "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "../private", "artifacts/link"} {
		mustTaskExec(t, db, "UPDATE artifacts SET storage_path = ? WHERE id = ?", path, artifact.ID)
		if _, file, err := files.Open(context.Background(), "execution-1", artifact.ID); err == nil || file != nil {
			if file != nil {
				file.Close()
			}
			t.Fatalf("Open accepted escaped path %q: %v", path, err)
		}
	}
}

func openArtifactStore(t *testing.T, db *sql.DB, dataDir string) *ArtifactStore {
	t.Helper()
	files, err := NewArtifactStore(db, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return files
}

type failingArtifactReader struct{ err error }

func (r failingArtifactReader) Read([]byte) (int, error) { return 0, r.err }
