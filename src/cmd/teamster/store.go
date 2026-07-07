package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bmjdotnet/teamster/internal/store"
)

// runStore dispatches the `teamster store <subcommand>` family. It returns
// the process exit code so the caller can call os.Exit directly without
// printing extra newlines.
//
// Supported subcommands:
//
//	teamster store migrate --dsn <dsn>
//
// All flags are long-form (--double-dash); short flags are not exposed for
// this surface to keep cutover scripts unambiguous.
func runStore(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, storeUsage)
		return 2
	}
	switch args[0] {
	case "migrate":
		return runStoreMigrate(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, storeUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown store subcommand: %s\n%s\n", args[0], storeUsage)
		return 2
	}
}

const storeUsage = `usage: teamster store <subcommand>

subcommands:
  migrate    apply schema migrations to a backend

flags (migrate):
  --dsn <DSN>         mysql://...`

func runStoreMigrate(args []string) int {
	fs := flag.NewFlagSet("teamster store migrate", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "DSN to migrate (mysql://...)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "--dsn is required")
		return 2
	}
	// Opening the store runs migrations as a side effect.
	s, err := openStore(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck
	fmt.Fprintln(os.Stdout, "ok: schema current")
	return 0
}

func openStore(dsn string) (store.Store, error) {
	return store.Open(context.Background(), dsn)
}
