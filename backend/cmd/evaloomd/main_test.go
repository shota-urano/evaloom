package main

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunInitializesDatabaseBeforeWaitingForShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, io.Discard) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	// Opening read-only avoids creating a file that would hide a missing
	// startup call. Observe the database actually initialized by evaloomd.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count)
		if err == nil {
			var mode string
			if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("startup journal_mode = %q, err = %v; want wal", mode, err)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("database was not initialized: %v", err)
		case <-ticker.C:
		}
	}
}

func TestListenLoopbackBindsOnlyIPv4Loopback(t *testing.T) {
	listener, err := listenLoopback(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address type = %T; want *net.TCPAddr", listener.Addr())
	}
	if !address.IP.IsLoopback() || address.IP.To4() == nil || !address.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("bound IP = %v; want exactly IPv4 127.0.0.1", address.IP)
	}
	if address.Port == 0 {
		t.Fatal("listener did not acquire a port")
	}
}

type startupOutput chan string

func (output startupOutput) Write(p []byte) (int, error) {
	output <- string(p)
	return len(p), nil
}

func TestRunServesAfterMigrationAndClosesListenerOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := make(startupOutput, 1)
	stopped := make(chan struct{})
	var runErr error
	go func() {
		runErr = run(ctx, []string{"-db", path, "-port", "0"}, output)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
			if runErr != nil {
				t.Errorf("run: %v", runErr)
			}
		case <-time.After(2 * shutdownTimeout):
			t.Error("run did not stop after cancellation")
		}
	})
	var line string
	select {
	case line = <-output:
	case <-stopped:
		t.Fatalf("server exited before startup: %v", runErr)
	case <-ctx.Done():
		t.Fatal("server did not start")
	}
	address, ok := strings.CutPrefix(strings.TrimSpace(line), "evaloomd: listening on ")
	if !ok {
		t.Fatalf("unexpected startup output: %q", line)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("running server address = %q, err = %v; want 127.0.0.1", address, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("HTTP started before migrations succeeded: %v", err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered route status = %d; want 404", response.StatusCode)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * shutdownTimeout):
		t.Fatal("run did not stop after cancellation")
	}
	connection, err := net.DialTimeout("tcp4", address, time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}

func TestRunRejectsOccupiedPort(t *testing.T) {
	listener, err := listenLoopback(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = run(ctx, []string{"-db", filepath.Join(t.TempDir(), "evaloom.db"), "-port", strconv.Itoa(port)}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("run with occupied port = %v; want listen error", err)
	}
}

func TestRunRejectsInvalidPortBeforeCreatingDatabase(t *testing.T) {
	for _, port := range []string{"-1", "65536", "0.0.0.0:8080", "[::]:8080"} {
		t.Run(port, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaloom.db")
			if err := run(context.Background(), []string{"-db", path, "-port", port}, io.Discard); err == nil {
				t.Fatal("run succeeded with invalid port")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration created database: %v", err)
			}
		})
	}
}

func TestRunRejectsInvalidDatabaseConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"-db", t.TempDir()}, {"-db", "unused.db", "unexpected"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := run(ctx, args, io.Discard)
		cancel()
		if err == nil {
			t.Fatalf("run(%v) succeeded; want startup error", args)
		}
	}
}
