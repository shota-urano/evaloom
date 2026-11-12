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

	"github.com/shota-urano/evaloom/backend/internal/model"
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
	return runWithInput(ctx, args, os.Stdin, output)
}

func runWithInput(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("evaloom-runner", flag.ContinueOnError)
	flags.SetOutput(output)
	configPath := flags.String("config", "", "Runner JSON configuration file path (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("usage: evaloom-runner -config <configuration file path> [secret set -access <read|write> <harness-id>/<slot>]")
	}
	config, err := runner.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if flags.NArg() > 0 {
		if flags.Arg(0) == "secret" {
			return setSecret(config.DataDir, flags.Args()[1:], input, output)
		}
		return fmt.Errorf("runner: unexpected arguments")
	}
	process, err := runner.New(config)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "evaloom-runner: started")
	// Result delivery must remain alive after an execution is cancelled.
	reportCtx := context.WithoutCancel(ctx)
	services := runner.ProposalServicesFromConfig(config)
	return process.RunJobs(ctx, runner.JobHandlers{
		Execution: func(ctx context.Context, claim runner.Claim) {
			// The development stages (worktree, Gate 2/3, Git Publisher) moved to
			// the special harness software-engineer (docs/specs/19-routing-and-stages.md
			// 3.11). No stage can run a claimed Execution until the stage framework
			// exists, so report it failed instead of leaving it claimed.
			if err := process.ReportResult(reportCtx, claim.Execution.ID, runner.ExecutionResult{
				FromStatus: model.ExecutionClaimed, Status: model.ExecutionFailed, ErrorClass: model.ErrorConfig,
				ErrorMessage: "段を動かす仕組みがまだ無いため実行できない",
			}); err != nil {
				fmt.Fprintln(output, err)
			}
		},
		RouteProposal: func(ctx context.Context, claim runner.ProposalClaim) {
			if err := process.ExecuteProposal(ctx, claim, services); err != nil {
				fmt.Fprintln(output, err)
			}
		},
	}, func(err error) {
		fmt.Fprintln(output, err)
	})
}
