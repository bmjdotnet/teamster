// Package sqlite is the SQLite-backed [store.Store] implementation, on
// modernc.org/sqlite (pure Go, no CGO). It exists as the kit's validation
// backend (03-architecture/07-conformance.md): proving the store.Store
// abstraction survives contact with a genuinely different engine, not as a
// production target. See that file's "Certificate scope" section for what a
// green conformance run here does and does not certify.
//
// DSN form: sqlite://PATH, where PATH is a filesystem path or the literal
// ":memory:" for an in-memory database (sqlite://:memory:). A bare path with
// no scheme (as New's first argument) is also accepted directly.
//
// The connection pool is deliberately pinned to a single connection
// (SetMaxOpenConns(1)): SQLite has no true row-level locking, so rather than
// fake MySQL's FOR UPDATE semantics, every operation is serialized through one
// *sql.DB connection and SQLite's own transaction isolation does the rest.
// This also makes ":memory:" databases behave correctly — without a pinned
// single connection, database/sql's pool would open a second, independent,
// empty in-memory database on the next connection it acquires.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// memDBCounter gives every ":memory:" Store instance its own uniquely-named
// SQLite in-memory database. SQLite's shared-cache mode (cache=shared, which
// this backend also needs for a *single* Store's own connection semantics)
// keys shared memory databases by NAME — a bare "file::memory:?cache=shared"
// URI is the same name for every caller in the process, so two independent
// New(":memory:") calls would silently see and mutate the SAME database
// (verified empirically: this is not a theoretical risk). Each Store must be
// its own isolated logical database unless a caller explicitly asks to share
// one by DSN.
var memDBCounter int64

var _ store.Store = (*Store)(nil)
var _ store.AtomicReplacer = (*Store)(nil)
var _ store.RawExecutor = (*Store)(nil)

// Store is the SQLite-backed implementation of [store.Store].
type Store struct {
	db                *sql.DB
	requireTagsOnDone bool
	skipMigrate       bool
	path              string // resolved filesystem path, or ":memory:"
}

// pathFromDSN extracts the filesystem path (or ":memory:") from a
// sqlite://PATH DSN, or returns raw unchanged if it carries no scheme —
// New is also called directly by tests/embedders with a bare path.
func pathFromDSN(raw string) string {
	if p, ok := strings.CutPrefix(raw, "sqlite://"); ok {
		if p == "" || p == ":memory:" {
			return ":memory:"
		}
		return p
	}
	return raw
}

