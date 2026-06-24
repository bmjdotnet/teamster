package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/backup"
	"github.com/bmjdotnet/teamster/internal/logging"
)

const backupUsage = `usage: teamster backup [subcommand] [flags]

subcommands:
  (none)        Take a backup now
  list          List available backups (most recent first)
  status        Show backup timer status and last run

flags (backup):
  --config <path>   Path to teamster.yaml (default: auto-detect from basedir)
  --dry-run         Show what would be backed up without doing it`

const restoreUsage = `usage: teamster restore <path> [flags]

arguments:
  <path>            Backup directory to restore from (path or 'latest')

flags:
  --config <path>   Path to teamster.yaml (default: auto-detect from basedir)
  --dry-run         Show what would be restored without doing it
  --force           Skip confirmation prompt`

// runBackup dispatches the `teamster backup <subcommand>` family.
func runBackup(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return runBackupList(args[1:])
		case "status":
			return runBackupStatus(args[1:])
		case "-h", "--help", "help":
			fmt.Fprintln(os.Stdout, backupUsage)
			return 0
		}
	}
	return runBackupRun(args)
}

// runBackupRun takes a backup immediately.
func runBackupRun(args []string) int {
	fs := flag.NewFlagSet("teamster backup", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to teamster.yaml")
	dryRun := fs.Bool("dry-run", false, "Show what would be backed up without doing it")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		return 1
	}

	logger := logging.Init("backup")

	cfg, err := backup.LoadConfig(path, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: config: %v\n", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := backup.Run(ctx, cfg, path, *dryRun, logger); err != nil {
		if errors.Is(err, backup.ErrBackupRunning) {
			fmt.Fprintln(os.Stderr, "backup: another backup is already running")
			return 0
		}
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		return 1
	}
	return 0
}

// runBackupList lists available backup directories with sizes and per-store status.
func runBackupList(args []string) int {
	fs := flag.NewFlagSet("teamster backup list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to teamster.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup list: %v\n", err)
		return 1
	}

	cfg, err := backup.LoadConfig(path, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup list: config: %v\n", err)
		return 1
	}

	entries, err := os.ReadDir(cfg.BackupDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "backup list: backup_dir does not exist: %s\n", cfg.BackupDir)
			return 1
		}
		fmt.Fprintf(os.Stderr, "backup list: read dir: %v\n", err)
		return 1
	}

	type backupEntry struct {
		name   string
		size   int64
		stores map[string]string // store name → status
	}

	var dirs []backupEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Only list timestamped directories (not "latest" symlink target).
		if len(name) < 16 || !strings.Contains(name, "T") {
			continue
		}
		dirPath := filepath.Join(cfg.BackupDir, name)
		size := dirSize(dirPath)

		// Read manifest for per-store status.
		stores := map[string]string{}
		manifestPath := filepath.Join(dirPath, "manifest.json")
		if data, err := os.ReadFile(manifestPath); err == nil {
			var m struct {
				Stores map[string]struct {
					Status string `json:"status"`
				} `json:"stores"`
			}
			if err := json.Unmarshal(data, &m); err == nil {
				for k, v := range m.Stores {
					if v.Status != "skipped" {
						stores[k] = v.Status
					}
				}
			}
		}

		dirs = append(dirs, backupEntry{name: name, size: size, stores: stores})
	}

	// Most recent first.
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].name > dirs[j].name
	})

	if len(dirs) == 0 {
		fmt.Println("No backups found.")
		return 0
	}

	for i, d := range dirs {
		storeStr := formatStores(d.stores)
		latest := ""
		if i == 0 {
			latest = "  (latest)"
		}
		fmt.Printf("%-22s  %6s  %s%s\n", d.name, formatSize(d.size), storeStr, latest)
	}
	return 0
}

