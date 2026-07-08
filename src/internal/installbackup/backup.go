// Package installbackup is the shared "back up before you write" utility for
// every client config file the installer modifies in place — Claude Code's
// settings.json/.claude.json/CLAUDE.md and Codex's config.toml alike. It has
// no format opinions; it operates on raw file bytes.
package installbackup

import (
	"fmt"
	"os"
	"time"
)

// Backup preserves path's current content two ways before the caller
// overwrites it:
//
//  1. The very first time this ever runs against path, it copies path to
//     path+".pre-teamster" and never touches that copy again — the durable
//     record of what the operator had before Teamster ever touched the file.
//  2. Every call, first or not, also makes a fresh
//     path+"."+<UTC timestamp>+".bak" copy and returns its name, so the
//     caller can roll back exactly this run's pre-write state if a
//     post-write validation gate (e.g. `codex --strict-config doctor`)
//     rejects the new content.
//
// A path that doesn't exist yet is a no-op: there is nothing to preserve.
// Backup returns ("", nil) in that case — callers pass this empty string to
// Restore to mean "roll back to file-did-not-exist", not "leave the file
// alone".
func Backup(path string) (timestampedPath string, err error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return "", nil
	} else if statErr != nil {
		return "", statErr
	}

	preTeamster := path + ".pre-teamster"
	if _, statErr := os.Stat(preTeamster); os.IsNotExist(statErr) {
		if err := copyFile(path, preTeamster); err != nil {
			return "", fmt.Errorf("preserve %s: %w", preTeamster, err)
		}
	} else if statErr != nil {
		return "", statErr
	}

	ts := path + "." + time.Now().UTC().Format("20060102T150405Z") + ".bak"
	if err := copyFile(path, ts); err != nil {
		return "", fmt.Errorf("timestamped backup %s: %w", ts, err)
	}
	return ts, nil
}

// Restore rolls path back to backupPath's content — used when a post-write
// gate rejects what the caller just wrote. backupPath is normally the value
// Backup just returned. An empty backupPath means Backup found nothing to
// preserve (path didn't exist before this run), so Restore removes path
// rather than leaving the rejected content in place — a freshly-created file
// that failed its gate should end up absent, not broken.
func Restore(backupPath, path string) error {
	if backupPath == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return copyFile(backupPath, path)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(src); statErr == nil {
		mode = info.Mode()
	}
	return os.WriteFile(dst, data, mode)
}
