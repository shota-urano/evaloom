package main

import (
	"context"
	"database/sql"
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
	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/notification"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	defaultPort       = 8080
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
	// linearRequestTimeout bounds one Linear HTTP request so a hung request
	// cannot stall polling forever. It is an implementation limit, not a
	// specification value.
	linearRequestTimeout = 30 * time.Second
)

// newLinearPoller builds the Linear Poller started with the Control Plane
// (spec 15 3.1). Tests replace it to avoid the real Linear API.
// port is the actual bound port, used by the macOS alert to open the Dashboard.
var newLinearPoller = func(db *sql.DB, logger *slog.Logger, port int) (*linearintake.Poller, error) {
	keyPath, err := linear.DefaultAPIKeyPath()
	if err != nil {
		return nil, fmt.Errorf("evaloomd: linear: %w", err)
	}
	client, err := linear.NewClient(&http.Client{Timeout: linearRequestTimeout}, linear.Endpoint, keyPath, logger)
	if err != nil {
		return nil, fmt.Errorf("evaloomd: linear: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("evaloomd: linear alert home: %w", err)
	}
	poller := linearPoller(db, client, logger)
	if alerter := linearPollAlerter(runtime.GOOS, home, port); alerter != nil {
		poller.Alerter = alerter
	}
	return poller, nil
}

// newLinearFeedback builds the Worker that sends the Linear comments queued
// in linear_outbox (spec 16 3.4). Tests replace it to avoid the real Linear
// API. The key file is read only when a row is due. port is the actual bound
// port, used by the authentication error alert to open the Dashboard.
var newLinearFeedback = func(db *sql.DB, logger *slog.Logger, port int) (*linearfeedback.Worker, error) {
	keyPath, err := linear.DefaultAPIKeyPath()
	if err != nil {
		return nil, fmt.Errorf("evaloomd: linear feedback: %w", err)
	}
	client, err := linear.NewClient(&http.Client{Timeout: linearRequestTimeout}, linear.Endpoint, keyPath, logger)
	if err != nil {
		return nil, fmt.Errorf("evaloomd: linear feedback: %w", err)
	}
	worker := &linearfeedback.Worker{DB: db, Client: client, Logger: logger, Interval: linearfeedback.DefaultInterval}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("evaloomd: linear feedback alert home: %w", err)
	}
	if alerter := linearCommentAlerter(runtime.GOOS, home, port); alerter != nil {
		worker.Alerter = alerter
	}
	return worker, nil
}

// linearPoller connects the Poller to Intake (spec 15 3.4), which starts
// tasks with TaskStarter (3.6), rechecks held tasks with the same TaskStarter
// (3.6.3) and cancels with TaskCanceller (3.7). client also re-reads the
// Issues that left the list.
func linearPoller(db *sql.DB, client *linear.Client, logger *slog.Logger) *linearintake.Poller {
	starter := &linearintake.TaskStarter{}
	return &linearintake.Poller{DB: db, Client: client, Logger: logger, Interval: linearintake.DefaultInterval,
		Handler: &linearintake.Intake{DB: db, Fetcher: client, Starter: starter,
			Canceller: linearintake.TaskCanceller{}, Rechecker: starter, Logger: logger}}
}

// macOSLinearAlerter sends the Linear polling alert (spec 15 6章) and the
// Linear comment authentication alert (spec 16 3.4.3) through the same macOS
// helper as task notifications. Both open the Dashboard (spec 12 3.5).
type macOSLinearAlerter struct{ macOS *notification.MacOS }

func (a macOSLinearAlerter) AlertLinearPollFailing(ctx context.Context, alert linearintake.Alert) error {
	return a.macOS.NotifyLinearPollFailing(ctx, string(alert.Class), alert.ConsecutiveFailures)
}

func (a macOSLinearAlerter) AlertLinearCommentAuth(ctx context.Context) error {
	return a.macOS.NotifyLinearCommentFailing(ctx, string(linear.ClassAuth))
}

// linearCommentAlerter returns the macOS alerter on darwin and nil elsewhere,
// like linearPollAlerter.
func linearCommentAlerter(goos, home string, port int) linearfeedback.AuthAlerter {
	if goos != "darwin" {
		return nil
	}
	return macOSLinearAlerter{notification.NewMacOS(notificationHelperPath(home), port)}
}

// linearPollAlerter returns the macOS alerter on darwin and nil elsewhere,
// like phase1NotificationChannels.
func linearPollAlerter(goos, home string, port int) linearintake.Alerter {
	if goos != "darwin" {
		return nil
	}
	return macOSLinearAlerter{notification.NewMacOS(notificationHelperPath(home), port)}
}

func notificationHelperPath(home string) string {
	return filepath.Join(home, "Applications", "Evaloom Notifications.app")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer, channels ...notification.Channel) (err error) {
	return runWithInput(ctx, args, os.Stdin, output, channels...)
}

