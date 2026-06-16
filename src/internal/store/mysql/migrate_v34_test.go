package mysql

import (
	"context"
	"database/sql"
	"testing"
)

// TestMigrateV34_HostUserCapture verifies migration v34: it adds a `username`
// VARCHAR(64) column to token_ledger and sessions (after the existing `host`
// column) so the focus-attribution recovery pass can route a session to the
// correct host+user-local ~/.claude transcript home. The column must be absent
// before v34 and present (NOT NULL, default '', length 64) on both tables after.
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so it does not contend with other schema-creating suites on the migrate
// advisory lock.
func TestMigrateV34_HostUserCapture(t *testing.T) {
	// Migrate up to v33 so both tables exist without the username column.
	db := freshBackfillDB(t, 33)
	ctx := context.Background()

	for _, tbl := range []string{"token_ledger", "sessions"} {
		if columnExistsDB(t, db, tbl, "username") {
			t.Fatalf("pre-v34 %s.username must not exist", tbl)
		}
		// The AFTER anchor must already be present.
		if !columnExistsDB(t, db, tbl, "host") {
			t.Fatalf("pre-v34 %s.host (the AFTER anchor) is missing", tbl)
		}
	}

	// Apply v34.
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v34: %v", err)
	}

	// v34 recorded exactly once.
	var v34Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 34`).Scan(&v34Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v34Recorded != 1 {
		t.Fatalf("v34 recorded %d times in schema_version, want 1", v34Recorded)
	}

	// Both columns now exist as VARCHAR(64).
	for _, tbl := range []string{"token_ledger", "sessions"} {
		if !columnExistsDB(t, db, tbl, "username") {
			t.Fatalf("post-v34 %s.username is missing", tbl)
		}
		if got := columnLength(t, db, tbl, "username"); got != 64 {
			t.Fatalf("post-v34 %s.username length = %d, want 64", tbl, got)
		}
		nullable, def := columnNullableDefault(t, db, tbl, "username")
		if nullable {
			t.Errorf("%s.username should be NOT NULL", tbl)
		}
		if def != "" {
			t.Errorf("%s.username default = %q, want empty string", tbl, def)
		}
	}

	// An INSERT that omits username gets '' (the default), proving NOT NULL +
	// default '' lets unstamped legacy writers keep working.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (session_id, agent_name, host, status, first_seen, last_seen)
		VALUES ('s-v34', '', 'hub-1', 'active', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))`); err != nil {
		t.Fatalf("insert session without username: %v", err)
	}
	var gotUser string
	if err := db.QueryRowContext(ctx,
		`SELECT username FROM sessions WHERE session_id = 's-v34'`).Scan(&gotUser); err != nil {
		t.Fatalf("read back username: %v", err)
	}
	if gotUser != "" {
		t.Fatalf("default username = %q, want empty string", gotUser)
	}
}

// TestMigrateV34_Idempotent re-runs migrate() on an already-current schema and
// asserts v34's guarded ADD COLUMN is a clean no-op: no error, schema_version
// unchanged, both username columns still present at VARCHAR(64).
func TestMigrateV34_Idempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion)
	ctx := context.Background()

	before := schemaVersionRows(t, db)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op (guarded ADD COLUMN), got: %v", err)
	}
	after := schemaVersionRows(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before max=v%d count=%d, after max=v%d count=%d",
			before.maxV, before.count, after.maxV, after.count)
	}
	for _, tbl := range []string{"token_ledger", "sessions"} {
		if got := columnLength(t, db, tbl, "username"); got != 64 {
			t.Fatalf("%s.username length after idempotent re-run = %d, want 64", tbl, got)
		}
	}
}

// columnExistsDB reports whether table has column in the current schema, against
// a *sql.DB (the migrations.go columnExists takes a *sql.Conn).
func columnExistsDB(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&n); err != nil {
		t.Fatalf("column-exists check %s.%s: %v", table, column, err)
	}
	return n > 0
}

// columnNullableDefault returns whether a column is nullable and its column
// default (empty string for DEFAULT '') in the current schema.
func columnNullableDefault(t *testing.T, db *sql.DB, table, column string) (nullable bool, def string) {
	t.Helper()
	var isNullable string
	var colDefault sql.NullString
	if err := db.QueryRow(`
		SELECT IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&isNullable, &colDefault); err != nil {
		t.Fatalf("nullable/default check %s.%s: %v", table, column, err)
	}
	return isNullable == "YES", colDefault.String
}
