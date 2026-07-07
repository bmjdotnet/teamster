package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.SchemaInspector = (*Store)(nil)

// rowQuerier is satisfied by both *sql.DB and *sql.Conn — the schema
// inspector doesn't care which one it runs on.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// mysqlSchemaInspector implements [store.SchemaInspector] over
// information_schema — the same table/column/index questions
// execMigrationStmt has always asked, exposed as a named capability so the
// migration runner (and Phase 15's cross-backend conformance suite) can call
// them without reaching into mysql-package internals.
type mysqlSchemaInspector struct{ q rowQuerier }

func (i mysqlSchemaInspector) TableExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := i.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`,
		name,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("table-exists check %s: %w", name, err)
	}
	return n > 0, nil
}

func (i mysqlSchemaInspector) ColumnExists(ctx context.Context, table, column string) (bool, error) {
	var n int
	err := i.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("column-exists check %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

func (i mysqlSchemaInspector) IndexExists(ctx context.Context, table, index string) (bool, error) {
	var n int
	err := i.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		table, index,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("index-exists check %s.%s: %w", table, index, err)
	}
	return n > 0, nil
}

// TableExists, ColumnExists, IndexExists on *Store surface the same
// [store.SchemaInspector] capability at the connection-pool level (used by
// tests and any future caller that doesn't hold the migration lock).
func (s *Store) TableExists(ctx context.Context, name string) (bool, error) {
	return mysqlSchemaInspector{q: s.db}.TableExists(ctx, name)
}

func (s *Store) ColumnExists(ctx context.Context, table, column string) (bool, error) {
	return mysqlSchemaInspector{q: s.db}.ColumnExists(ctx, table, column)
}

func (s *Store) IndexExists(ctx context.Context, table, index string) (bool, error) {
	return mysqlSchemaInspector{q: s.db}.IndexExists(ctx, table, index)
}
