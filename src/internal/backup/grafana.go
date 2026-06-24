package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func BackupGrafana(ctx context.Context, cfg *GrafanaConfig, destDir string) (*StoreResult, error) {
	start := time.Now()

	// External mode: Grafana is shared with other applications; we don't own
	// grafana.db and provisioning is already captured by the teamster store.
	if cfg.Mode == "external" {
		return &StoreResult{
			Status:     "skipped",
			DurationMS: time.Since(start).Milliseconds(),
			Error:      "external mode — provisioning files backed up via teamster store",
		}, nil
	}

	result := &StoreResult{Status: "ok"}

	grafanaDir := filepath.Join(destDir, "grafana")
	if err := os.MkdirAll(grafanaDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir grafana: %w", err)
	}

	// Use sqlite3's online backup command to copy grafana.db. This handles
	// WAL mode correctly (flushes WAL, produces a consistent snapshot)
	// without needing to stop Grafana.
	dbSrc := filepath.Join(cfg.DataDir, "grafana.db")
	dbDst := filepath.Join(grafanaDir, "grafana.db")
	// Write the .backup command to a temp file and pipe it via stdin so the
	// destination path is never subject to shell or sqlite3 dot-command quoting.
	sqlTmp, err := os.CreateTemp("", "teamster-sqlite3-*.sql")
	if err != nil {
		return nil, fmt.Errorf("create sqlite3 script: %w", err)
	}
	defer os.Remove(sqlTmp.Name())
	if _, err := fmt.Fprintf(sqlTmp, ".backup %s\n", dbDst); err != nil {
		sqlTmp.Close()
		return nil, fmt.Errorf("write sqlite3 script: %w", err)
	}
	if err := sqlTmp.Close(); err != nil {
		return nil, fmt.Errorf("close sqlite3 script: %w", err)
	}
	scriptData, err := os.Open(sqlTmp.Name())
	if err != nil {
		return nil, fmt.Errorf("open sqlite3 script: %w", err)
	}
	defer scriptData.Close()
	sqliteCmd := exec.CommandContext(ctx, "sqlite3", dbSrc)
	sqliteCmd.Stdin = scriptData
	if out, err := sqliteCmd.CombinedOutput(); err != nil {
		return &StoreResult{
			Status:     "error",
			DurationMS: time.Since(start).Milliseconds(),
			Error:      fmt.Sprintf("sqlite3 backup grafana.db: %v: %s", err, out),
		}, fmt.Errorf("sqlite3 backup grafana.db: %w", err)
	}
	if info, _ := os.Stat(dbDst); info != nil {
		result.TotalBytes += info.Size()
	}
	result.Files = append(result.Files, "grafana/grafana.db")

	// Tar provisioning directory
	provPath := filepath.Join(grafanaDir, "provisioning.tar.gz")
	provParent := filepath.Dir(cfg.ProvisioningDir)
	provName := filepath.Base(cfg.ProvisioningDir)
	cmd := exec.CommandContext(ctx, "tar", "czf", provPath, "-C", provParent, provName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return &StoreResult{
			Status:     "error",
			DurationMS: time.Since(start).Milliseconds(),
			Error:      fmt.Sprintf("tar provisioning: %v: %s", err, out),
		}, fmt.Errorf("tar provisioning: %w", err)
	}
	if pi, _ := os.Stat(provPath); pi != nil {
		result.TotalBytes += pi.Size()
	}
	result.Files = append(result.Files, "grafana/provisioning.tar.gz")

	// Optional plugins directory
	if cfg.IncludePlugins {
		pluginsPath := filepath.Join(grafanaDir, "plugins.tar.gz")
		cmd := exec.CommandContext(ctx, "tar", "czf", pluginsPath, "-C", cfg.DataDir, "plugins")
		if out, err := cmd.CombinedOutput(); err != nil {
			// Non-fatal: plugins dir may not exist
			_ = out
		} else {
			if pi, _ := os.Stat(pluginsPath); pi != nil {
				result.TotalBytes += pi.Size()
			}
			result.Files = append(result.Files, "grafana/plugins.tar.gz")
		}
	}

	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}

func copyFile(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "cp", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s -> %s: %v: %s", src, dst, err, out)
	}
	return nil
}
