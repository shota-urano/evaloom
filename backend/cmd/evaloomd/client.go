package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/programauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const clientUsage = "usage: evaloomd -db <database file path> client register (-harness <id> | -project <id>) [-name <display name>] <client id> | client revoke <client id> | client list"

// programClient runs the program key commands (spec 20 4.3). Only register
// prints the key, once; nothing else prints the key or its hash.
func programClient(ctx context.Context, path string, args []string, output io.Writer) (err error) {
	if len(args) == 0 {
		return errors.New(clientUsage)
	}
	switch args[0] {
	case "register":
		return registerProgramClient(ctx, path, args[1:], output)
	case "revoke":
		if len(args) != 2 {
			return errors.New(clientUsage)
		}
		db, err := store.Open(ctx, path)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, db.Close()) }()
		if err := store.RevokeProgramClient(ctx, db, args[1], time.Now()); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("client revoke: client %q is not registered", args[1])
		} else if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "revoked %s\n", args[1])
		return err
	case "list":
		if len(args) != 1 {
			return errors.New(clientUsage)
		}
		db, err := store.Open(ctx, path)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, db.Close()) }()
		clients, err := store.ListProgramClients(ctx, db)
		if err != nil {
			return err
		}
		for _, c := range clients {
			target := "harness:" + c.HarnessID
			if c.ProjectID != "" {
				target = "project:" + c.ProjectID
			}
			revoked := "-"
			if c.RevokedAt != nil {
				revoked = *c.RevokedAt
			}
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\tcreated=%s\trevoked=%s\n", c.ID, c.Name, target, c.CreatedAt, revoked); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New(clientUsage)
}

func registerProgramClient(ctx context.Context, path string, args []string, output io.Writer) (err error) {
	flags := flag.NewFlagSet("evaloomd client register", flag.ContinueOnError)
	flags.SetOutput(output)
	harness := flags.String("harness", "", "Special Harness ID routed to directly")
	project := flags.String("project", "", "Project ID routed to")
	name := flags.String("name", "", "display name (defaults to client ID)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("client register: exactly one client ID is required; put flags before the ID")
	}
	id := flags.Arg(0)
	if *name == "" {
		*name = id
	}
	if err := programauth.ValidateRegistration(id, *name, *project, *harness); err != nil {
		return err
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	key, err := programauth.Register(ctx, db, id, *name, *project, *harness)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, key)
	return err
}
