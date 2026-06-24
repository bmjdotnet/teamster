package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrBackupRunning is returned by lockBackupDir when another backup process
// already holds the lock. The caller should exit 0 (not an error condition —
// the timer fired while a previous run was still active).
var ErrBackupRunning = errors.New("another backup is already running")

// lockBackupDir acquires an exclusive advisory flock on backup_dir/.lock.
// Returns an unlock function; call it when the backup is complete.
// Returns ErrBackupRunning if another backup process holds the lock.
func lockBackupDir(backupDir string) (func(), error) {
	lockPath := filepath.Join(backupDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	// LOCK_EX | LOCK_NB: exclusive, non-blocking. Returns EWOULDBLOCK if
	// another process holds the lock.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrBackupRunning
		}
		return nil, fmt.Errorf("flock: %w", err)
	}

	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}
