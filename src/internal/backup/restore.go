package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
)

// RunRestore restores stores from a backup directory into the locations
// specified by cfg. Only stores that are "ok" in the manifest AND enabled
// in cfg are restored. Acquires the same flock as backup to prevent
// concurrent restore+backup.
func RunRestore(ctx context.Context, cfg *Config, restoreDir string, dryRun bool, logger *slog.Logger) error {
	// Resolve symlinks (e.g. "latest").
	resolved, err := filepath.EvalSymlinks(restoreDir)
	if err != nil {
		return fmt.Errorf("resolve restore dir %q: %w", restoreDir, err)
	}
	restoreDir = resolved

	manifest, err := loadManifest(restoreDir)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if manifest.Version != manifestVersion {
		return fmt.Errorf("unsupported manifest version %q (want %q)", manifest.Version, manifestVersion)
	}

	// Warn when restoring a backup taken on a different host.
	if manifest.Hostname != cfg.Hostname {
		logger.Warn("restoring backup from different host", "backup_host", manifest.Hostname, "this_host", cfg.Hostname)
	}

	// Determine which stores to restore. The manifest is the source of truth
	// for what's available; config controls paths and mode (e.g. grafana
	// external = skip). A store is restored if it's "ok" in the manifest,
	// skipping only stores explicitly excluded by mode.
	type restorePlan struct {
		name  string
		files []string
	}
	var plan []restorePlan
	add := func(name string, skip bool) {
		if skip {
			return
		}
		r, ok := manifest.Stores[name]
		if ok && r.Status == "ok" {
			plan = append(plan, restorePlan{name: name, files: r.Files})
		}
	}
	add("mysql", false)
	add("prometheus", false)
	add("grafana", cfg.Stores.Grafana.Mode == "external")
	add("otel", false)
	add("teamster", false)

	if len(plan) == 0 {
		logger.Info("restore: nothing to restore (no stores have status ok in manifest)")
		return nil
	}

	logger.Info("restore plan",
		"backup_dir", restoreDir,
		"backup_time", manifest.Timestamp.Format("2006-01-02T15:04:05Z"),
		"stores", func() []string {
			names := make([]string, len(plan))
			for i, p := range plan {
				names[i] = p.name
			}
			return names
		}(),
	)

	if dryRun {
		for _, p := range plan {
			logger.Info("dry-run: would restore", "store", p.name, "files", p.files)
		}
		return nil
	}

	// BackupDir may be empty in restore-only mode; fall back to the parent of
	// the resolved restore directory so we never flock on "/.lock".
	lockDir := cfg.BackupDir
	if lockDir == "" {
		lockDir = filepath.Dir(restoreDir)
	}
	unlock, err := lockBackupDir(lockDir)
	if err != nil {
		return fmt.Errorf("acquire backup lock: %w", err)
	}
	defer unlock()

	anyFailed := false
	for _, p := range plan {
		var rerr error
		switch p.name {
		case "mysql":
			rerr = RestoreMySQL(ctx, &cfg.Stores.MySQL, restoreDir, logger)
		case "prometheus":
			rerr = RestorePrometheus(ctx, &cfg.Stores.Prometheus, restoreDir, logger)
		case "grafana":
			rerr = RestoreGrafana(ctx, &cfg.Stores.Grafana, restoreDir, logger)
		case "otel":
			rerr = RestoreOTel(ctx, &cfg.Stores.OTel, restoreDir, logger)
		case "teamster":
			rerr = RestoreTeamster(ctx, &cfg.Stores.Teamster, restoreDir, logger)
		}
		if rerr != nil {
			logger.Error("restore failed", "store", p.name, "error", rerr)
			anyFailed = true
		} else {
			logger.Info("restore complete", "store", p.name)
		}
	}

	if anyFailed {
		return fmt.Errorf("one or more stores failed to restore")
	}
	return nil
}