// New opens a SQLite database at path (a filesystem path, or ":memory:" for
// an in-memory database) and runs migrations. Optional store.Options set
// behavior flags (e.g. store.WithRequireTagsOnDone).
func New(dsn string, opts ...store.Option) (*Store, error) {
	path := pathFromDSN(dsn)

	// _time_format=sqlite writes time.Time values in SQLite's own recognized
	// date/time format (sqlite.org/lang_datefunc.html format 4:
	// "YYYY-MM-DD HH:MM:SS.SSS...+-HH:MM", full nanosecond precision via Go's
	// ".999999999" verb) instead of the driver's default Go-flavored
	// time.Time.String() text. Without this, columns declared DATETIME still
	// round-trip correctly through database/sql's Scan (the driver tries
	// several parse formats on read), but SQLite's OWN SQL-level date/time
	// functions (date(), strftime(), julianday()) cannot parse the default
	// format — this backend's reporting/rollup queries need those functions,
	// so writes must land in a format SQLite itself can read back in SQL, not
	// just one this driver's Go-side Scan can parse.
	const timeFormatParam = "_time_format=sqlite"
	driverDSN := path
	if path == ":memory:" {
		// A uniquely-named shared-cache memory database (see memDBCounter's
		// doc comment) — NOT the bare "file::memory:" form, which is one
		// shared anonymous database for the whole process.
		n := atomic.AddInt64(&memDBCounter, 1)
		driverDSN = fmt.Sprintf("file:sqlite-store-memdb-%d-%d?mode=memory&cache=shared&%s",
			time.Now().UnixNano(), n, timeFormatParam)
	} else {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		driverDSN = path + sep + timeFormatParam
	}

	db, err := sql.Open("sqlite", driverDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite has no server-side connection pool; pinning to one connection
	// serializes all access (the "single-writer" outcome the design calls
	// for) and is required for ":memory:" databases to behave as one
	// logical database rather than a fresh empty one per connection.
	db.SetMaxOpenConns(1)

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.ExecContext(pingCtx, `PRAGMA foreign_keys = ON`); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("enable foreign_keys: %w", err)
	}
	if _, err := db.ExecContext(pingCtx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	var so store.Options
	for _, opt := range opts {
		opt(&so)
	}
	s := &Store{db: db, requireTagsOnDone: so.RequireTagsOnDone, skipMigrate: so.SkipMigrate, path: path}
	if !s.skipMigrate {
		migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer migrateCancel()
		if err := store.RunMigrations(migrateCtx, newSQLiteMigrator(db)); err != nil {
			db.Close() //nolint:errcheck
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return s, nil
}

func init() {
	store.Register("sqlite", Open)
}

// Open constructs a [store.Store] from dsn — the registry entry point for the
// "sqlite" scheme registered by init above.
func Open(ctx context.Context, dsn string, opts ...store.Option) (store.Store, error) {
	return New(dsn, opts...)
}

// Close releases the underlying connection.
func (s *Store) Close() error { return s.db.Close() }

// Ping implements [store.Prober]. SQLite has no connection to ping in the
// network sense (01-interfaces.md's Prober subsection, F10): "reachable"
// means the database file opens and is readable, which PingContext against
// our pinned connection already establishes.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// nowUTC mirrors the mysql backend's convention: Go always writes UTC
// time.Time values; no DDL DEFAULT clauses on timestamps.
func nowUTC() time.Time { return time.Now().UTC() }

// --- error classification ---
//
// SQLite result codes (sqlite.org/rescode.html): with extended result codes
// enabled (modernc.org/sqlite turns them on for every connection), a
// constraint violation's Code() has primary code SQLITE_CONSTRAINT (19) in
// its low byte, with the specific sub-kind in the high bytes — e.g.
// SQLITE_CONSTRAINT_UNIQUE = 2067, SQLITE_CONSTRAINT_PRIMARYKEY = 1555.
// These are the exact two sub-kinds 07-conformance.md's error-sentinel
// dimension names for ErrConflict.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

// classifyConflict maps a SQLite UNIQUE/PRIMARY KEY constraint violation
// (the uq_open collisions interval writes can hit, mirroring the mysql
// backend's 1062 duplicate-key mapping) onto store.ErrConflict, so callers
// branch via errors.Is instead of matching driver text.
func classifyConflict(op string, err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return store.Conflict(op, err)
		}
	}
	return err
}

// requireOneRow mirrors the mysql backend's helper: a zero-row UPDATE/DELETE
// on a "must exist" path is ErrNotFound.
func requireOneRow(res sql.Result, op, entityType, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.NotFound(op, entityType, id)
	}
	return nil
}

// validTagEntityType reports whether entityType may carry tags — identical
// contract to the mysql backend.
func validTagEntityType(entityType string) error {
	switch entityType {
	case wms.EntityOutcome, wms.EntityWorkUnit, wms.EntityInterval:
		return nil
	default:
		return fmt.Errorf("unknown entity type: %s", entityType)
	}
}

// statusTableName maps an entity type to the base table whose status cache
// the event-record machinery keeps current — identical contract to the
// mysql backend.
func statusTableName(entityType string) (string, error) {
	switch entityType {
	case wms.EntityOutcome:
		return "outcomes", nil
	case wms.EntityWorkUnit:
		return "workunits", nil
	default:
		return "", fmt.Errorf("unknown entity type: %s", entityType)
	}
}

// maxTagDescriptionLen mirrors the mysql backend's tags.description guard
// (v31 widened the column to 1024 runes).
const maxTagDescriptionLen = 1024

// checkTagDescriptionLen returns a clear over-length error, else nil —
// identical contract to the mysql backend.
func checkTagDescriptionLen(description string) error {
	if n := len([]rune(description)); n > maxTagDescriptionLen {
		return fmt.Errorf("description too long: %d chars (max %d)", n, maxTagDescriptionLen)
	}
	return nil
}
