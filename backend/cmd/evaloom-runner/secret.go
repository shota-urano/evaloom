package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/secretstore"
	"golang.org/x/term"
)

const secretUsage = "usage: evaloom-runner -config <configuration file path> secret set -access <read|write> <harness-id>/<slot> (value on standard input)"

func setSecret(dataDir string, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || args[0] != "set" {
		return fmt.Errorf("%s", secretUsage)
	}
	flags := flag.NewFlagSet("secret set", flag.ContinueOnError)
	// flag errors may quote input. Do not echo arguments or secret values.
	flags.SetOutput(io.Discard)
	access := flags.String("access", "", "registered use: read or write (required)")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 1 {
		return fmt.Errorf("%s", secretUsage)
	}
	harnessID, slot, ok := strings.Cut(flags.Arg(0), "/")
	if !ok || secretstore.Validate(harnessID, slot, *access) != nil {
		return fmt.Errorf("%s", secretUsage)
	}
	var value []byte
	var err error
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		value, err = term.ReadPassword(int(file.Fd()))
	} else {
		// Pipe input is exact, including spaces and newlines. Use printf %s
		// rather than echo when a trailing newline is not part of the key.
		value, err = io.ReadAll(input)
	}
	defer clear(value)
	if err != nil {
		return fmt.Errorf("runner: cannot read secret from standard input")
	}
	if err := secretstore.New(dataDir).Set(harnessID, slot, *access, value); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Runner secret saved.")
	return err
}
