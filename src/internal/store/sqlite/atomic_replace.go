package sqlite

import (
	"context"
	"fmt"
	"regexp"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.AtomicReplacer = (*Store)(nil)

// validIdentifier guards the table names AtomicReplace interpolates directly
// into DDL — SQLite cannot bind identifiers as query parameters, so this is
// the injection defense for callers that ever pass anything less trusted than
// a hardcoded constant. Mirrors the mysql backend's identical guard.
var validIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// createTableNameRe matches the table-name token immediately following
// "CREATE TABLE [IF NOT EXISTS]" at the start of a sqlite_master.sql dump, so
// AtomicReplace can rename it to a shadow table without disturbing anything
// else in the DDL body (column names, CHECK constraints, etc).
var createTableNameRe = regexp.MustCompile(`(?is)^(CREATE TABLE\s+(?:IF NOT EXISTS\s+)?)"?([a-zA-Z_][a-zA-Z0-9_]*)"?`)

// tableCreateSQL returns table's original CREATE TABLE statement from
// sqlite_master — used to clone its exact column declarations (including
// decltypes like DATETIME, which drive this backend's automatic time.Time
// scanning) into a shadow table.
func (s *Store) tableCreateSQL(ctx context.Context, table string) (string, error) {
	var sqlText string
	err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&sqlText)
	if err != nil {
		return "", fmt.Errorf("read schema for %s: %w", table, err)
	}
	return sqlText, nil
}

// AtomicReplace implements [store.AtomicReplacer] for SQLite: build populates
// a shadow "<table>_new" table (cloned from table's own schema so column
// decltypes match exactly), then a transactional double-rename swaps it in.
// Wrapping the swap in a transaction matters because SQLite (unlike MySQL)
// has no single statement that renames two tables atomically — two
// independent ALTER TABLE...RENAME statements would leave a window where
// table does not exist at all. SQLite DDL is transactional, so bracketing
// both renames in one BEGIN/COMMIT closes that window: a concurrent reader
// on this backend's pinned single connection simply blocks for the pool
// slot until COMMIT, then sees the fully-swapped table — never a missing or
// empty one.
func (s *Store) AtomicReplace(ctx context.Context, table string, build func(ctx context.Context, into string) error) error {
	if !validIdentifier.MatchString(table) {
		return fmt.Errorf("atomic replace: invalid table name %q", table)
	}
	newTable := table + "_new"
	oldTable := table + "_old"

	// Clean up shadow tables left behind by a prior crashed run.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+newTable); err != nil {
		return fmt.Errorf("atomic replace %s: drop stale %s: %w", table, newTable, err)
	}

	createSQL, err := s.tableCreateSQL(ctx, table)
	if err != nil {
		return fmt.Errorf("atomic replace %s: %w", table, err)
	}
	if !createTableNameRe.MatchString(createSQL) {
		return fmt.Errorf("atomic replace %s: could not parse table name out of schema %q", table, createSQL)
	}
	newCreateSQL := createTableNameRe.ReplaceAllString(createSQL, "${1}"+newTable)
	if _, err := s.db.ExecContext(ctx, newCreateSQL); err != nil {
		return fmt.Errorf("atomic replace %s: create %s: %w", table, newTable, err)
	}

	if err := build(ctx, newTable); err != nil {
		_, _ = s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+newTable)
		return fmt.Errorf("atomic replace %s: build: %w", table, err)
	}

	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+oldTable); err != nil {
		return fmt.Errorf("atomic replace %s: drop stale %s: %w", table, oldTable, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("atomic replace %s: begin swap: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+oldTable); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("atomic replace %s: rename old: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+newTable+" RENAME TO "+table); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("atomic replace %s: rename new: %w", table, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("atomic replace %s: commit swap: %w", table, err)
	}

	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+oldTable); err != nil {
		return fmt.Errorf("atomic replace %s: drop old %s: %w", table, oldTable, err)
	}
	return nil
}