func loadManifest(restoreDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(restoreDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	return &m, nil
}

// RestoreMySQL pipes each <db>.sql.gz dump back into mysql using credentials
// from the DSN — no sudo required.
func RestoreMySQL(ctx context.Context, cfg *MySQLConfig, restoreDir string, logger *slog.Logger) error {
	if cfg.DSN == "" {
		return fmt.Errorf("mysql restore requires store.dsn in teamster.yaml")
	}

	mysqlDir := filepath.Join(restoreDir, "mysql")
	for _, db := range cfg.Databases {
		dumpFile := filepath.Join(mysqlDir, db+".sql.gz")
		if _, err := os.Stat(dumpFile); err != nil {
			return fmt.Errorf("mysql restore: dump file not found for db %q: %w", db, err)
		}
		logger.Info("mysql restore: importing", "db", db)
		if err := restoreDatabase(ctx, cfg.DSN, db, dumpFile); err != nil {
			return fmt.Errorf("import %s: %w", db, err)
		}
	}
	return nil
}

// restoreDatabase opens db via store.Open (retargeted to the given database
// name per dsnForDatabase) and restores it through the backend's BackupEngine
// — the gunzip/mysql shell-out mechanism itself is unchanged, just relocated
// behind the admin-plane interface in internal/store/mysql (ADR-2).
func restoreDatabase(ctx context.Context, rawDSN, db, dumpFile string) error {
	dsn, err := dsnForDatabase(rawDSN, db)
	if err != nil {
		return fmt.Errorf("build DSN for db %q: %w", db, err)
	}
	st, err := store.Open(ctx, dsn, store.WithSkipMigrate())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close() //nolint:errcheck

	be, ok := st.(store.BackupEngine)
	if !ok {
		return fmt.Errorf("backend has no backup engine")
	}
	return be.Restore(ctx, dumpFile)
}

// RestorePrometheus stops prometheus, extracts into a fresh data dir with
// rename-aside rollback on failure, then restarts.
func RestorePrometheus(ctx context.Context, cfg *PrometheusConfig, restoreDir string, logger *slog.Logger) error {
	tarFile := filepath.Join(restoreDir, "prometheus", "metrics2.tar.gz")
	if _, err := os.Stat(tarFile); err != nil {
		return fmt.Errorf("prometheus tar not found: %w", err)
	}

	dataDir := cfg.DataDir
	parentDir := filepath.Dir(dataDir)
	aside := dataDir + ".pre-restore"

	logger.Info("prometheus restore: stopping service")
	_ = exec.CommandContext(ctx, "systemctl", "stop", "prometheus").Run()

	// Rename-aside before clearing so we can roll back on extract failure.
	// Remove any stale aside from a prior failed restore first.
	_ = os.RemoveAll(aside)
	if err := os.Rename(dataDir, aside); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rename data dir aside: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		// Can't create fresh dir; restore the original.
		_ = os.Rename(aside, dataDir)
		_ = exec.CommandContext(ctx, "systemctl", "start", "prometheus").Run()
		return fmt.Errorf("create fresh data dir: %w", err)
	}

	logger.Info("prometheus restore: extracting", "tar", tarFile)
	// tar was created as: tar czf metrics2.tar.gz -C parentDir dirName
	// so it expands back to parentDir/dirName when extracted with -C parentDir.
	if out, err := exec.CommandContext(ctx, "tar", "xzf", tarFile, "-C", parentDir).CombinedOutput(); err != nil {
		// Extract failed — roll back.
		_ = os.RemoveAll(dataDir)
		_ = os.Rename(aside, dataDir)
		_ = exec.CommandContext(ctx, "systemctl", "start", "prometheus").Run()
		return fmt.Errorf("tar extract prometheus: %w: %s", err, strings.TrimSpace(string(out)))
	}

	logger.Info("prometheus restore: starting service")
	if out, err := exec.CommandContext(ctx, "systemctl", "start", "prometheus").CombinedOutput(); err != nil {
		logger.Warn("prometheus restore: could not start prometheus", "error", err, "output", string(out))
	}
	_ = os.RemoveAll(aside)
	return nil
}

