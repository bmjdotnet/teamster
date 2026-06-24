package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func BackupOTel(ctx context.Context, cfg *OTelConfig, destDir string) (*StoreResult, error) {
	start := time.Now()
	result := &StoreResult{Status: "ok"}

	otelDir := filepath.Join(destDir, "otel")
	if err := os.MkdirAll(otelDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir otel: %w", err)
	}

	for _, src := range cfg.Files {
		dst := filepath.Join(otelDir, filepath.Base(src))
		if err := copyFile(ctx, src, dst); err != nil {
			return &StoreResult{
				Status:     "error",
				DurationMS: time.Since(start).Milliseconds(),
				Error:      fmt.Sprintf("copy %s: %v", src, err),
			}, err
		}
		if info, _ := os.Stat(dst); info != nil {
			result.TotalBytes += info.Size()
		}
		result.Files = append(result.Files, "otel/"+filepath.Base(src))
	}

	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}

func BackupTeamster(ctx context.Context, cfg *TeamsterConfig, destDir string) (*StoreResult, error) {
	start := time.Now()
	result := &StoreResult{Status: "ok"}

	tsDir := filepath.Join(destDir, "teamster")
	if err := os.MkdirAll(tsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir teamster: %w", err)
	}

	// Tar the etc/ directory
	cfgPath := filepath.Join(tsDir, "config.tar.gz")
	cmd := exec.CommandContext(ctx, "tar", "czf", cfgPath, "-C", cfg.BaseDir, "etc")
	if out, err := cmd.CombinedOutput(); err != nil {
		return &StoreResult{
			Status:     "error",
			DurationMS: time.Since(start).Milliseconds(),
			Error:      fmt.Sprintf("tar etc: %v: %s", err, out),
		}, fmt.Errorf("tar teamster etc: %w", err)
	}
	if info, _ := os.Stat(cfgPath); info != nil {
		result.TotalBytes += info.Size()
	}
	result.Files = append(result.Files, "teamster/config.tar.gz")

	// Gather state files from var/
	varDir := filepath.Join(cfg.BaseDir, "var")
	stateFiles, err := collectStateFiles(varDir, cfg.IncludeLogs)
	if err != nil {
		// Non-fatal: var/ may be empty or not exist
		stateFiles = nil
	}

	if len(stateFiles) > 0 {
		statePath := filepath.Join(tsDir, "state.tar.gz")
		args := []string{"czf", statePath, "-C", varDir}
		args = append(args, stateFiles...)
		cmd := exec.CommandContext(ctx, "tar", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return &StoreResult{
				Status:     "error",
				DurationMS: time.Since(start).Milliseconds(),
				Error:      fmt.Sprintf("tar var state: %v: %s", err, out),
			}, fmt.Errorf("tar teamster state: %w", err)
		}
		if info, _ := os.Stat(statePath); info != nil {
			result.TotalBytes += info.Size()
		}
		result.Files = append(result.Files, "teamster/state.tar.gz")
	}

	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}

// collectStateFiles returns basenames of state files in varDir to include.
func collectStateFiles(varDir string, includeLogs bool) ([]string, error) {
	entries, err := os.ReadDir(varDir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isStateFile(name, includeLogs) {
			files = append(files, name)
		}
	}
	return files, nil
}

func isStateFile(name string, includeLogs bool) bool {
	if includeLogs && strings.HasSuffix(name, ".log") {
		return true
	}
	if name == "events.jsonl" {
		return true
	}
	if name == "scraper-cursors.json" {
		return true
	}
	if strings.HasSuffix(name, ".db") {
		return true
	}
	if strings.HasSuffix(name, ".json") {
		return true
	}
	return false
}
