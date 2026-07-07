package backup

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
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

	if err := os.MkdirAll(filepath.Join(destDir, "mysql"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir mysql: %w", err)
	}

	var errs []string
	for _, db := range cfg.Databases {
		outPath := filepath.Join(destDir, "mysql", db+".sql.gz")
		if err := dumpDatabase(ctx, cfg.DSN, db, outPath); err != nil {
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

// dumpDatabase opens db via store.Open (retargeted to the given database name
// per dsnForDatabase) and dumps it through the backend's BackupEngine — the
// mysqldump/gzip shell-out mechanism itself is unchanged, just relocated
// behind the admin-plane interface in internal/store/mysql (ADR-2).
func dumpDatabase(ctx context.Context, rawDSN, db, outPath string) error {
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
	return be.Dump(ctx, outPath)
}

// dsnForDatabase returns rawDSN with its path (database name) replaced by db.
// mysqldump can target any database the connection's credentials can reach,
// independent of which database the DSN's own path names — cfg.Databases may
// list several schemas sharing one set of credentials.
func dsnForDatabase(rawDSN, db string) (string, error) {
	u, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	u.Path = "/" + db
	return u.String(), nil
}
