package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startEvaloomd runs the daemon on an ephemeral port and returns its address
// and a function that stops it and waits for shutdown.
func startEvaloomd(t *testing.T, path string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	output := make(startupOutput, 1)
	stopped := make(chan error, 1)
	go func() { stopped <- run(ctx, []string{"-db", path, "-port", "0"}, output) }()
	var line string
	select {
	case line = <-output:
	case err := <-stopped:
		cancel()
		t.Fatalf("server exited before startup: %v", err)
	case <-ctx.Done():
		t.Fatal("server did not start")
	}
	address, ok := strings.CutPrefix(strings.Split(strings.TrimSpace(line), "\n")[0], "evaloomd: listening on ")
	if !ok {
		cancel()
		t.Fatalf("unexpected startup output: %q", line)
	}
	stop := func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(2 * shutdownTimeout):
			t.Error("run did not stop after cancellation")
		}
	}
	return address, stop
}

func ownerHTTP(t *testing.T, method, address, path, body string, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest(method, "http://"+address+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "http://"+address)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(raw)
}

// 受け入れ基準: Control Plane を再起動しても期限内のセッションで本人用 API を呼べる。
// The password is set with the CLI, the daemon is stopped and started again,
// and the cookie issued before the restart still reaches an owner API.
func TestOwnerSessionSurvivesControlPlaneRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	const password = "correct horse battery staple"
	if _, err := runOwnerCommand(t, path, []string{"set-password", "-id", "orion"}, password+"\n"+password+"\n"); err != nil {
		t.Fatal(err)
	}
	address, stop := startEvaloomd(t, path)
	login, body := ownerHTTP(t, "POST", address, "/auth/login", `{"id":"orion","password":"`+password+`"}`, nil)
	cookies := login.Cookies()
	if login.StatusCode != http.StatusOK || len(cookies) != 1 || !strings.Contains(body, `"csrfToken"`) {
		t.Fatalf("login: %d %s cookies=%v", login.StatusCode, body, cookies)
	}
	if response, body := ownerHTTP(t, "GET", address, "/api/runners", "", cookies[0]); response.StatusCode != http.StatusOK {
		t.Fatalf("before restart: %d %s", response.StatusCode, body)
	}
	stop()

	address, stop = startEvaloomd(t, path)
	defer stop()
	if response, body := ownerHTTP(t, "GET", address, "/api/runners", "", cookies[0]); response.StatusCode != http.StatusOK {
		t.Fatalf("after restart: %d %s", response.StatusCode, body)
	}
	if response, body := ownerHTTP(t, "GET", address, "/api/runners", "", &http.Cookie{Name: cookies[0].Name, Value: "unknown"}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown cookie after restart: %d %s", response.StatusCode, body)
	}
}
