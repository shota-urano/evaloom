package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

func run(ctx context.Context, args []string, output io.Writer) (err error) {
	flags := flag.NewFlagSet("evaloomd", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("db", "", "SQLite database file path (required)")
	port := flags.Int("port", defaultPort, "HTTP port on 127.0.0.1 (0 selects an available port)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: evaloomd -db <database file path> [-port <0-65535>]")
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("evaloomd: port must be between 0 and 65535")
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
	server := &http.Server{
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	fmt.Fprintf(output, "evaloomd: listening on %s\n", listener.Addr())

	select {
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

func listenLoopback(ctx context.Context, port int) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
