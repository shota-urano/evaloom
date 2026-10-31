package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestPreparedBaselineReportNeverUsesPreviousOutput(t *testing.T) {
	path := t.TempDir()
	report := filepath.Join(path, "report.xml")
	if err := os.WriteFile(report, []byte("previous report"), 0600); err != nil {
		t.Fatal(err)
	}
	launcher := newPreparedBaselineSandbox(BaselineConfig{Shell: "/bin/sh", JUnitFormat: "junit"})
	request := BaselineTestRequest{Kind: sandbox.TestRun, WorktreePath: path, Command: "test command", ReportFormat: new("junit"), ReportPath: new("report.xml")}
	if _, _, err := launcher.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	// A successful workload that does not write its configured report must not
	// inherit setup's or a previous invocation's report.
	if file, err := launcher.OpenReport(context.Background(), request); err != nil || file != nil {
		t.Fatalf("previous output reused: %v %v", file, err)
	}
	if err := os.WriteFile(report, []byte("new report"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := launcher.OpenReport(context.Background(), request)
	if err != nil || file == nil {
		t.Fatalf("fresh report: %v %v", file, err)
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil || string(body) != "new report" {
		t.Fatalf("report=%q err=%v", body, err)
	}
}

func TestPreparedBaselineReportRejectsEscapesAndSymlinks(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	protected := filepath.Join(outside, "protected")
	if err := os.WriteFile(protected, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, filepath.Join(path, "report.xml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git", "config"), []byte("git metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(path, "git-alias")); err != nil {
		t.Fatal(err)
	}
	launcher := newPreparedBaselineSandbox(BaselineConfig{Shell: "/bin/sh", JUnitFormat: "junit"})
	for _, name := range []string{"../protected", protected, "escape/protected", ".git/config", "git-alias/config", "."} {
		request := BaselineTestRequest{Kind: sandbox.TestRun, WorktreePath: path, Command: "true", ReportFormat: new("junit"), ReportPath: &name}
		if _, _, err := launcher.Prepare(context.Background(), request); err == nil {
			t.Fatalf("unsafe report path accepted: %s", name)
		}
	}
	request := BaselineTestRequest{Kind: sandbox.TestRun, WorktreePath: path, Command: "true", ReportFormat: new("junit"), ReportPath: new("report.xml")}
	if file, err := launcher.OpenReport(context.Background(), request); err == nil {
		if file != nil {
			file.Close()
		}
		t.Fatal("symlink report accepted")
	}
	// Removing a stale symlink must remove the link, not its target.
	if _, _, err := launcher.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(protected); err != nil || string(body) != "unchanged" {
		t.Fatalf("outside file changed: %q %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(path, ".git", "config")); err != nil || string(body) != "git metadata" {
		t.Fatalf("Git metadata changed: %q %v", body, err)
	}
}
