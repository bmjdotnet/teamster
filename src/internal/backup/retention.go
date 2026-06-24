package backup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var timestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{6}Z$`)

func applyRetention(backupDir string, cfg RetentionConfig, logger *slog.Logger) error {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return err
	}

	// Resolve the "latest" symlink so we can exclude it from pruning — a short
	// keep_for could otherwise delete the backup we just created.
	latestTarget, _ := os.Readlink(filepath.Join(backupDir, "latest"))

	var dirs []string
	for _, e := range entries {
		if e.IsDir() && timestampPattern.MatchString(e.Name()) {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs) // lexicographic = chronological for ISO timestamps

	// Age-based pruning: delete dirs older than keep_for.
	if cfg.KeepFor != "" {
		keepFor, err := parseDuration(cfg.KeepFor)
		if err != nil {
			logger.Warn("retention: invalid keep_for, skipping age pruning", "keep_for", cfg.KeepFor, "error", err)
		} else {
			cutoff := time.Now().UTC().Add(-keepFor)
			var remaining []string
			for _, name := range dirs {
				if name == latestTarget {
					remaining = append(remaining, name)
					continue
				}
				ts, err := time.Parse("2006-01-02T150405Z", name)
				if err != nil {
					remaining = append(remaining, name)
					continue
				}
				if ts.Before(cutoff) {
					target := filepath.Join(backupDir, name)
					logger.Info("retention: removing old backup (age)", "dir", name, "age", time.Since(ts).Round(time.Minute))
					if err := os.RemoveAll(target); err != nil {
						logger.Warn("retention: failed to remove", "dir", name, "error", err)
					}
				} else {
					remaining = append(remaining, name)
				}
			}
			dirs = remaining
		}
	}

	// Count-based cap: delete oldest until count <= max_count.
	if cfg.MaxCount > 0 {
		for len(dirs) > cfg.MaxCount {
			oldest := dirs[0]
			dirs = dirs[1:]
			if oldest == latestTarget {
				continue
			}
			target := filepath.Join(backupDir, oldest)
			logger.Info("retention: removing old backup (count cap)", "dir", oldest)
			if err := os.RemoveAll(target); err != nil {
				logger.Warn("retention: failed to remove", "dir", oldest, "error", err)
			}
		}
	}

	return nil
}

func updateLatestSymlink(backupDir, timestampDir string) error {
	linkPath := filepath.Join(backupDir, "latest")
	tmpLink := linkPath + ".tmp"
	// Create temp symlink then rename atomically over latest.
	_ = os.Remove(tmpLink)
	if err := os.Symlink(timestampDir, tmpLink); err != nil {
		return err
	}
	return os.Rename(tmpLink, linkPath)
}

// parseDuration parses human-friendly durations including days, months, years.
// Handles Go's time.ParseDuration suffixes (h, m, s) plus d, mo, y.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	// Try Go's built-in parser first (handles h, m, s).
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	// Parse a number + suffix. Use "mo" for months to avoid ambiguity with
	// Go's built-in "m" (minutes) — "1m" is 1 minute, "1mo" is 1 month.
	re := regexp.MustCompile(`^(\d+)(h|d|mo|y)$`)
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("unrecognised duration %q (use e.g. 1h, 7d, 1mo, 1y)", s)
	}
	n, _ := strconv.Atoi(m[1])
	switch m[2] {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "mo":
		return time.Duration(n) * 30 * 24 * time.Hour, nil
	case "y":
		return time.Duration(n) * 365 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown suffix in %q", s)
}
