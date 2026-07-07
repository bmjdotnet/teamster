package sqlite

import (
	"context"
	"database/sql"
	"sync"

	"github.com/bmjdotnet/teamster/internal/store"
)

// migrateMu serializes migration application across concurrent New() calls
// within one process. SQLite has no portable cross-process advisory lock
// primitive (unlike MySQL's GET_LOCK); per 04-migrations.md, "a backend with
// a genuine single-writer guarantee may implement [Lock/Unlock] as a no-op or
// a filesystem flock." A process-wide mutex is stronger than a no-op and
// covers the in-process concurrent-fresh-install race the design cares
// about; SQLite's own single-writer file locking backstops cross-process
// contention, which is out of this kit's certificate scope (07-conformance.md
// "Certificate scope").
var migrateMu sync.Mutex

// sqliteMigrator implements store.Migrator over a *sql.DB pinned to a single
// connection (see store.go's New) — so Exec/Query here run against the exact
// same serialized connection the Store itself uses after migration completes.
type sqliteMigrator struct {
	db *sql.DB
}

var _ store.Migrator = (*sqliteMigrator)(nil)

func newSQLiteMigrator(db *sql.DB) *sqliteMigrator {
	return &sqliteMigrator{db: db}
}

// Lock serializes migration across concurrent New() calls in this process.
func (m *sqliteMigrator) Lock(ctx context.Context) (func() error, error) {
	migrateMu.Lock()
	return func() error {
		migrateMu.Unlock()
		return nil
	}, nil
}

// CurrentVersion reads schema_version, creating it first if absent.
func (m *sqliteMigrator) CurrentVersion(ctx context.Context) (int, error) {
	if _, err := m.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version    INTEGER NOT NULL PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at DATETIME NOT NULL
		)`); err != nil {
		return 0, err
	}
	var maxV sql.NullInt64
	if err := m.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxV); err != nil {
		return 0, err
	}
	return int(maxV.Int64), nil
}

// SetVersion records an applied step.
func (m *sqliteMigrator) SetVersion(ctx context.Context, v int, name string) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, ?)`, v, name, nowUTC())
	return err
}

// Exec implements store.Execer for portable SQL steps and Func steps.
func (m *sqliteMigrator) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := m.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Query implements store.Execer for Func steps that need to read.
func (m *sqliteMigrator) Query(ctx context.Context, query string, args ...any) (store.Rows, error) {
	return m.db.QueryContext(ctx, query, args...)
}

// Steps returns the ordered migration list — defined in migrations.go.
func (m *sqliteMigrator) Steps() []store.Migration {
	return migrations
}

// highestKnownVersion returns the maximum version number in migrations —
// mirrors the mysql package's helper of the same name, used by this
// package's own dimension-5-style migration lifecycle test.
func highestKnownVersion() int {
	max := 0
	for _, s := range migrations {
		if s.Version > max {
			max = s.Version
		}
	}
	return max
}
