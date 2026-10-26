package main

import (
	"context"
	"io"
	"testing"
	"time"
)

type startupOutput chan string

func (output startupOutput) Write(p []byte) (int, error) {
	output <- string(p)
	return len(p), nil
}

func TestRunWaitsUntilShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(startupOutput, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, output) }()
	select {
	case <-output:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("runner exited before shutdown: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop after cancellation")
	}
}

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"-db", "control-plane.db"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("run(%v) succeeded; want usage error", args)
		}
	}
}

func TestRunHelpExitsWithoutStarting(t *testing.T) {
	if err := run(context.Background(), []string{"-help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}
