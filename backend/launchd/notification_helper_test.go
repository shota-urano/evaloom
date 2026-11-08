package launchd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests compile the Swift notification helper's URL allowlist
// (spec 12 3.6, Q77). They need macOS and the Swift compiler (Xcode Command
// Line Tools); elsewhere they are skipped.

func requireSwift(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the notification helper is macOS only")
	}
	if err := exec.Command("xcrun", "--find", "swiftc").Run(); err != nil {
		t.Skipf("swiftc is not available: %v", err)
	}
}

func swiftc(t *testing.T, out string, frameworks []string, sources ...string) {
	t.Helper()
	args := []string{"swiftc"}
	for _, f := range frameworks {
		args = append(args, "-framework", f)
	}
	args = append(args, "-module-cache-path", filepath.Join(t.TempDir(), "module-cache"))
	args = append(args, sources...)
	args = append(args, "-o", out)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("swiftc: %v\n%s", err, output)
	}
}

// buildPolicyChecker compiles the helper's URLPolicy.swift with the test-only
// checker in testdata, so the cases below run against the helper's own code.
func buildPolicyChecker(t *testing.T) string {
	t.Helper()
	requireSwift(t)
	out := filepath.Join(t.TempDir(), "url-policy-check")
	swiftc(t, out, []string{"UserNotifications"},
		filepath.Join("notification-helper", "URLPolicy.swift"),
		filepath.Join("testdata", "url-policy-check", "main.swift"))
	return out
}

func runChecker(t *testing.T, checker string, args ...string) []string {
	t.Helper()
	output, err := exec.Command(checker, args...).Output()
	if err != nil {
		t.Fatalf("checker %q: %v", args, err)
	}
	return strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
}

var (
	acceptedURLs = []string{
		"http://127.0.0.1:8080/tasks/task-001",
		"http://127.0.0.1:1/tasks/t",
		"http://127.0.0.1:65535/tasks/t",
		"http://127.0.0.1:8080/",
		// The exact task URL evaloomd builds for an escaped task ID
		// (internal/notification TestMacOSOpenArgumentsAndResult).
		"http://127.0.0.1:49123/tasks/task%20%2F%3F%23%E6%97%A5%E6%9C%AC%E8%AA%9E",
		// The Dashboard URL evaloomd passes for the Linear polling alert
		// (internal/notification TestMacOSLinearPollAlertShowsOnlyClassAndCountAndOpensDashboard).
		"http://127.0.0.1:49123/",
	}
	rejectedURLs = []string{
		"http://127.0.0.1:8080/tasks/",              // empty taskId
		"http://127.0.0.1:8080/tasks",               // other path
		"http://127.0.0.1:8080/tasks/a/b",           // other path
		"http://127.0.0.1:8080/settings",            // other path
		"http://127.0.0.1:8080/api/settings",        // other path
		"http://127.0.0.1:8080",                     // empty path
		"https://127.0.0.1:8080/",                   // scheme
		"https://127.0.0.1:8080/tasks/t",            // scheme
		"file:///tasks/t",                           // scheme
		"http://localhost:8080/",                    // host alias
		"http://localhost:8080/tasks/t",             // host alias
		"http://127.0.0.2:8080/tasks/t",             // other host
		"http://[::1]:8080/",                        // other host
		"http://127.0.0.1/",                         // no port
		"http://127.0.0.1/tasks/t",                  // no port
		"http://127.0.0.1:/tasks/t",                 // empty port
		"http://127.0.0.1:0/tasks/t",                // port 0
		"http://127.0.0.1:65536/tasks/t",            // port 65536
		"http://owner@127.0.0.1:8080/tasks/t",       // user name
		"http://owner:secret@127.0.0.1:8080/",       // user name and password
		"http://@127.0.0.1:8080/",                   // empty user name
		"http://127.0.0.1:8080/tasks/t?x=1",         // query
		"http://127.0.0.1:8080/?",                   // empty query
		"http://127.0.0.1:8080/tasks/t#frag",        // fragment
		"http://127.0.0.1:8080/#",                   // empty fragment
		"",                                          // empty
		"not a url",                                 // not a URL
		"http://127.0.0.1:8080/tasks/t\nhttps://x/", // trailing garbage
	}
)