// RestoreGrafana copies grafana.db and provisioning back, restarts grafana.
func RestoreGrafana(ctx context.Context, cfg *GrafanaConfig, restoreDir string, logger *slog.Logger) error {
	// External mode: Grafana is shared — do not restore grafana.db into it.
	if cfg.Mode == "external" {
		logger.Info("grafana restore: skipped (external mode — grafana is shared)")
		return nil
	}

	grafanaRestoreDir := filepath.Join(restoreDir, "grafana")

	// Restore grafana.db.
	dbSrc := filepath.Join(grafanaRestoreDir, "grafana.db")
	dbDst := filepath.Join(cfg.DataDir, "grafana.db")
	if _, err := os.Stat(dbSrc); err == nil {
		// Remove stale WAL/SHM before copying the restored db, otherwise
		// Grafana will replay the old WAL against the new database on startup.
		_ = os.Remove(dbDst + "-wal")
		_ = os.Remove(dbDst + "-shm")
		logger.Info("grafana restore: copying database", "dst", dbDst)
		if err := copyFile(ctx, dbSrc, dbDst); err != nil {
			return fmt.Errorf("copy grafana.db: %w", err)
		}
	}

	// Restore WAL sidecar files if present in the backup.
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := dbSrc + suffix
		if _, err := os.Stat(sidecar); err == nil {
			dst := dbDst + suffix
			if err := copyFile(ctx, sidecar, dst); err != nil {
				logger.Warn("grafana restore: could not copy sidecar", "file", sidecar, "error", err)
			}
		}
	}

	// Restore provisioning.
	provTar := filepath.Join(grafanaRestoreDir, "provisioning.tar.gz")
	if _, err := os.Stat(provTar); err == nil {
		provParent := filepath.Dir(cfg.ProvisioningDir)
		logger.Info("grafana restore: extracting provisioning", "dst", provParent)
		if out, err := exec.CommandContext(ctx, "tar", "xzf", provTar, "-C", provParent).CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract provisioning: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	// Log restart outcome instead of silently ignoring failure.
	logger.Info("grafana restore: restarting service")
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "grafana-server").CombinedOutput(); err != nil {
		logger.Warn("grafana restore: could not restart grafana-server", "error", err, "output", string(out))
	}
	return nil
}

// RestoreOTel copies each config file back to its original location.
func RestoreOTel(ctx context.Context, cfg *OTelConfig, restoreDir string, logger *slog.Logger) error {
	otelRestoreDir := filepath.Join(restoreDir, "otel")
	for _, dstPath := range cfg.Files {
		base := filepath.Base(dstPath)
		src := filepath.Join(otelRestoreDir, base)
		// Missing backup file for an enabled store is an error.
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("otel restore: backup file not found for %q: %w", base, err)
		}
		logger.Info("otel restore: copying", "dst", dstPath)
		if err := copyFile(ctx, src, dstPath); err != nil {
			return fmt.Errorf("copy otel file %s: %w", base, err)
		}
	}
	return nil
}

// RestoreTeamster untars config and state archives back into basedir.
func RestoreTeamster(ctx context.Context, cfg *TeamsterConfig, restoreDir string, logger *slog.Logger) error {
	teamsterRestoreDir := filepath.Join(restoreDir, "teamster")

	configTar := filepath.Join(teamsterRestoreDir, "config.tar.gz")
	if _, err := os.Stat(configTar); err == nil {
		logger.Info("teamster restore: extracting config", "dst", cfg.BaseDir)
		if out, err := exec.CommandContext(ctx, "tar", "xzf", configTar, "-C", cfg.BaseDir).CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract teamster config: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	stateTar := filepath.Join(teamsterRestoreDir, "state.tar.gz")
	if _, err := os.Stat(stateTar); err == nil {
		varDir := filepath.Join(cfg.BaseDir, "var")
		// Ensure var/ exists before extracting state.
		if err := os.MkdirAll(varDir, 0o755); err != nil {
			return fmt.Errorf("create var dir: %w", err)
		}
		logger.Info("teamster restore: extracting state", "dst", varDir)
		if out, err := exec.CommandContext(ctx, "tar", "xzf", stateTar, "-C", varDir).CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract teamster state: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	return nil
}
