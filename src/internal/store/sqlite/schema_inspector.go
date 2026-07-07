package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.SchemaInspector = (*Store)(nil)

// rowQuerier is satisfied by both *sql.DB and *sql.Conn.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqliteSchemaInspector implements [store.SchemaInspector] over
// sqlite_master/pragma_table_info — SQLite has no information_schema.
// Defense-in-depth only: every DDL statement in this package's migrations
// already uses "CREATE TABLE IF NOT EXISTS" / guards a column add, so these
// are rarely the primary safety net (04-migrations.md's "Drift-guard
// portability").
type sqliteSchemaInspector struct{ q rowQuerier }

func (i sqliteSchemaInspector) TableExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := i.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("table-exists check %s: %w", name, err)
	}
	return n > 0, nil
}

func (i sqliteSchemaInspector) ColumnExists(ctx context.Context, table, column string) (bool, error) {
	var n int
	err := i.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("column-exists check %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

func (i sqliteSchemaInspector) IndexExists(ctx context.Context, table, index string) (bool, error) {
	var n int
	err := i.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND name = ?`, table, index,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("index-exists check %s.%s: %w", table, index, err)
	}
	return n > 0, nil
}

// TableExists, ColumnExists, IndexExists on *Store surface the same
// [store.SchemaInspector] capability at the connection-pool level.
func (s *Store) TableExists(ctx context.Context, name string) (bool, error) {
	return sqliteSchemaInspector{q: s.db}.TableExists(ctx, name)
}

func (s *Store) ColumnExists(ctx context.Context, table, column string) (bool, error) {
	return sqliteSchemaInspector{q: s.db}.ColumnExists(ctx, table, column)
}

func (s *Store) IndexExists(ctx context.Context, table, index string) (bool, error) {
	return sqliteSchemaInspector{q: s.db}.IndexExists(ctx, table, index)
}
