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
	fmt.Fprintln(output, "evaloom-runner: started (execution stages are not implemented)")
	return process.Run(ctx, func(ctx context.Context, _ runner.Claim) {
		// Stage execution belongs to the next implementation unit. Keep the
		// claimed job registered until cancellation/shutdown, without claiming
		// success or freeing its slot before execution is implemented.
		<-ctx.Done()
	}, func(err error) {
		fmt.Fprintln(output, err)
	})
}
