package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// labelPollClient reports the trigger label of each list query.
type labelPollClient struct{ labels chan string }

func (c labelPollClient) CheckKey() error { return nil }

func (c labelPollClient) ListReadyIssues(_ context.Context, _, label string) ([]linear.Issue, error) {
	select {
	case c.labels <- label:
	default:
	}
	return nil, nil
}

// lockedBuffer collects the daemon's output and logs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// daemon is one evaloomd run on path with a logged-in owner client.
type daemon struct {
	address, csrf string
	client        *http.Client
	output        *lockedBuffer
	poller        *linearintake.Poller
	labels        chan string
	stop          func()
}

func startDaemon(t *testing.T, path string) *daemon {
	t.Helper()
	configureTestOwner(t, path)
	d := &daemon{output: &lockedBuffer{}, labels: make(chan string, 1)}
	original := newLinearPoller
	newLinearPoller = func(db *sql.DB, logger *slog.Logger, _ int) (*linearintake.Poller, error) {
		// An interval far longer than the test isolates the startup poll.
		d.poller = &linearintake.Poller{DB: db, Client: labelPollClient{d.labels}, Logger: logger, Interval: time.Hour}
		return d.poller, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, d.output) }()
	stopped := false
	d.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
		newLinearPoller = original
	}
	t.Cleanup(d.stop)
	for d.address == "" {
		for _, line := range strings.Split(d.output.String(), "\n") {
			if address, ok := strings.CutPrefix(line, "evaloomd: listening on "); ok {
				d.address = address
			}
		}
		if d.address != "" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("run exited before startup: %v", err)
		case <-ctx.Done():
			t.Fatal("evaloomd did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	d.client = &http.Client{Transport: transport, Timeout: 2 * time.Second, Jar: jar}
	r, err := http.NewRequest("POST", "http://"+d.address+"/auth/login", strings.NewReader(testOwnerLoginBody))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "http://"+d.address)
	response, err := d.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var session struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil || response.StatusCode != 200 {
		t.Fatalf("login status=%d err=%v", response.StatusCode, err)
	}
	d.csrf = session.CSRFToken
	return d
}

// polledLabel waits for the startup poll and returns its trigger label.
func (d *daemon) polledLabel(t *testing.T) string {
	t.Helper()
	select {
	case label := <-d.labels:
		return label
	case <-time.After(5 * time.Second):
		t.Fatal("evaloomd did not poll Linear at startup")
		return ""
	}
}

func (d *daemon) request(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, "http://"+d.address+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://"+d.address)
		request.Header.Set("X-CSRF-Token", d.csrf)
	}
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func newLinearDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	setupLinearProject(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// PATCH /api/settings stores the values in settings, so after a Control
// Plane restart GET /api/settings serves them and the Linear Poller queries
// with them (spec 15 3.4, 5章, spec 02 3.4, Q76).
func TestSettingsSurviveRestartForAPIAndPoller(t *testing.T) {
	path := newLinearDB(t)

	first := startDaemon(t, path)
	if label := first.polledLabel(t); label != "ai-ready" {
		t.Fatalf("first run polled with %q; want the default", label)
	}
	status, body := first.request(t, "PATCH", "/api/settings", `{"linear":{"triggerLabel":"go","routingLabelPrefix":"to:"}}`)
	if status != 200 || body != `{"linear":{"triggerLabel":"go","routingLabelPrefix":"to:"}}`+"\n" {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	first.stop()

	second := startDaemon(t, path)
	if label := second.polledLabel(t); label != "go" {
		t.Fatalf("restarted Poller queried with %q; want go", label)
	}
	if got := second.poller.Settings.LinearLabels(context.Background()); got != (linearintake.Labels{TriggerLabel: "go", RoutingLabelPrefix: "to:"}) {
		t.Fatalf("restarted Poller settings = %+v", got)
	}
	status, body = second.request(t, "GET", "/api/settings", "")
	if status != 200 || body != `{"linear":{"triggerLabel":"go","routingLabelPrefix":"to:"}}`+"\n" {
		t.Fatalf("GET after restart status=%d body=%s", status, body)
	}
}

// A settings row with an empty value written directly to the DB: the
// started Control Plane polls with that key's default and logs it (spec 15
// 6章, Q76).
func TestStartupWithEmptyStoredSettingPollsWithDefaultAndLogs(t *testing.T) {
	path := newLinearDB(t)
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES
		('linear.triggerLabel', '""', 't'), ('linear.routingLabelPrefix', '"to:"', 't')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	d := startDaemon(t, path)
	if label := d.polledLabel(t); label != "ai-ready" {
		t.Fatalf("polled with %q; want the default ai-ready", label)
	}
	status, body := d.request(t, "GET", "/api/settings", "")
	if status != 200 || body != `{"linear":{"triggerLabel":"ai-ready","routingLabelPrefix":"to:"}}`+"\n" {
		t.Fatalf("GET status=%d body=%s", status, body)
	}
	d.stop()
	if logs := d.output.String(); !strings.Contains(logs, "setting is empty; using the default") || !strings.Contains(logs, "key=linear.triggerLabel") {
		t.Fatalf("logs = %s", logs)
	}
}
