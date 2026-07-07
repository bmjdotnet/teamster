package mysql

import (
	"context"
	"fmt"
	"regexp"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.AtomicReplacer = (*Store)(nil)

// validIdentifier guards the table names AtomicReplace interpolates directly
// into DDL — MySQL cannot bind identifiers as query parameters, so this is
// the injection defense for callers that ever pass anything less trusted than
// a hardcoded constant.
var validIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// AtomicReplace implements [store.AtomicReplacer] for MySQL: build populates a
// shadow "<table>_new" table, then a single multi-table RENAME swaps it in.
// RENAME TABLE is atomic in MySQL/MariaDB — a concurrent reader of table sees
// either the full old rows or the full new rows, never an empty or missing
// table, unlike the TRUNCATE-in-tx pattern this replaces (TRUNCATE
// auto-commits in InnoDB, so that transaction wrapper was never real).
func (s *Store) AtomicReplace(ctx context.Context, table string, build func(ctx context.Context, into string) error) error {
	if !validIdentifier.MatchString(table) {
		return fmt.Errorf("atomic replace: invalid table name %q", table)
	}
	newTable := table + "_new"
	oldTable := table + "_old"

	// Clean up shadow tables left behind by a prior crashed run before we
	// start — CREATE TABLE LIKE below requires newTable not to exist yet.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+newTable); err != nil {
		return fmt.Errorf("atomic replace %s: drop stale %s: %w", table, newTable, err)
	}
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE "+newTable+" LIKE "+table); err != nil {
		return fmt.Errorf("atomic replace %s: create %s: %w", table, newTable, err)
	}

	if err := build(ctx, newTable); err != nil {
		_, _ = s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+newTable)
		return fmt.Errorf("atomic replace %s: build: %w", table, err)
	}

	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+oldTable); err != nil {
		return fmt.Errorf("atomic replace %s: drop stale %s: %w", table, oldTable, err)
	}
	// The swap: a single atomic multi-table RENAME. Never a DROP+CREATE or a
	// two-statement rename — either both renames land or neither does, and
	// table is never briefly absent.
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("RENAME TABLE %s TO %s, %s TO %s", table, oldTable, newTable, table)); err != nil {
		return fmt.Errorf("atomic replace %s: swap: %w", table, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+oldTable); err != nil {
		return fmt.Errorf("atomic replace %s: drop old %s: %w", table, oldTable, err)
	}
	return nil
}
