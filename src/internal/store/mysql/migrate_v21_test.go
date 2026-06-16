package mysql

import (
	"context"
	"database/sql"
	"testing"
)

// TestMigrateV21_CreatesWmsIntervals verifies migration v21 (B3 Wave 1):
// the wms_intervals table is created with the exact column set, the uq_open
// UNIQUE key, and all named indexes from B3-design §1.2 — and that v21 is
// recorded in schema_version. Additive only: v21 creates the table beside the
// old two; no reader/writer touches it yet.
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so it does not contend with other schema-creating suites on the migrate
// advisory lock.
func TestMigrateV21_CreatesWmsIntervals(t *testing.T) {
	db := freshBackfillDB(t, 21) // migrate fully through v21
	ctx := context.Background()

	// v21 recorded.
	var v21Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 21`).Scan(&v21Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v21Recorded != 1 {
		t.Fatalf("v21 recorded %d times in schema_version, want 1", v21Recorded)
	}

	// Table exists.
	if !tableExists(t, db, "wms_intervals") {
		t.Fatal("wms_intervals table not created by v21")
	}

	// Every intrinsic + assembled + identity column from §1.2 is present.
	wantCols := []string{
		"id", "kind", "entity_type", "entity_id",
		"state", "session_id", "agent_name", "host",
		"started_at", "ended_at", "duration_ms",
		"phase", "phase_source", "assembled_at", "cost_usd", "cost_tokens",
		"identity_source",
	}
	for _, c := range wantCols {
		if !v21ColumnExists(t, db, "wms_intervals", c) {
			t.Errorf("wms_intervals missing column %q", c)
		}
	}

	// uq_open is a UNIQUE key over (entity_type, entity_id, kind, ended_at).
	if !indexExists(t, db, "wms_intervals", "uq_open") {
		t.Error("wms_intervals missing uq_open index")
	}
	if !indexIsUnique(t, db, "wms_intervals", "uq_open") {
		t.Error("uq_open must be a UNIQUE index")
	}
	if got := indexColumns(t, db, "wms_intervals", "uq_open"); !equalCols(got, []string{"entity_type", "entity_id", "kind", "ended_at"}) {
		t.Errorf("uq_open columns = %v, want [entity_type entity_id kind ended_at]", got)
	}

	// All named secondary indexes from §1.2 exist.
	wantIdx := []string{
		"idx_entity_time", "idx_started_ended", "idx_ended_started",
		"idx_duration", "idx_phase", "idx_assemble",
		"idx_focus_lookup", "idx_focus_open", "idx_kind_entity",
	}
	for _, idx := range wantIdx {
		if !indexExists(t, db, "wms_intervals", idx) {
			t.Errorf("wms_intervals missing index %q", idx)
		}
	}
}

// TestMigrateV21_Idempotent re-runs migrate() on an already-v21 schema and
// asserts it is a clean no-op: no error, schema_version unchanged, and the
// table still has exactly one v21 row. CREATE TABLE IF NOT EXISTS makes the
// re-run safe (it routes through execMigrationStmt verbatim, not the ADD-COLUMN
// rewrite), and the schema_version "applied" check skips the already-recorded
// step.
func TestMigrateV21_Idempotent(t *testing.T) {
	// Migrate FULLY (through the current highest version) so nothing is left to
	// apply — then a second migrate() must be a true no-op. Stopping at v21 would
	// legitimately apply the later v23 backfill, which is correct behavior, not a
	// re-run; the v21 CREATE's own idempotency is asserted in the test above via
	// the wms_intervals-still-present check below.
	db := freshBackfillDB(t, currentSchemaVersion)
	ctx := context.Background()

	before := schemaVersionRows(t, db)

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op, got: %v", err)
	}

	after := schemaVersionRows(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before max=v%d count=%d, after max=v%d count=%d",
			before.maxV, before.count, after.maxV, after.count)
	}
	if after.maxV != currentSchemaVersion {
		t.Fatalf("schema_version max = v%d, want v%d", after.maxV, currentSchemaVersion)
	}
	if after.count != after.distinct {
		t.Fatalf("schema_version has duplicate rows: %d rows, %d distinct", after.count, after.distinct)
	}
	// The v21 table survived the no-op re-run.
	if !tableExists(t, db, "wms_intervals") {
		t.Fatal("wms_intervals table missing after idempotent re-run")
	}
}

// --- information_schema helpers (DATABASE()-scoped, portable MySQL/MariaDB) ---

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&n); err != nil {
		t.Fatalf("table-exists check %s: %v", table, err)
	}
	return n > 0
}

func v21ColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&n); err != nil {
		t.Fatalf("column-exists check %s.%s: %v", table, column, err)
	}
	return n > 0
}

func indexExists(t *testing.T, db *sql.DB, table, index string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		table, index).Scan(&n); err != nil {
		t.Fatalf("index-exists check %s.%s: %v", table, index, err)
	}
	return n > 0
}

func indexIsUnique(t *testing.T, db *sql.DB, table, index string) bool {
	t.Helper()
	// NON_UNIQUE = 0 marks a unique index on both MySQL and MariaDB.
	var nonUnique int
	if err := db.QueryRow(
		`SELECT NON_UNIQUE FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?
		 LIMIT 1`, table, index).Scan(&nonUnique); err != nil {
		t.Fatalf("index-unique check %s.%s: %v", table, index, err)
	}
	return nonUnique == 0
}

func indexColumns(t *testing.T, db *sql.DB, table, index string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT COLUMN_NAME FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?
		 ORDER BY SEQ_IN_INDEX`, table, index)
	if err != nil {
		t.Fatalf("index-columns query %s.%s: %v", table, index, err)
	}
	defer rows.Close() //nolint:errcheck
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan index column: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index-columns rows err: %v", err)
	}
	return cols
}

func equalCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
