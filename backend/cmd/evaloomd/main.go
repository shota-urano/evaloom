package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/notification"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	defaultPort       = 8080
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer, channels ...notification.Channel) (err error) {
	flags := flag.NewFlagSet("evaloomd", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("db", "", "SQLite database file path (required)")
	port := flags.Int("port", defaultPort, "HTTP port on 127.0.0.1 (0 selects an available port)")
	webDir := flags.String("web", "", "directory of the built Management UI (frontend/dist); empty disables UI serving")
	contextRoot := flags.String("context-root", ".", "evaloom repository root for Global Context")
	var globalPaths []string
	flags.Func("global-context", "selected Global Context file below context/ (repeatable)", func(value string) error {
		globalPaths = append(globalPaths, value)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *path != "" && flags.NArg() > 0 && flags.Arg(0) == "runner" {
		return registerRunner(ctx, *path, flags.Args()[1:], output)
	}
	if *path == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: evaloomd -db <database file path> [-port <0-65535>] [-web <built UI directory>]")
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("evaloomd: port must be between 0 and 65535")
	}
	var web fs.FS
	if *webDir != "" {
		root, err := openWeb(*webDir)
		if err != nil {
			return err
		}
		defer root.Close()
		web = root.FS()
	}
	db, err := store.Open(ctx, *path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	// Bind only after database initialization succeeds. The host is deliberately
	// not configurable: neither a wildcard nor an IPv6 listener is permitted.
	listener, err := listenLoopback(ctx, *port)
	if err != nil {
		return fmt.Errorf("evaloomd: listen: %w", err)
	}
	defer listener.Close()
	logger := slog.New(slog.NewTextHandler(output, nil))
	auth, err := api.NewOwnerAuth(db, logger, listener.Addr().String())
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           api.NewHandlerWithContext(db, logger, auth, web, api.ContextConfig{RepositoryRoot: *contextRoot, GlobalPaths: globalPaths}),
		ReadHeaderTimeout: readHeaderTimeout,
		// Cancel long-lived SSE requests before waiting for graceful shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	// Tests may supply transports explicitly; production Phase 1 registers only
	// the macOS helper, using the actual bound port (including -port 0).
	if channels == nil && runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("evaloomd: notification helper home: %w", err)
		}
		channels = phase1NotificationChannels(runtime.GOOS, home, listener.Addr().(*net.TCPAddr).Port)
	}
	notificationCtx, cancelNotifications := context.WithCancel(ctx)
	notificationsDone := make(chan struct{})
	dispatcher := notification.Dispatcher{DB: db, Channels: channels}
	go func() {
		defer close(notificationsDone)
		dispatcher.Run(notificationCtx, logger)
	}()
	defer func() {
		cancelNotifications()
		<-notificationsDone // Stop DB users before closing the connection.
	}()
	served := make(chan error, 1)
	recoveryStartedAt := time.Now().UTC()
	go func() { served <- server.Serve(listener) }()
	// Serve heartbeats throughout the startup grace period so a surviving
	// Runner can keep its Execution after a Control Plane restart.
	recoveryTimer := time.NewTimer(store.StartupHeartbeatGrace)
	defer recoveryTimer.Stop()
	fmt.Fprintf(output, "evaloomd: listening on %s\nevaloomd: owner login URL: %s\n", listener.Addr(), auth.LoginURL())

	for {
		select {
		case <-recoveryTimer.C:
			if err := recoverOnStartup(ctx, db, recoveryStartedAt, time.Now().UTC()); err != nil {
				logger.Error("startup recovery failed", "error", err)
				// A failed transaction has no partial effects. Retry recovery, never
				// the Executions, while keeping heartbeat and shutdown available.
				recoveryTimer.Reset(store.StartupHeartbeatGrace)
			}
		case err := <-served:
			return fmt.Errorf("evaloomd: serve: %w", err)
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				server.Close()
				<-served
				return fmt.Errorf("evaloomd: shutdown: %w", err)
			}
			err := <-served
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("evaloomd: serve: %w", err)
			}
			return nil
		}
	}
}

func phase1NotificationChannels(goos, home string, port int) []notification.Channel {
	if goos != "darwin" {
		return nil
	}
	helper := filepath.Join(home, "Applications", "Evaloom Notifications.app")
	return []notification.Channel{{Name: "macos", Notifier: notification.NewMacOS(helper, port)}}
}

func registerRunner(ctx context.Context, path string, args []string, output io.Writer) (err error) {
	if len(args) == 0 || args[0] != "register" {
		return fmt.Errorf("usage: evaloomd -db <database file path> runner register -worktree-root <absolute path> [-name <display name>] <id>")
	}
	flags := flag.NewFlagSet("evaloomd runner register", flag.ContinueOnError)
	flags.SetOutput(output)
	root := flags.String("worktree-root", "", "absolute directory for Runner worktrees (required)")
	name := flags.String("name", "", "display name (defaults to Runner ID)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("runner register: exactly one Runner ID is required; put flags before the ID")
	}
	id := flags.Arg(0)
	if *name == "" {
		*name = id
	}
	if err := runnerauth.ValidateRegistration(id, *name, *root); err != nil {
		return err
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	token, err := runnerauth.Register(ctx, db, id, *name, *root)
	if err != nil {
		return err
	}
	// Only this explicit local command displays the token, once. Normal daemon
	// startup and the owner Runner list never expose it or its hash.
	_, err = fmt.Fprintln(output, token)
	return err
}

func listenLoopback(ctx context.Context, port int) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// openWeb confines UI serving to dir (symlinks cannot escape it) and fails at
// startup, not on the first request, when the build output is missing.
func openWeb(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("evaloomd: web directory: %w", err)
	}
	info, err := root.Stat("index.html")
	if err != nil || !info.Mode().IsRegular() {
		root.Close()
		return nil, fmt.Errorf("evaloomd: web directory %q has no index.html; build the frontend first", dir)
	}
	return root, nil
}
