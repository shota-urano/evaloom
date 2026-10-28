package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinishedRunnerLogsRegisterMatchingContentAndSHA256(t *testing.T) {
	for _, stream := range []string{"harness", "setup", "test"} {
		t.Run(stream, func(t *testing.T) {
			f := newLogControlPlane(t)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			log, err := process.NewLog(logExecution, stream, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { log.Finish(false) })
			if err := log.Read(strings.NewReader("output\r\n日本語\n" + process.token)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("registered before Finish: %d %v", count, err)
			}
			if err := log.Finish(false); err != nil {
				t.Fatal(err)
			}
			if err := await(t, startLogDelivery(t, log)); err != nil {
				t.Fatal(err)
			}
			want := "output\n日本語\n[REDACTED]\n"
			local, err := os.ReadFile(log.Path())
			if err != nil || string(local) != want {
				t.Fatalf("local=%q err=%v", local, err)
			}
			var path, hash, kind string
			var size int64
			var redacted int
			err = f.db.QueryRow(`SELECT storage_path,sha256,size_bytes,kind,redacted FROM artifacts WHERE execution_id=? AND id=?`, logExecution, log.id).Scan(&path, &hash, &size, &kind, &redacted)
			if err != nil {
				t.Fatal(err)
			}
			if !filepath.IsLocal(path) {
				t.Fatalf("storage_path=%q", path)
			}
			stored, err := os.ReadFile(filepath.Join(f.dataDir, path))
			sum := sha256.Sum256(local)
			if err != nil || string(stored) != want || hash != hex.EncodeToString(sum[:]) || size != int64(len(local)) || kind != stream+"_log" || redacted != 1 {
				t.Fatalf("stored=%q sha256=%s size=%d kind=%s redacted=%d err=%v", stored, hash, size, kind, redacted, err)
			}
		})
	}
}

func TestArtifactHTTPRetryAfterLostAcknowledgementKeepsOneRow(t *testing.T) {
	f := newLogControlPlane(t)
	f.loseArtifactAck.Store(true)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	log, err := process.NewLog(logExecution, "harness", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Read(strings.NewReader("log\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	if err := await(t, startLogDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	// Reopen the Control Plane DB and retry explicitly with the same identity.
	f.stop()
	f.start()
	if err := process.RegisterLogArtifact(context.Background(), LogArtifact{ID: log.id, ExecutionID: logExecution, Kind: "harness_log", Path: log.Path()}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	entries, err := os.ReadDir(filepath.Join(f.dataDir, "artifacts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("files=%d err=%v", len(entries), err)
	}
	if content, err := os.ReadFile(log.Path()); err != nil || string(content) != "log\n" {
		t.Fatalf("local log=%q err=%v", content, err)
	}
}
