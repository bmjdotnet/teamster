package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func BackupMySQL(ctx context.Context, cfg *MySQLConfig, destDir string) (*StoreResult, error) {
	start := time.Now()
	result := &StoreResult{Status: "ok"}

	if cfg.DSN == "" {
		return &StoreResult{
			Status:     "error",
			DurationMS: time.Since(start).Milliseconds(),
			Error:      "mysql backup requires store.dsn in teamster.yaml",
		}, fmt.Errorf("mysql backup requires store.dsn in teamster.yaml")
	}

	fields, err := ParseMySQLDSN(cfg.DSN)
	if err != nil {
		return &StoreResult{
			Status:     "error",
			DurationMS: time.Since(start).Milliseconds(),
			Error:      fmt.Sprintf("parse mysql dsn: %v", err),
		}, fmt.Errorf("parse mysql dsn: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(destDir, "mysql"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir mysql: %w", err)
	}

	var errs []string
	for _, db := range cfg.Databases {
		outPath := filepath.Join(destDir, "mysql", db+".sql.gz")
		if err := dumpDatabase(ctx, fields, db, outPath); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", db, err))
			continue
		}
		info, err := os.Stat(outPath)
		if err == nil {
			result.TotalBytes += info.Size()
		}
		result.Files = append(result.Files, "mysql/"+db+".sql.gz")
	}

	result.DurationMS = time.Since(start).Milliseconds()

	if len(errs) > 0 {
		result.Error = strings.Join(errs, "; ")
		if len(result.Files) == 0 {
			result.Status = "error"
			return result, fmt.Errorf("all databases failed: %s", result.Error)
		}
		result.Status = "partial"
		return result, fmt.Errorf("some databases failed: %s", result.Error)
	}
	return result, nil
}

func dumpDatabase(ctx context.Context, fields MySQLDSNFields, db, outPath string) error {
	// Write credentials to a temp file with 0600 so they never appear on the
	// command line (visible in `ps`) or in logs.
	tmp, err := os.CreateTemp("", "teamster-mysql-*.cnf")
	if err != nil {
		return fmt.Errorf("create temp defaults file: %w", err)
	}
	defer os.Remove(tmp.Name())

	cnf := fmt.Sprintf("[client]\nuser=%s\npassword=%s\nhost=%s\nport=%s\n",
		fields.User, fields.Password, fields.Host, fields.Port)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp defaults file: %w", err)
	}
	if _, err := tmp.WriteString(cnf); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp defaults file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp defaults file: %w", err)
	}

	return runDump(ctx, tmp.Name(), db, outPath)
}

// runDump runs mysqldump --defaults-extra-file=<tmpfile> | gzip > outPath.
// stderr is captured and included in the error so failures are never silent.
func runDump(ctx context.Context, defaultsFile, db, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(outPath)
		}
	}()

	dump := exec.CommandContext(ctx, "mysqldump",
		"--defaults-extra-file="+defaultsFile,
		"--single-transaction", "--routines", "--triggers", "--no-tablespaces", db)
	gzip := exec.CommandContext(ctx, "gzip")

	var dumpStderr, gzipStderr bytes.Buffer
	dump.Stderr = &dumpStderr
	gzip.Stderr = &gzipStderr

	dumpOut, err := dump.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dump stdout pipe: %w", err)
	}
	gzip.Stdin = dumpOut
	gzip.Stdout = f

	if err := dump.Start(); err != nil {
		return fmt.Errorf("mysqldump start: %w", err)
	}
	if err := gzip.Start(); err != nil {
		dump.Wait() //nolint:errcheck
		return fmt.Errorf("gzip start: %w", err)
	}

	dumpErr := dump.Wait()
	gzipErr := gzip.Wait()

	if dumpErr != nil {
		stderr := dumpStderr.String()
		if stderr != "" {
			return fmt.Errorf("mysqldump: %w: %s", dumpErr, stderr)
		}
		return fmt.Errorf("mysqldump: %w", dumpErr)
	}
	if gzipErr != nil {
		return fmt.Errorf("gzip: %w", gzipErr)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	ok = true
	return nil
}
