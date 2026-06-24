package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func BackupPrometheus(ctx context.Context, cfg *PrometheusConfig, destDir string) (*StoreResult, error) {
	start := time.Now()

	if err := os.MkdirAll(filepath.Join(destDir, "prometheus"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir prometheus: %w", err)
	}

	outPath := filepath.Join(destDir, "prometheus", "metrics2.tar.gz")
	parentDir := filepath.Dir(cfg.DataDir)
	dirName := filepath.Base(cfg.DataDir)

	cmd := exec.CommandContext(ctx, "tar", "czf", outPath, "-C", parentDir, dirName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// tar exit code 1 = "file changed as we read it" — expected for
		// hot Prometheus backups (WAL writes during archive). The archive
		// is still usable; treat as warning, not failure.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// continue — archive was created
		} else {
			return &StoreResult{
				Status:     "error",
				DurationMS: time.Since(start).Milliseconds(),
				Error:      fmt.Sprintf("tar: %v: %s", err, out),
			}, fmt.Errorf("tar prometheus: %w", err)
		}
	}

	info, _ := os.Stat(outPath)
	var size int64
	if info != nil {
		size = info.Size()
	}

	return &StoreResult{
		Status:     "ok",
		Files:      []string{"prometheus/metrics2.tar.gz"},
		TotalBytes: size,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}
