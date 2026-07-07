// Admin-plane implementations: RawExecutor (ADR-3) and BackupEngine (ADR-2,
// via VACUUM INTO). Neither is part of store.Store — callers discover them by
// type-assertion. Per 07-conformance.md, a backend need not implement every
// admin-plane interface; DemoSeeder and CredentialProber are skipped here
// (DemoSeeder's bulk-ledger seeding has no sqlite-specific value for a
// validation backend; CredentialProber's distinct-credential probe is a
// MySQL-deployment concept — SQLite has no server-side user/grant system).
package sqlite

import (
	"context"

	"github.com/bmjdotnet/teamster/internal/store"
)

var (
	_ store.RawExecutor  = (*Store)(nil)
	_ store.BackupEngine = (*Store)(nil)
)

// --- ADR-3: RawExecutor ---

// ExecRaw implements [store.RawExecutor]. The driver's sql.Result already
// satisfies store.RawResult structurally, so it is returned unwrapped.
func (s *Store) ExecRaw(ctx context.Context, stmt string, args ...any) (store.RawResult, error) {
	return s.db.ExecContext(ctx, stmt, args...)
}

// QueryRaw implements [store.RawExecutor]. The driver's *sql.Rows already
// satisfies store.RawRows structurally, so it is returned unwrapped.
func (s *Store) QueryRaw(ctx context.Context, query string, args ...any) (store.RawRows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// --- ADR-2: BackupEngine ---

// Dump implements [store.BackupEngine] via SQLite's native VACUUM INTO,
// which writes a fully consistent, defragmented copy of the live database to
// dest in one step — no external tool, no CGO.
func (s *Store) Dump(ctx context.Context, dest string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}

// Restore implements [store.BackupEngine] by replacing this Store's live
// database contents with src's, table by table, inside one transaction: for
// a file-backed store this re-derives the same end state as swapping in the
// backup file wholesale, without requiring callers to reopen the Store
// against a new path (an in-memory Store, in particular, has no path to
// swap).
func (s *Store) Restore(ctx context.Context, src string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `ATTACH DATABASE ? AS backup_src`, src); err != nil {
		return err
	}
	defer s.db.ExecContext(ctx, `DETACH DATABASE backup_src`) //nolint:errcheck

	rows, err := tx.QueryContext(ctx, `SELECT name FROM backup_src.sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close() //nolint:errcheck

	for _, table := range tables {
		if !validIdentifier.MatchString(table) {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" SELECT * FROM backup_src."+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Verify implements [store.BackupEngine] by opening src read-only and
// running SQLite's built-in integrity check.
func (s *Store) Verify(ctx context.Context, src string) error {
	tmp, err := New(src, store.WithSkipMigrate())
	if err != nil {
		return err
	}
	defer tmp.Close() //nolint:errcheck

	var result string
	if err := tmp.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return &integrityError{result: result}
	}
	return nil
}

type integrityError struct{ result string }

func (e *integrityError) Error() string { return "sqlite integrity check failed: " + e.result }
