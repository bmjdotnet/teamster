package store

import (
	"context"
	"fmt"
)

// Migrator is the per-backend half of the migration framework: the runner
// below (RunMigrations) is shared and backend-agnostic; each backend supplies
// a Migrator implementing the four primitives the runner needs plus its
// ordered step list. See 03-architecture/04-migrations.md "The Migrator
// contract".
//
// Migrator embeds Execer so a Migration.Func step can run its statements
// through the same locked connection/transaction the runner holds — a Func
// cannot escape onto an unlocked connection because the only Execer it is
// ever given is the Migrator itself.
type Migrator interface {
	Execer

	// Lock serializes migration across concurrent processes opening the store
	// at the same time. MySQL: GET_LOCK/RELEASE_LOCK on a pinned connection.
	// A backend with a genuine single-writer guarantee (e.g. SQLite) may
	// implement this as a no-op and document that assumption.
	Lock(ctx context.Context) (unlock func() error, err error)

	// CurrentVersion reads the backend's schema_version table (or equivalent)
	// for the highest applied version. Called only after Lock succeeds.
	CurrentVersion(ctx context.Context) (int, error)

	// SetVersion records that version v (named name) has been applied.
	// Called only after that step's statements/Func have succeeded.
	SetVersion(ctx context.Context, v int, name string) error

	// Steps returns the ordered migration list this backend applies.
	Steps() []Migration
}

// Migration is one versioned step. A step may set SQL, Func, both, or
// neither; SQL statements (if any) run first, then Func (if set). Version
// numbers are immutable history once shipped — see 04-migrations.md
// Invariant 1.
type Migration struct {
	Version int
	Name    string
	// SQL is portable statements the backend runs verbatim. Empty when the
	// step needs dialect-specific handling.
	SQL []string
	// Func is a backend-implemented step for anything not expressible
	// portably (engine-specific DDL, dialect-function backfills). It receives
	// the Migrator itself as its Execer.
	Func func(ctx context.Context, x Execer) error
}

// Execer is the minimal exec surface a Migration.Func step gets — no raw
// driver handle, so it cannot escape the migration's connection/transaction.
type Execer interface {
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	Query(ctx context.Context, query string, args ...any) (Rows, error)
}

// Rows is the minimal result-set surface Execer.Query returns — the same
// shape as [database/sql.Rows] restricted to what a Func step needs.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// SchemaInspector answers the three idempotent-DDL-guard questions a
// migration runner needs: does this table/column/index already exist? A
// backend whose DDL is idempotent by construction (CREATE TABLE IF NOT
// EXISTS, ADD COLUMN IF NOT EXISTS) may implement these trivially.
type SchemaInspector interface {
	TableExists(ctx context.Context, name string) (bool, error)
	ColumnExists(ctx context.Context, table, column string) (bool, error)
	IndexExists(ctx context.Context, table, index string) (bool, error)
}

// RunMigrations locks, reads the current version, refuses to run against a
// schema newer than this binary knows (closes the incident class in memory
// live-hub-schema-v46-ahead-of-binaries), then applies every unapplied step
// in order. This preserves the exact safety property the MySQL-specific
// migrate() had: lock the whole run, apply in order under the lock, record
// version under the lock.
func RunMigrations(ctx context.Context, m Migrator) error {
	unlock, err := m.Lock(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire migration lock: %w", err)
	}
	defer unlock() //nolint:errcheck

	cur, err := m.CurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: read current schema version: %w", err)
	}

	steps := m.Steps()
	maxKnown := 0
	for _, s := range steps {
		if s.Version > maxKnown {
			maxKnown = s.Version
		}
	}
	if cur > maxKnown {
		return fmt.Errorf("store: schema v%d is newer than this binary supports (max known v%d) — upgrade the binary", cur, maxKnown)
	}

	for _, step := range steps {
		if step.Version <= cur {
			continue
		}
		if err := applyStep(ctx, m, step); err != nil {
			return fmt.Errorf("migration v%d %q: %w", step.Version, step.Name, err)
		}
		if err := m.SetVersion(ctx, step.Version, step.Name); err != nil {
			return fmt.Errorf("store: record v%d: %w", step.Version, err)
		}
	}
	return nil
}

// applyStep runs one step's SQL statements (if any) then its Func (if set),
// both through m as the Execer so neither can escape the locked connection.
func applyStep(ctx context.Context, m Migrator, step Migration) error {
	for _, stmt := range step.SQL {
		if _, err := m.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	if step.Func != nil {
		if err := step.Func(ctx, m); err != nil {
			return err
		}
	}
	return nil
}
