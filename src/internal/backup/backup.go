package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const manifestVersion = "1"

// Run executes a full backup according to cfg. Returns an error if any store
// failed (but continues running all stores before returning). When backup_dir
// is unset, logs a warning and skips — the service is not considered failed.
func Run(ctx context.Context, cfg *Config, configPath string, dryRun bool, logger *slog.Logger) error {
	if cfg.BackupDir == "" {
		logger.Warn("backup_dir is not configured — skipping backup (set backup.backup_dir in teamster.yaml to enable)")
		return nil
	}
	ts := time.Now().UTC()
	tsDir := ts.Format("2006-01-02T150405Z")
	destDir := filepath.Join(cfg.BackupDir, tsDir)

	if dryRun {
		logger.Info("dry-run: would create backup dir", "path", destDir)
		logDryRun(cfg, logger)
		return nil
	}

	// Acquire the flock before creating the timestamped directory so two
	// concurrent processes starting at the same second don't both create the
	// same directory and then race — one would be locked out leaving an empty dir.
	unlock, err := lockBackupDir(cfg.BackupDir)
	if err != nil {
		return fmt.Errorf("acquire backup lock: %w", err)
	}
	defer unlock()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	manifest := &Manifest{
		Version:         manifestVersion,
		Hostname:        cfg.Hostname,
		TeamsterVersion: teamsterVersion(),
		Timestamp:       ts,
		ConfigPath:      configPath,
		Stores:          make(map[string]StoreResult),
	}

	overallStart := time.Now()
	anyFailed := false

	if cfg.Stores.MySQL.Enabled {
		result, err := BackupMySQL(ctx, &cfg.Stores.MySQL, destDir)
		if result == nil {
			result = &StoreResult{Status: "error", Error: fmt.Sprintf("%v", err)}
		}
		if err != nil {
			logger.Error("mysql backup failed", "error", err)
			anyFailed = true
		} else {
			logger.Info("mysql backup complete", "files", len(result.Files), "bytes", result.TotalBytes, "ms", result.DurationMS)
		}
		manifest.Stores["mysql"] = *result
	} else {
		manifest.Stores["mysql"] = StoreResult{Status: "skipped"}
	}

	if cfg.Stores.Prometheus.Enabled {
		result, err := BackupPrometheus(ctx, &cfg.Stores.Prometheus, destDir)
		if result == nil {
			result = &StoreResult{Status: "error", Error: fmt.Sprintf("%v", err)}
		}
		if err != nil {
			logger.Error("prometheus backup failed", "error", err)
			anyFailed = true
		} else {
			logger.Info("prometheus backup complete", "bytes", result.TotalBytes, "ms", result.DurationMS)
		}
		manifest.Stores["prometheus"] = *result
	} else {
		manifest.Stores["prometheus"] = StoreResult{Status: "skipped"}
	}

	if cfg.Stores.Grafana.Enabled {
		result, err := BackupGrafana(ctx, &cfg.Stores.Grafana, destDir)
		if result == nil {
			result = &StoreResult{Status: "error", Error: fmt.Sprintf("%v", err)}
		}
		if err != nil {
			logger.Error("grafana backup failed", "error", err)
			anyFailed = true
		} else {
			logger.Info("grafana backup complete", "files", len(result.Files), "bytes", result.TotalBytes, "ms", result.DurationMS)
		}
		manifest.Stores["grafana"] = *result
	} else {
		manifest.Stores["grafana"] = StoreResult{Status: "skipped"}
	}

	if cfg.Stores.OTel.Enabled {
		result, err := BackupOTel(ctx, &cfg.Stores.OTel, destDir)
		if result == nil {
			result = &StoreResult{Status: "error", Error: fmt.Sprintf("%v", err)}
		}
		if err != nil {
			logger.Error("otel backup failed", "error", err)
			anyFailed = true
		} else {
			logger.Info("otel backup complete", "files", len(result.Files), "bytes", result.TotalBytes, "ms", result.DurationMS)
		}
		manifest.Stores["otel"] = *result
	} else {
		manifest.Stores["otel"] = StoreResult{Status: "skipped"}
	}

	if cfg.Stores.Teamster.Enabled {
		result, err := BackupTeamster(ctx, &cfg.Stores.Teamster, destDir)
		if result == nil {
			result = &StoreResult{Status: "error", Error: fmt.Sprintf("%v", err)}
		}
		if err != nil {
			logger.Error("teamster backup failed", "error", err)
			anyFailed = true
		} else {
			logger.Info("teamster backup complete", "files", len(result.Files), "bytes", result.TotalBytes, "ms", result.DurationMS)
		}
		manifest.Stores["teamster"] = *result
	} else {
		manifest.Stores["teamster"] = StoreResult{Status: "skipped"}
	}

	manifest.DurationMS = time.Since(overallStart).Milliseconds()

	if err := writeManifest(destDir, manifest); err != nil {
		logger.Warn("failed to write manifest", "error", err)
	}

	if err := updateLatestSymlink(cfg.BackupDir, tsDir); err != nil {
		logger.Warn("failed to update latest symlink", "error", err)
	}

	if err := applyRetention(cfg.BackupDir, cfg.Retention, logger); err != nil {
		logger.Warn("retention pass failed", "error", err)
	}

	logger.Info("backup complete", "dir", tsDir, "duration_ms", manifest.DurationMS, "any_failed", anyFailed)

	if anyFailed {
		return fmt.Errorf("one or more stores failed")
	}
	return nil
}

func logDryRun(cfg *Config, logger *slog.Logger) {
	if cfg.Stores.MySQL.Enabled {
		logger.Info("dry-run: would backup mysql", "databases", cfg.Stores.MySQL.Databases)
	}
	if cfg.Stores.Prometheus.Enabled {
		logger.Info("dry-run: would backup prometheus", "data_dir", cfg.Stores.Prometheus.DataDir)
	}
	if cfg.Stores.Grafana.Enabled {
		logger.Info("dry-run: would backup grafana", "data_dir", cfg.Stores.Grafana.DataDir)
	}
	if cfg.Stores.OTel.Enabled {
		logger.Info("dry-run: would backup otel", "files", cfg.Stores.OTel.Files)
	}
	if cfg.Stores.Teamster.Enabled {
		logger.Info("dry-run: would backup teamster", "base_dir", cfg.Stores.Teamster.BaseDir)
	}
}

func teamsterVersion() string {
	if v := buildVersion; v != "" {
		return v
	}
	return "dev"
}

// buildVersion is set by the linker at build time.
var buildVersion string
