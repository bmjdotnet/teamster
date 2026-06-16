package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestMigrateV33_WidenAttributionMethod verifies migration v33: it widens
// usage_attribution.method from VARCHAR(32) to VARCHAR(48) so the focus-
// attribution recovery labels fit. 'temporal_join_lead_session_fallback' (the
// P1a lead-session fallback) is 35 chars and overflowed the old VARCHAR(32) with
// Error 1406; after v33 it must INSERT cleanly and round-trip intact.
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so it does not contend with other schema-creating suites on the migrate
// advisory lock.
func TestMigrateV33_WidenAttributionMethod(t *testing.T) {
	// Migrate up to v32 so usage_attribution exists at its pre-widen VARCHAR(32).
	db := freshBackfillDB(t, 32)
	ctx := context.Background()

	// Pre-v33 the column is VARCHAR(32).
	if got := columnLength(t, db, "usage_attribution", "method"); got != 32 {
		t.Fatalf("pre-v33 method column length = %d, want 32", got)
	}

	// Apply v33.
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v33: %v", err)
	}

	// v33 recorded exactly once.
	var v33Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 33`).Scan(&v33Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v33Recorded != 1 {
		t.Fatalf("v33 recorded %d times in schema_version, want 1", v33Recorded)
	}

	// Column widened to 48.
	if got := columnLength(t, db, "usage_attribution", "method"); got != 48 {
		t.Fatalf("post-v33 method column length = %d, want 48", got)
	}

	// The 35-char P1a label now fits (this INSERT was Error 1406 pre-v33).
	const label = "temporal_join_lead_session_fallback"
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_attribution
			(message_id, entity_type, entity_id, weight, method, computed_at)
		VALUES ('msg-p1a', 'outcome', 'o1', 1.0, ?, ?)`, label, base); err != nil {
		t.Fatalf("insert 35-char method label after v33: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx,
		`SELECT method FROM usage_attribution WHERE message_id = 'msg-p1a'`).Scan(&got); err != nil {
		t.Fatalf("read back method: %v", err)
	}
	if got != label {
		t.Fatalf("method round-trip = %q, want %q (not truncated)", got, label)
	}
}

// TestMigrateV33_Idempotent re-runs migrate() on an already-current schema and
// asserts v33's MODIFY-to-same-width is a clean no-op: no error, schema_version
// unchanged, column still VARCHAR(48).
func TestMigrateV33_Idempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion)
	ctx := context.Background()

	before := schemaVersionRows(t, db)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op (MODIFY to same width), got: %v", err)
	}
	after := schemaVersionRows(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before max=v%d count=%d, after max=v%d count=%d",
			before.maxV, before.count, after.maxV, after.count)
	}
	if got := columnLength(t, db, "usage_attribution", "method"); got != 48 {
		t.Fatalf("method column length after idempotent re-run = %d, want 48", got)
	}
}

// columnLength returns CHARACTER_MAXIMUM_LENGTH for a VARCHAR column in the
// current schema.
func columnLength(t *testing.T, db *sql.DB, table, column string) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRow(`
		SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&n); err != nil {
		t.Fatalf("column-length check %s.%s: %v", table, column, err)
	}
	if !n.Valid {
		t.Fatalf("column %s.%s has no CHARACTER_MAXIMUM_LENGTH (not a string column?)", table, column)
	}
	return n.Int64
}
