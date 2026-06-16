package main

import (
	"fmt"
	"os"
)

// runWMS dispatches the `teamster wms <subcommand>` family. Returns the exit code.
func runWMS(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, wmsUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return runWMSList(args[1:])
	case "drain":
		return runWMSDrain(args[1:])
	case "close":
		return runWMSClose(args[1:])
	case "adopt":
		return runWMSAdopt(args[1:])
	case "gc":
		return runWMSGC(args[1:])
	case "backfill":
		return runWMSBackfill(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, wmsUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown wms subcommand: %s\n%s\n", args[0], wmsUsage)
		return 2
	}
}

const wmsUsage = `usage: teamster wms <subcommand>

subcommands:
  list                  list outcomes and work units
  drain                 close open intervals (dry-run by default)
  close <entity-id>     transition entity to done + close intervals
  adopt <entity-id>     guidance for adopting an entity in a new session
  gc                    garbage collect: drain + close stale entities (dry-run by default)
  backfill              recover session_id and close timestamps from JSONL (dry-run by default)

flags (list):
  --status <status>     filter by status (default: all non-terminal)
  --stale <duration>    show entities not updated within duration (e.g. 24h)
  --type <type>         filter by type: outcome or workunit

flags (drain):
  --session <id>        drain intervals for a specific session
  --older-than <dur>    drain intervals on sessions idle longer than duration
  --dry-run             preview what would be drained (default)
  --confirm             actually execute the drain

flags (close):
  --resolution <res>    set resolution: abandoned or achieved (default: abandoned)

flags (gc):
  --older-than <dur>    stale threshold for closing entities (default: 7d)
  --dry-run             preview what would be collected (default)
  --confirm             actually execute the gc

flags (backfill):
  --events <path>       path to events.jsonl (default: auto-detect)
  --dry-run             preview what would be changed (default)
  --confirm             actually execute the backfill`
