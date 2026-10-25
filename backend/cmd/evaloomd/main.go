package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("evaloomd", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("db", "", "SQLite database file path (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: evaloomd -db <database file path>")
	}
	db, err := store.Open(ctx, *path)
	if err != nil {
		return err
	}
	// Future API startup belongs here, after all migrations have succeeded.
	<-ctx.Done()
	return db.Close()
}