func runWithInput(ctx context.Context, args []string, input *os.File, output io.Writer, channels ...notification.Channel) (err error) {
	flags := flag.NewFlagSet("evaloomd", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("db", "", "SQLite database file path (required)")
	dataDir := flags.String("data-dir", "", "Control Plane artifact data directory outside Git tracking (requires -artifact-max-bytes)")
	artifactMaxBytes := flags.Int64("artifact-max-bytes", 0, "maximum artifact body size in bytes (requires -data-dir)")
	port := flags.Int("port", defaultPort, "HTTP port on 127.0.0.1 (0 selects an available port)")
	remoteHost := flags.String("remote-host", "", "single Tailscale DNS name allowed as Host with its HTTPS Origin")
	webDir := flags.String("web", "", "directory of the built Management UI (frontend/dist); empty disables UI serving")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *path != "" && flags.NArg() > 0 && flags.Arg(0) == "runner" {
		return registerRunner(ctx, *path, flags.Args()[1:], output)
	}
	if *path != "" && flags.NArg() > 0 && flags.Arg(0) == "owner" {
		return setOwnerPassword(ctx, *path, flags.Args()[1:], input, output)
	}
	if *path == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: evaloomd -db <database file path> [-port <0-65535>] [-remote-host <name>] [-web <built UI directory>]")
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("evaloomd: port must be between 0 and 65535")
	}
	if (*dataDir == "") != (*artifactMaxBytes == 0) || *artifactMaxBytes < 0 {
		return fmt.Errorf("evaloomd: -data-dir and a positive -artifact-max-bytes must be supplied together")
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
	var artifacts api.ArtifactConfig
	if *dataDir != "" {
		files, err := store.NewArtifactStore(db, *dataDir)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, files.Close()) }()
		artifacts = api.ArtifactConfig{Store: files, MaxBytes: *artifactMaxBytes}
		// Linear comment bodies read 作業概要 from work_summary (spec 16 3.3).
		defer linearoutbox.SetSummarySource(files.HarnessSummaryTx)()
	}

	// Bind only after database initialization succeeds. The host is deliberately
	// not configurable: neither a wildcard nor an IPv6 listener is permitted.
	listener, err := listenLoopback(ctx, *port)
	if err != nil {
		return fmt.Errorf("evaloomd: listen: %w", err)
	}
	defer listener.Close()
	logger := slog.New(slog.NewTextHandler(output, nil))
	auth, err := api.NewOwnerAuth(db, logger, listener.Addr().String(), *remoteHost)
	if err != nil {
		return err
	}
	// The settings API and the Linear Poller share the label settings stored
	// in the settings table, so they survive a restart (spec 15 5章, Q48, Q76).
	linearSettings := linearintake.NewLabelSettings(db, logger)
	adapters, err := controlPlaneAdapters()
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           api.NewHandlerWithSettings(db, logger, auth, web, artifacts, linearSettings, adapters),
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
	// One authentication error notification suppression shared by the Poller
	// and the comment Worker: the same API key failing for queries and sends
	// alerts once (spec 16 3.4.3, spec 12 3.5, Q84). It lives in the process,
	// so a restart alerts again.
	authGate := &linear.AuthAlertGate{}
	// Poll Linear once at startup and then at each interval (spec 15 3.1).
	poller, err := newLinearPoller(db, logger, listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	poller.Settings = linearSettings
	poller.AuthGate = authGate
	pollerCtx, cancelPoller := context.WithCancel(ctx)
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		poller.Run(pollerCtx)
	}()
	defer func() {
		cancelPoller()
		<-pollerDone // Stop DB users before closing the connection.
	}()
	// Send the queued Linear comments with retries (spec 16 3.4).
	feedback, err := newLinearFeedback(db, logger, listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	feedback.AuthGate = authGate
	feedbackCtx, cancelFeedback := context.WithCancel(ctx)
	feedbackDone := make(chan struct{})
	go func() {
		defer close(feedbackDone)
		feedback.Run(feedbackCtx)
	}()
	defer func() {
		cancelFeedback()
		<-feedbackDone // Stop DB users before closing the connection.
	}()
	served := make(chan error, 1)
	recoveryStartedAt := time.Now().UTC()
	go func() { served <- server.Serve(listener) }()
	// Serve heartbeats throughout the startup grace period so a surviving
	// Runner can keep its Execution after a Control Plane restart.
	recoveryTimer := time.NewTimer(store.StartupHeartbeatGrace)
	defer recoveryTimer.Stop()
	fmt.Fprintf(output, "evaloomd: listening on %s\n", listener.Addr())

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
	return []notification.Channel{{Name: "macos", Notifier: notification.NewMacOS(notificationHelperPath(home), port)}}
}

func registerRunner(ctx context.Context, path string, args []string, output io.Writer) (err error) {
	if len(args) == 0 || args[0] != "register" {
		return fmt.Errorf("usage: evaloomd -db <database file path> runner register [-name <display name>] <id>")
	}
	flags := flag.NewFlagSet("evaloomd runner register", flag.ContinueOnError)
	flags.SetOutput(output)
	root := flags.String("worktree-root", "", "deprecated compatibility option; configure the workspace directory on the Runner")
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
