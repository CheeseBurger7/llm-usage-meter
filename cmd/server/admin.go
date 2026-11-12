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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

const adminUsage = `usage:
  server orgs create --name <name>
  server keys create --org <org id> --scope ingest|read
  server keys revoke --prefix <8 characters>`

// keyAdmin is the part of auth.Service the admin commands use.
type keyAdmin interface {
	CreateOrg(ctx context.Context, name string) (auth.Org, error)
	CreateKey(ctx context.Context, org auth.OrgID, scope auth.Scope) (auth.NewKey, error)
	RevokeByPrefix(ctx context.Context, prefix string) (int64, error)
}

// isAdminCommand reports whether the first argument selects an admin command.
func isAdminCommand(name string) bool {
	return name == "orgs" || name == "keys"
}

// adminMain connects to the database and runs an orgs or keys command.
func adminMain() error {
	dsn, err := databaseURL()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := storage.Migrate(ctx, pool); err != nil {
		return err
	}
	return runAdmin(ctx, os.Args[1:], auth.New(pool), os.Stdout, os.Stderr)
}

// runAdmin executes one admin command. The result a script needs (the org id
// or the plaintext key) is the only thing written to stdout; notes for humans
// go to stderr. Nothing goes through the logger, so a key never reaches logs.
func runAdmin(ctx context.Context, args []string, svc keyAdmin, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return errors.New(adminUsage)
	}
	fs := flag.NewFlagSet(args[0]+" "+args[1], flag.ContinueOnError)
	fs.SetOutput(stderr)

	switch args[0] + " " + args[1] {
	case "orgs create":
		name := fs.String("name", "", "display name of the org")
		if err := parseFlags(fs, args[2:]); err != nil {
			return err
		}
		org, err := svc.CreateOrg(ctx, *name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderr, "created org %q\n", org.Name)
		_, _ = fmt.Fprintln(stdout, org.ID)
		return nil

	case "keys create":
		orgFlag := fs.String("org", "", "org id (UUID)")
		scopeFlag := fs.String("scope", "", "ingest or read")
		if err := parseFlags(fs, args[2:]); err != nil {
			return err
		}
		org, err := auth.ParseOrgID(*orgFlag)
		if err != nil {
			return fmt.Errorf("--org: %w", err)
		}
		scope, err := auth.ParseScope(*scopeFlag)
		if err != nil {
			return fmt.Errorf("--scope: %w", err)
		}
		key, err := svc.CreateKey(ctx, org, scope)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderr,
			"created %s key with prefix %s for org %s\nStore the key now: it is not saved and cannot be shown again.\n",
			key.Scope, key.Prefix, key.OrgID)
		_, _ = fmt.Fprintln(stdout, key.Plaintext)
		return nil

	case "keys revoke":
		prefix := fs.String("prefix", "", "the 8-character prefix printed when the key was created")
		if err := parseFlags(fs, args[2:]); err != nil {
			return err
		}
		n, err := svc.RevokeByPrefix(ctx, *prefix)
		if err != nil {
			return err
		}
		if n == 0 {
			// A typo must not look like a successful revocation.
			return fmt.Errorf("no active key has prefix %q", *prefix)
		}
		_, _ = fmt.Fprintf(stderr, "revoked %d key(s) with prefix %s\n", n, *prefix)
		return nil
	}
	return errors.New(adminUsage)
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}
