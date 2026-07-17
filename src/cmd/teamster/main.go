// Command teamster is the Claude Code hook client binary.
// It reads a hook event JSON from stdin, enriches it, POSTs to the hook
// server, and writes any additionalContext response to stdout.
// It always exits 0 — it must never crash or block Claude Code.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	_ "github.com/bmjdotnet/teamster/internal/store/mysql" // registers mysql, mariadb
	"github.com/bmjdotnet/teamster/internal/version"
)

func main() {
	isTTY := false
	if fi, err := os.Stdin.Stat(); err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}

	if len(os.Args) <= 1 && isTTY {
		printUsage()
		os.Exit(0)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "--help", "-h":
			printUsage()
			os.Exit(0)
		case "version", "--version", "-v":
			fmt.Printf("teamster %s\n", version.String())
			os.Exit(0)
		}
	}

	// Supervisor subcommands: real exit codes, not the hook-client's must-exit-0 rule.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start", "stop", "status", "wms-reset":
			runSupervisor(os.Args[1:])
			return
		case "store":
			os.Exit(runStore(os.Args[2:]))
		case "sql":
			os.Exit(runSQL(os.Args[2:]))
		case "tags":
			os.Exit(runTags(os.Args[2:]))
		case "wms":
			os.Exit(runWMS(os.Args[2:]))
		case "search":
			os.Exit(runSearch(os.Args[2:]))
		case "backup":
			os.Exit(runBackup(os.Args[2:]))
		case "restore":
			os.Exit(runRestore(os.Args[2:]))
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		case "install-remote":
			os.Exit(runInstallRemote(os.Args[2:]))
		}
	}

	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "{") && isTTY {
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\nRun 'teamster help' for usage.\n", os.Args[1])
		os.Exit(1)
	}

	// Hook client path: stdin is a pipe from Claude Code.
	defer func() {
		recover() //nolint:errcheck // panic safety: must never crash Claude Code
		os.Exit(0)
	}()

	cfg, err := config.Load()
	if err != nil {
		// Config failure: still exit 0, just skip processing.
		os.Exit(0)
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}

	if os.Getenv("TEAMSTER_DEBUG_RAW") != "" {
		if p := filepath.Join(cfg.DataDir, "raw-hook-debug.jsonl"); p != "" {
			f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				f.Write(raw)
				f.Write([]byte("\n"))
				f.Close()
			}
		}
	}

	var event hook.HookEvent
	var rawData map[string]interface{}

	if err := json.Unmarshal(raw, &event); err != nil {
		os.Exit(0)
	}
	if err := json.Unmarshal(raw, &rawData); err != nil {
		os.Exit(0)
	}

	out := hook.ProcessEvent(event, rawData, cfg.HookServerURL, cfg.DedupDir, cfg.Solo)
	if out != "" {
		fmt.Print(out)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "usage: teamster <subcommand> [flags]\n\n")
	fmt.Fprintf(os.Stderr, "Subcommands:\n")
	fmt.Fprintf(os.Stderr, "  start       Start all supervised services (daemonizes)\n")
	fmt.Fprintf(os.Stderr, "  stop        Stop all services\n")
	fmt.Fprintf(os.Stderr, "  status      Show service status (--live for interactive dashboard)\n")
	fmt.Fprintf(os.Stderr, "  wms-reset   Reset WMS database (stop → delete → start)\n")
	fmt.Fprintf(os.Stderr, "  store       Store management commands\n")
	fmt.Fprintf(os.Stderr, "  sql         Run SQL via $TEAMSTER_STORE_DSN (password stays off argv)\n")
	fmt.Fprintf(os.Stderr, "  tags        Tag vocabulary management\n")
	fmt.Fprintf(os.Stderr, "  wms         WMS entity management (list, drain, close, gc)\n")
	fmt.Fprintf(os.Stderr, "  search      Search outcomes/workunits/sessions (search sessions <query>)\n")
	fmt.Fprintf(os.Stderr, "  backup      Take a backup (or: backup list, backup status)\n")
	fmt.Fprintf(os.Stderr, "  restore     Restore from a backup directory\n")
	fmt.Fprintf(os.Stderr, "  setup       Guided setup and configuration\n")
	fmt.Fprintf(os.Stderr, "  install-remote  Install Teamster remote client on another host\n")
	fmt.Fprintf(os.Stderr, "  version     Print build version and exit\n")
	fmt.Fprintf(os.Stderr, "  help        Show this message\n")
	fmt.Fprintf(os.Stderr, "\nWith no subcommand and stdin piped, acts as Claude Code hook client.\n")
}
