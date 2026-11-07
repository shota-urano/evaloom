package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type startupPollClient struct{ polled chan string }

func (c startupPollClient) CheckKey() error { return nil }

func (c startupPollClient) ListReadyIssues(_ context.Context, teamID, _ string) ([]linear.Issue, error) {
	select {
	case c.polled <- teamID:
	default:
	}
	return nil, nil
}

func TestRunPollsLinearAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('r1', 'r1', 'h', '/w', 'online', '2026-11-08T00:00:00Z', '2026-11-08T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Fill every required column so the test follows schema changes.
	rows, err := db.Query(`SELECT name FROM pragma_table_info('projects') WHERE "notnull" = 1 AND dflt_value IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	columns := []string{"linear_team_id", "linear_routing_label"}
	values := []any{"team-a", "repo:p"}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		value := map[string]string{"id": "p1", "local_repo_path": "/repos/p1", "runner_id": "r1"}[column]
		if value == "" {
			value = "x"
		}
		columns = append(columns, column)
		values = append(values, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (`+strings.Join(columns, ", ")+`) VALUES (?`+
		strings.Repeat(", ?", len(columns)-1)+`)`, values...); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	polled := make(chan string, 1)
	original := newLinearPoller
	t.Cleanup(func() { newLinearPoller = original })
	newLinearPoller = func(db *sql.DB, logger *slog.Logger, _ int) (*linearintake.Poller, error) {
		// An interval far longer than the test isolates the startup poll.
		return &linearintake.Poller{DB: db, Client: startupPollClient{polled}, Logger: logger, Interval: time.Hour}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, io.Discard) }()
	select {
	case team := <-polled:
		if team != "team-a" {
			t.Fatalf("polled team = %q", team)
		}
	case <-ctx.Done():
		t.Fatal("Control Plane did not poll Linear at startup")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestDefaultLinearPollerUsesProductionConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	poller, err := newLinearPoller(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 49123)
	if err != nil {
		t.Fatal(err)
	}
	if poller.Interval != time.Minute || poller.TriggerLabel != "" {
		t.Fatalf("poller = %+v; want 1 minute interval and default trigger label", poller)
	}
	if _, ok := poller.Client.(*linear.Client); !ok {
		t.Fatalf("client = %T; want *linear.Client", poller.Client)
	}
	// On macOS the production Poller must alert through the macOS helper.
	if runtime.GOOS == "darwin" {
		if _, ok := poller.Alerter.(macOSLinearAlerter); !ok {
			t.Fatalf("alerter = %T; want macOSLinearAlerter", poller.Alerter)
		}
	}
}

func TestLinearPollAlerterIsMacOSOnlyOnDarwin(t *testing.T) {
	alerter, ok := linearPollAlerter("darwin", "/Users/owner", 49123).(macOSLinearAlerter)
	if !ok || alerter.macOS == nil {
		t.Fatalf("darwin alerter = %#v; want macOSLinearAlerter", alerter)
	}
	for _, goos := range []string{"linux", "windows"} {
		if got := linearPollAlerter(goos, "/unused", 8080); got != nil {
			t.Fatalf("%s alerter = %#v; want nil", goos, got)
		}
	}
}