// runBackupStatus shows the last backup time and systemd timer state.
func runBackupStatus(args []string) int {
	fs := flag.NewFlagSet("teamster backup status", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to teamster.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup status: %v\n", err)
		return 1
	}

	cfg, err := backup.LoadConfig(path, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup status: config: %v\n", err)
		return 1
	}

	// Last backup: read latest manifest.
	latestLink := filepath.Join(cfg.BackupDir, "latest")
	latestDir, err := filepath.EvalSymlinks(latestLink)
	if err == nil {
		manifestPath := filepath.Join(latestDir, "manifest.json")
		if data, err2 := os.ReadFile(manifestPath); err2 == nil {
			var m struct {
				Timestamp time.Time              `json:"timestamp"`
				Hostname  string                 `json:"hostname"`
				Stores    map[string]struct {
					Status     string `json:"status"`
					TotalBytes int64  `json:"total_bytes"`
				} `json:"stores"`
			}
			if err2 := json.Unmarshal(data, &m); err2 == nil {
				age := time.Since(m.Timestamp).Round(time.Second)
				fmt.Printf("Last backup:  %s  (%s ago, host: %s)\n",
					m.Timestamp.Format("2006-01-02T15:04:05Z"), age, m.Hostname)
				fmt.Printf("Location:     %s\n", latestDir)
				stores := map[string]string{}
				for k, v := range m.Stores {
					if v.Status != "skipped" {
						stores[k] = v.Status
					}
				}
				fmt.Printf("Stores:       %s\n", formatStores(stores))
			}
		}
	} else {
		fmt.Println("Last backup:  none")
	}

	// Timer status via systemctl.
	fmt.Println()
	out, err := exec.Command("systemctl", "show", "teamster-backup.timer",
		"--property=ActiveState,LastTriggerUSec,NextElapseUSec").Output()
	if err != nil {
		fmt.Println("Timer:        (systemctl unavailable)")
		return 0
	}
	props := parseSystemctlProps(string(out))
	fmt.Printf("Timer state:  %s\n", props["ActiveState"])
	if v := props["LastTriggerUSec"]; v != "" && v != "0" {
		fmt.Printf("Last trigger: %s\n", formatUSec(v))
	}
	if v := props["NextElapseUSec"]; v != "" && v != "0" {
		fmt.Printf("Next elapse:  %s\n", formatUSec(v))
	}
	return 0
}

// runRestore restores from a backup directory.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("teamster restore", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to teamster.yaml")
	dryRun := fs.Bool("dry-run", false, "Show what would be restored without doing it")
	force := fs.Bool("force", false, "Skip confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, restoreUsage)
		return 2
	}
	restoreDir := fs.Arg(0)

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}

	logger := logging.Init("restore")

	cfg, err := backup.LoadConfig(path, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: config: %v\n", err)
		return 1
	}

	if !*dryRun && !*force {
		fmt.Fprintf(os.Stderr, "\nWARNING: This will overwrite live data with the backup from:\n  %s\n\nStores to restore: mysql, prometheus, grafana, otel, teamster (if enabled and ok in manifest).\nServices will be stopped and restarted as needed.\n\nPass --force to proceed without this prompt, or --dry-run to preview.\n\nType 'yes' to continue: ", restoreDir)
		var answer string
		fmt.Fscan(os.Stdin, &answer) //nolint:errcheck
		if answer != "yes" {
			fmt.Fprintln(os.Stderr, "Restore cancelled.")
			return 1
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := backup.RunRestore(ctx, cfg, restoreDir, *dryRun, logger); err != nil {
		if errors.Is(err, backup.ErrBackupRunning) {
			fmt.Fprintln(os.Stderr, "restore: backup is currently running, try again later")
			return 0
		}
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	return 0
}

// resolveConfigPath returns the explicit config path or auto-detects from basedir.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	// Auto-detect: TEAMSTER_BASEDIR env, then ~/teamster/, then /usr/local/teamster/.
	if v := os.Getenv("TEAMSTER_BASEDIR"); v != "" {
		return filepath.Join(v, "etc", "teamster.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	for _, candidate := range []string{
		filepath.Join(home, "teamster"),
		"/usr/local/teamster",
	} {
		p := filepath.Join(candidate, "etc", "teamster.yaml")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("teamster.yaml not found; pass --config <path>")
}

// dirSize returns the total size of all files under dir (best-effort; ignores errors).
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

func formatSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.0fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func formatStores(stores map[string]string) string {
	order := []string{"mysql", "prometheus", "grafana", "otel", "teamster"}
	var parts []string
	for _, k := range order {
		if v, ok := stores[k]; ok {
			parts = append(parts, k+":"+v)
		}
	}
	return strings.Join(parts, " ")
}

func parseSystemctlProps(output string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if eq := strings.IndexByte(line, '='); eq >= 0 {
			props[line[:eq]] = line[eq+1:]
		}
	}
	return props
}

func formatUSec(usec string) string {
	// systemd USec values are microseconds since epoch or human-readable strings.
	// Try to parse as integer microseconds.
	var us int64
	if _, err := fmt.Sscan(usec, &us); err == nil && us > 0 {
		t := time.Unix(us/1e6, (us%1e6)*1e3).UTC()
		return t.Format("2006-01-02T15:04:05Z")
	}
	return usec
}
