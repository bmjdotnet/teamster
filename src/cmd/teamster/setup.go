package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bmjdotnet/teamster/internal/tui"
)

// runSetup dispatches `teamster setup <subcommand>`. Returns exit code.
func runSetup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, setupUsage)
		return 2
	}
	switch args[0] {
	case "tags":
		return runSetupTags(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, setupUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "setup %s: coming soon\n", args[0])
		return 1
	}
}

const setupUsage = `usage: teamster setup <subcommand>

subcommands:
  tags    Guided tag vocabulary setup and editor

flags (tags):
  --interview    Re-run the guided setup interview`

// setupTagsHint maps a store-open failure to a one-line remediation hint, or ""
// when none applies. It matches on the error text (the DSN itself is never in
// these messages — go-sql-driver prints "(using password: YES)", not the
// secret, and the DSN-parse path masks the scheme only).
func setupTagsHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "newer than this binary supports"):
		return "the deployed binary is older than the database schema — run ./install.sh to upgrade the binary"
	case strings.Contains(msg, "Access denied"):
		return "auth failed — ensure $TEAMSTER_STORE_DSN is set, or reinstall to refresh teamster.yaml"
	default:
		return ""
	}
}

func runSetupTags(args []string) int {
	fs := flag.NewFlagSet("teamster setup tags", flag.ContinueOnError)
	interview := fs.Bool("interview", false, "re-run the guided setup interview")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
		if hint := setupTagsHint(err); hint != "" {
			fmt.Fprintf(os.Stderr, "  hint: %s\n", hint)
		}
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	// Detect first-run: no non-empty product values in the vocabulary.
	productValues, err := s.TagValues(ctx, "product")
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
		return 1
	}
	productValueCount := 0
	for _, v := range productValues {
		if v.Value != "" {
			productValueCount++
		}
	}

	if productValueCount == 0 || *interview {
		if err := tui.RunWizard(s); err != nil {
			fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
			return 1
		}
		return 0
	}

	if err := tui.RunEditor(s); err != nil {
		fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
		return 1
	}
	return 0
}