func TestNotificationHelperURLAllowlist(t *testing.T) {
	checker := buildPolicyChecker(t)
	got := runChecker(t, checker, append([]string{"url"}, acceptedURLs...)...)
	for i, url := range acceptedURLs {
		if got[i] != "accept "+url {
			t.Errorf("%q = %q; want accepted unchanged", url, got[i])
		}
	}
	got = runChecker(t, checker, append([]string{"url"}, rejectedURLs...)...)
	if len(got) != len(rejectedURLs) {
		t.Fatalf("checker output %q", got)
	}
	for i, url := range rejectedURLs {
		if got[i] != "reject" {
			t.Errorf("%q = %q; want rejected", url, got[i])
		}
	}
}

func TestNotificationHelperLaunchArguments(t *testing.T) {
	checker := buildPolicyChecker(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"件名", "本文", "http://127.0.0.1:8080/tasks/t"}, "accept http://127.0.0.1:8080/tasks/t"},
		{[]string{"件名", "本文", "http://127.0.0.1:8080/"}, "accept http://127.0.0.1:8080/"},
		{[]string{"件名", "本文", "http://localhost:8080/"}, "invalid_arguments"},
		{[]string{"件名", "本文", "http://127.0.0.1:8080/settings"}, "invalid_arguments"},
		{[]string{"件名", "http://127.0.0.1:8080/"}, "invalid_arguments"},
		{[]string{"件名", "本文", "http://127.0.0.1:8080/", "extra"}, "invalid_arguments"},
	} {
		if got := runChecker(t, checker, append([]string{"launch"}, tc.args...)...); len(got) != 1 || got[0] != tc.want {
			t.Errorf("launch %q = %q; want %q", tc.args, got, tc.want)
		}
	}
}

// A click re-checks the URL stored in the notification; only an allowed URL
// with the default action is opened.
func TestNotificationHelperClickRechecksStoredURL(t *testing.T) {
	checker := buildPolicyChecker(t)
	for _, url := range acceptedURLs {
		if got := runChecker(t, checker, "click", "default", url); got[0] != "open "+url {
			t.Errorf("click %q = %q; want opened", url, got)
		}
		if got := runChecker(t, checker, "click", "dismiss", url); got[0] != "none" {
			t.Errorf("dismiss %q = %q; want nothing opened", url, got)
		}
	}
	for _, url := range rejectedURLs {
		if got := runChecker(t, checker, "click", "default", url); got[0] != "none" {
			t.Errorf("click %q = %q; want nothing opened", url, got)
		}
	}
	if got := runChecker(t, checker, "click-non-string"); got[0] != "none" {
		t.Errorf("non-string stored URL = %q; want nothing opened", got)
	}
}

// Builds the deployed helper exactly as backend/launchd/README.md does (minus
// signing and deployment) and runs it with rejected URLs. The helper must
// report invalid_arguments and exit before asking for notification
// permission, so no notification is posted. Accepted URLs are not run here
// because they would post a real notification.
func TestNotificationHelperRejectsInvalidURLWithoutNotifying(t *testing.T) {
	requireSwift(t)
	dir := t.TempDir()
	app := filepath.Join(dir, "Evaloom Notifications.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(filepath.Join("notification-helper", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), plist, 0o644); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(app, "Contents", "MacOS", "evaloom-notification-helper")
	swiftc(t, helper, []string{"AppKit", "UserNotifications"},
		filepath.Join("notification-helper", "URLPolicy.swift"),
		filepath.Join("notification-helper", "main.swift"))
	for _, url := range []string{
		"http://127.0.0.1:8080/tasks/", "http://127.0.0.1:8080/settings", "https://127.0.0.1:8080/",
		"http://localhost:8080/", "http://127.0.0.2:8080/", "http://127.0.0.1/", "http://127.0.0.1:0/",
		"http://127.0.0.1:65536/", "http://owner@127.0.0.1:8080/", "http://127.0.0.1:8080/?x=1",
		"http://127.0.0.1:8080/#x",
	} {
		t.Run(url, func(t *testing.T) {
			result := filepath.Join(t.TempDir(), "result")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, helper, "evaloom: 完了", "件名\ntask-001\nsucceeded", url)
			cmd.Env = append(os.Environ(), "EVALOOM_NOTIFICATION_RESULT="+result)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("helper: %v\n%s", err, output)
			}
			got, err := os.ReadFile(result)
			if errors.Is(err, os.ErrNotExist) || string(got) != "invalid_arguments" {
				t.Fatalf("result = %q, %v; want invalid_arguments", got, err)
			}
		})
	}
}
