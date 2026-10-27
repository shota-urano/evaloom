package notification

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestMacOSContentAllowsOnlyTitleIDAndClassification(t *testing.T) {
	for _, tc := range []struct {
		status model.ExecutionStatus
		title  string
		class  string
	}{
		{model.ExecutionSucceeded, "evaloom: 完了", "succeeded"},
		{model.ExecutionFailed, "evaloom: 失敗", "test_failed"},
		{model.ExecutionInterrupted, "evaloom: 失敗", "test_failed"},
		{model.ExecutionTimedOut, "evaloom: 失敗", "test_failed"},
		{model.ExecutionBlocked, "evaloom: 対応が必要", "gate1_test_change"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			n := Notification{
				TaskID: "task-001", Title: "件名 $(touch /tmp/should-not-exist)\n引用 \"", Status: tc.status,
				ErrorClass: model.ErrorTestFailed, BlockCause: model.BlockGate1TestChange,
				ExecutionID: "PRIVATE-EXECUTION", PRURL: "https://private.example/pr/1",
				Reason: "PRIVATE-REASON secret-token log context task-body", Stage: "PRIVATE-STAGE",
			}
			title, body, err := macOSContent(n)
			if err != nil || title != tc.title || body != n.Title+"\n"+n.TaskID+"\n"+tc.class {
				t.Fatalf("content = %q %q, %v", title, body, err)
			}
			// Even malformed typed classifications must not expose free-form text.
			n.ErrorClass = "SECRET-ERROR"
			n.BlockCause = "SECRET-BLOCK"
			_, body, err = macOSContent(n)
			if err != nil || body != n.Title+"\n"+n.TaskID+"\n"+string(n.Status) {
				t.Fatalf("unsafe classification fallback = %q, %v", body, err)
			}
		})
	}
}

func TestMacOSOpenArgumentsAndResult(t *testing.T) {
	n := Notification{TaskID: "task /?#日本語", Title: "件名 ' \" $HOME;\n", Status: model.ExecutionBlocked, BlockCause: model.BlockContextConflict}
	m := NewMacOS("/Users/owner/Applications/Evaloom Notifications.app", 49123)
	var resultPath string
	// Prove each launch replaces an inherited stale result path.
	t.Setenv(resultEnvironment, "/unused/stale-result")
	m.run = func(cmd *exec.Cmd) error {
		want := []string{"/usr/bin/open", "-n", "-a", m.helperPath, "--args", "evaloom: 対応が必要",
			n.Title + "\n" + n.TaskID + "\ncontext_conflict",
			"http://127.0.0.1:49123/tasks/task%20%2F%3F%23%E6%97%A5%E6%9C%AC%E8%AA%9E"}
		if cmd.Path != "/usr/bin/open" || !reflect.DeepEqual(cmd.Args, want) {
			t.Fatalf("command = %q %q; want %q", cmd.Path, cmd.Args, want)
		}
		resultPath = helperResultPath(t, cmd)
		if resultPath == "/unused/stale-result" {
			t.Fatal("used stale inherited result path")
		}
		info, err := os.Stat(filepath.Dir(resultPath))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("result directory not private: %v %v", info, err)
		}
		return os.WriteFile(resultPath, []byte("sent"), 0o600)
	}
	if err := m.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(resultPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper result directory leaked: %v", err)
	}
}

func helperResultPath(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	var path string
	for _, env := range cmd.Environ() {
		if value, ok := strings.CutPrefix(env, resultEnvironment+"="); ok {
			path = value
		}
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("missing absolute helper result path: %q", path)
	}
	return path
}

func TestMacOSReturnsLaunchAndHelperErrors(t *testing.T) {
	n := Notification{TaskID: "task-001", Title: "Title", Status: model.ExecutionFailed}
	t.Run("launch failure", func(t *testing.T) {
		m := NewMacOS("/Applications/missing.app", 8080)
		launchErr := errors.New("launch failed")
		var resultPath string
		m.run = func(cmd *exec.Cmd) error {
			resultPath = helperResultPath(t, cmd)
			return launchErr
		}
		if err := m.Notify(context.Background(), n); !errors.Is(err, launchErr) || !errors.Is(err, ErrLaunchFailed) {
			t.Fatalf("launch error = %v", err)
		}
		if _, err := os.Stat(filepath.Dir(resultPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed launch leaked directory: %v", err)
		}
	})
	for _, code := range []string{"permission_denied", "authorization_failed", "registration_failed", "invalid_arguments", "timed_out", "", "PRIVATE-HELPER-OUTPUT"} {
		t.Run(code, func(t *testing.T) {
			m := NewMacOS("/Applications/helper.app", 8080)
			m.run = func(cmd *exec.Cmd) error {
				return os.WriteFile(helperResultPath(t, cmd), []byte(code), 0o600)
			}
			err := m.Notify(context.Background(), n)
			if err == nil || strings.Contains(err.Error(), "PRIVATE-HELPER-OUTPUT") {
				t.Fatalf("helper error = %v", err)
			}
			wantReason := "other"
			if code == "permission_denied" {
				wantReason = "permission_denied"
				if !errors.Is(err, ErrPermissionDenied) {
					t.Fatalf("permission denied not typed: %v", err)
				}
			} else if code == "timed_out" {
				wantReason = "timeout"
				if !errors.Is(err, ErrTimeout) {
					t.Fatalf("helper timeout not typed: %v", err)
				}
			}
			if got := failureReason(err); got != wantReason {
				t.Fatalf("helper reason = %q; want %q", got, wantReason)
			}
		})
	}
}

func TestMacOSDoesNotTreatOpenSuccessAsDelivery(t *testing.T) {
	m := NewMacOS("/Applications/helper.app", 8080)
	m.timeout = 30 * time.Millisecond
	m.run = func(cmd *exec.Cmd) error { return nil }
	err := m.Notify(context.Background(), Notification{TaskID: "task-001", Status: model.ExecutionSucceeded})
	if !errors.Is(err, context.DeadlineExceeded) || failureReason(err) != "timeout" {
		t.Fatalf("open without helper response = %v; want deadline", err)
	}
}

func TestMacOSWaitCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMacOS("/Applications/helper.app", 8080)
	m.run = func(cmd *exec.Cmd) error { cancel(); return nil }
	if err := m.Notify(ctx, Notification{TaskID: "task-001", Status: model.ExecutionSucceeded}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestMacOSRejectsInvalidInputWithoutLaunching(t *testing.T) {
	for _, tc := range []struct {
		path   string
		port   int
		id     string
		status model.ExecutionStatus
	}{
		{"relative.app", 8080, "task", model.ExecutionSucceeded},
		{"/helper.app", 0, "task", model.ExecutionSucceeded},
		{"/helper.app", 65536, "task", model.ExecutionSucceeded},
		{"/helper.app", 8080, "", model.ExecutionSucceeded},
		{"/helper.app", 8080, "task", model.ExecutionCancelled},
		{"/helper.app", 8080, "task", model.ExecutionRunning},
		{"/helper.app", 8080, "task", "SECRET-STATUS"},
	} {
		m := NewMacOS(tc.path, tc.port)
		m.run = func(cmd *exec.Cmd) error { t.Fatal("invalid input launched helper"); return nil }
		if err := m.Notify(context.Background(), Notification{TaskID: tc.id, Status: tc.status}); err == nil {
			t.Fatalf("accepted invalid input: %+v", tc)
		}
	}
}
