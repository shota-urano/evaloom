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

	"github.com/shota-urano/evaloom/backend/internal/runner"
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
	flags := flag.NewFlagSet("evaloom-runner", flag.ContinueOnError)
	flags.SetOutput(output)
	configPath := flags.String("config", "", "Runner JSON configuration file path (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: evaloom-runner -config <configuration file path>")
	}
	config, err := runner.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	process, err := runner.New(config)
	if err != nil {
		return err
	}
	if err := process.Heartbeat(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		// Connection failures must not stop ongoing executions. Scheduling
		// subsequent heartbeats and claim calls belongs to the pull-loop task.
		fmt.Fprintln(output, err)
	}
	fmt.Fprintln(output, "evaloom-runner: started (pull loop is not implemented)")
	<-ctx.Done()
	return nil
}
