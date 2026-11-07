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
	fmt.Fprintln(output, "evaloom-runner: started")
	// Result/log delivery must remain alive after an execution is cancelled.
	reportCtx := context.WithoutCancel(ctx)
	return process.Run(ctx, func(ctx context.Context, claim runner.Claim) {
		if err := process.ExecuteClaim(ctx, reportCtx, config, claim, process.PipelineDependenciesForClaim(config, claim)); err != nil {
			fmt.Fprintln(output, err)
		}
	}, func(err error) {
		fmt.Fprintln(output, err)
	})
}
