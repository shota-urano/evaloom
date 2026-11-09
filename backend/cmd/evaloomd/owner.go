package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"golang.org/x/term"
)

func setOwnerPassword(ctx context.Context, path string, args []string, input *os.File, output io.Writer) (err error) {
	if len(args) == 0 || args[0] != "set-password" {
		return fmt.Errorf("usage: evaloomd -db <database file path> owner set-password -id <ID>")
	}
	flags := flag.NewFlagSet("evaloomd owner set-password", flag.ContinueOnError)
	flags.SetOutput(output)
	id := flags.String("id", "", "owner login ID (required)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("owner set-password: unexpected arguments; supply passwords only on standard input")
	}
	if err := ownerauth.ValidateID(*id); err != nil {
		return err
	}
	read := ownerPasswordReader(input)
	password, err := promptOwnerPassword(output, "Password: ", read)
	defer clear(password)
	if err != nil {
		return err
	}
	confirmation, err := promptOwnerPassword(output, "Confirm password: ", read)
	defer clear(confirmation)
	if err != nil {
		return err
	}
	if !bytes.Equal(password, confirmation) {
		return fmt.Errorf("owner: passwords do not match; nothing saved")
	}
	hash, err := ownerauth.HashPassword(password)
	if err != nil {
		return err
	}
	// Do not even create the DB until both inputs have passed validation.
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := store.SetOwnerCredentials(ctx, db, *id, hash); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Owner password saved; all owner sessions revoked.")
	return err
}

// Only newline framing is removed from pipe input; spaces are password data.
// Use one buffered reader for both reads so a pipe's second line is not lost.
func ownerPasswordReader(input *os.File) func() ([]byte, error) {
	if term.IsTerminal(int(input.Fd())) {
		return func() ([]byte, error) { return term.ReadPassword(int(input.Fd())) }
	}
	reader := bufio.NewReader(input)
	return func() ([]byte, error) {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) > 0 {
			err = nil
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		return line, err
	}
}

func promptOwnerPassword(output io.Writer, prompt string, read func() ([]byte, error)) ([]byte, error) {
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return nil, err
	}
	password, err := read()
	if err != nil {
		clear(password)
		// A reader error may contain input; never echo it to logs or the terminal.
		return nil, fmt.Errorf("owner: could not read password; nothing saved")
	}
	if _, err := fmt.Fprintln(output); err != nil {
		clear(password)
		return nil, err
	}
	return password, nil
}
