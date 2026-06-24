// Command backup creates a timestamped snapshot of all teamster data stores.
// It is designed to run as a systemd service on a timer (hourly by default).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bmjdotnet/teamster/internal/backup"
	"github.com/bmjdotnet/teamster/internal/logging"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "Path to teamster.yaml (required)")
	dryRun := flag.Bool("dry-run", false, "Show what would be backed up/restored without doing it")
	restoreDir := flag.String("restore", "", "Restore from this backup directory (path or 'latest' symlink)")
	force := flag.Bool("force", false, "Skip restore confirmation prompt (for non-interactive use)")
	flag.Parse()

	if *configPath == "" {
		flag.Usage()
		return 2
	}

	logger := logging.Init("backup")

	cfg, err := backup.LoadConfig(*configPath, *restoreDir != "")
	if err != nil {
		logger.Error("config load failed", "error", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *restoreDir != "" {
		if !*dryRun && !*force {
			fmt.Fprintf(os.Stderr, "\nWARNING: This will overwrite live data with the backup from:\n  %s\n\nStores to restore: mysql, prometheus, grafana, otel, teamster (if enabled and ok in manifest).\nServices will be stopped and restarted as needed.\n\nPass --force to proceed without this prompt, or --dry-run to preview.\n\nType 'yes' to continue: ", *restoreDir)
			var answer string
			fmt.Fscan(os.Stdin, &answer)
			if answer != "yes" {
				fmt.Fprintf(os.Stderr, "Restore cancelled.\n")
				return 1
			}
		}
		if err := backup.RunRestore(ctx, cfg, *restoreDir, *dryRun, logger); err != nil {
			if errors.Is(err, backup.ErrBackupRunning) {
				logger.Info("restore skipped: backup is currently running")
				return 0
			}
			logger.Error("restore failed", "error", err)
			return 1
		}
		return 0
	}

	if err := backup.Run(ctx, cfg, *configPath, *dryRun, logger); err != nil {
		if errors.Is(err, backup.ErrBackupRunning) {
			logger.Info("backup skipped: another instance is running")
			return 0
		}
		logger.Error("backup failed", "error", err)
		return 1
	}
	return 0
}
