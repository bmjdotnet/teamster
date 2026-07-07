package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// mysqlMigrator implements store.Migrator by wrapping the primitives migrate()
// has always used (migrateLockName/migrateLockTimeout, the migrations slice,
// execMigrationStmt's idempotency guard) — a representation change only, per
// 04-migrations.md Invariant 2: the lock name/semantics, the schema_version
// table, and every step's SQL/Func are byte-identical to what migrate() ran.
//
// migrate()/runMigrations(ctx, conn, db) are deliberately left in place,
// unmodified, alongside this type rather than rewritten to delegate to it:
// several existing tests (TestMigrateRerunIdempotent,
// TestMigratePartialApplyRecovery, TestMigrateRefusesNewerSchema) call them
// directly to exercise specific recovery/guard scenarios, and this is a
// schema-drift-sensitive area (see memory live-hub-schema-v46-ahead-of-
// binaries) where duplicating ~15 lines of lock boilerplate is a smaller risk
// than touching already-proven code paths. New() below is the only production
// caller, and it now goes through RunMigrations/mysqlMigrator exclusively.
type mysqlMigrator struct {
	db   *sql.DB
	conn *sql.Conn // set by Lock; every other method requires Lock to have run first
}

var _ store.Migrator = (*mysqlMigrator)(nil)

func newMysqlMigrator(db *sql.DB) *mysqlMigrator {
	return &mysqlMigrator{db: db}
}

// Lock pins a connection and takes the same named advisory lock migrate() has
// always used — identical name and timeout — so a fleet mid-rollout (old
// migrate()-based binaries and new Migrator-based binaries) still mutually
// excludes on the same lock.
func (m *mysqlMigrator) Lock(ctx context.Context) (func() error, error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migrate connection: %w", err)
	}
	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT GET_LOCK(?, ?)`, migrateLockName, int(migrateLockTimeout.Seconds()),
	).Scan(&locked); err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("acquire migrate lock: %w", err)
	}
	// GET_LOCK returns 1 on success, 0 on timeout, NULL on error.
	if !locked.Valid || locked.Int64 != 1 {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("acquire migrate lock %q: timed out after %s (another migration in progress)", migrateLockName, migrateLockTimeout)
	}
	m.conn = conn
	return func() error {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, relErr := conn.ExecContext(relCtx, `SELECT RELEASE_LOCK(?)`, migrateLockName)
		closeErr := conn.Close()
		if relErr != nil {
			return relErr
		}
		return closeErr
	}, nil
}

// CurrentVersion reads the same schema_version table migrate() reads,
// creating it first if absent — byte-identical DDL to runMigrations, so on a
// hub that already has the table this is a no-op read, never a reshape.
func (m *mysqlMigrator) CurrentVersion(ctx context.Context) (int, error) {
	if _, err := m.conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INT NOT NULL PRIMARY KEY,
			name    VARCHAR(128) NOT NULL,
			applied_at DATETIME(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return 0, fmt.Errorf("create schema_version: %w", err)
	}
	var maxV sql.NullInt64
	if err := m.conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxV); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	return int(maxV.Int64), nil
}

// SetVersion records an applied step exactly as runMigrations always did.
func (m *mysqlMigrator) SetVersion(ctx context.Context, v int, name string) error {
	_, err := m.conn.ExecContext(ctx,
		`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))`, v, name)
	return err
}

// Exec implements store.Execer for portable SQL steps by running query
// through execMigrationStmt — the same information_schema ADD-COLUMN
// idempotency guard migrate() has always applied. Only invoked by the shared
// runner with the plain migration statements (no args); the args branch is
// defensive for future v51+ Func steps that might parameterize a query.
func (m *mysqlMigrator) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if len(args) == 0 {
		if err := execMigrationStmt(ctx, m.conn, query); err != nil {
			return 0, err
		}
		return 0, nil
	}
	res, err := m.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Query implements store.Execer for Func steps that need to read through the
// locked connection. No current migration Func uses this (they operate on
// the captured *sql.DB directly, per Steps() below) — present for interface
// conformance and future v51+ Func steps.
func (m *mysqlMigrator) Query(ctx context.Context, query string, args ...any) (store.Rows, error) {
	return m.conn.QueryContext(ctx, query, args...)
}

// Steps adapts the existing migrations slice into []store.Migration.
// Version/Name/Stmts carry over unchanged (Invariant 1: no renumbering, no
// resplitting, no schema change). A step's legacy Func (backfillV1ToV3,
// backfillWmsIntervals, mergeProjectToProduct) takes a *sql.DB so it can run
// its own multi-statement transaction — that signature predates store.Execer
// and narrowing it would mean rewriting already-tested transactional logic,
// so the adapter closes over m.db and ignores the store.Execer argument the
// runner passes it. Concurrency safety is unaffected: the legacy Func still
// runs serialized under the same advisory lock (losers block in Lock above).
func (m *mysqlMigrator) Steps() []store.Migration {
	out := make([]store.Migration, 0, len(migrations))
	for _, step := range migrations {
		s := store.Migration{
			Version: step.Version,
			Name:    step.Name,
			SQL:     step.Stmts,
		}
		if step.Func != nil {
			legacy := step.Func
			db := m.db
			s.Func = func(ctx context.Context, _ store.Execer) error {
				return legacy(ctx, db)
			}
		}
		out = append(out, s)
	}
	return out
}
