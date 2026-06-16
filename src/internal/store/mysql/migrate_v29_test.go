package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestMigrateV29_CostViews verifies migration v29 (cost-views):
// the two views cost_facts and entity_tags_resolved are created, v29 is
// recorded in schema_version, and SUM(cost_usd) over cost_facts equals
// SUM(cost_usd) over token_ledger on fixture data (conservation invariant).
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run
// with -p 1 so it does not contend with other schema-creating suites on the
// migrate advisory lock.
func TestMigrateV29_CostViews(t *testing.T) {
	// Migrate up to v28 so the base tables exist but the views do not yet.
	db := freshBackfillDB(t, 28)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)

	// Seed two token_ledger rows: one that will be attributed, one that will not
	// (LEFT JOIN must preserve the unattributed row with entity '' and weight 1).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO token_ledger
			(message_id, session_id, agent_name, host, model, timestamp,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
		VALUES
			('msg-a', 'sess-1', 'agent', 'hub-1', 'sonnet', ?, 100, 50, 0, 0, 0.00030),
			('msg-b', 'sess-1', 'agent', 'hub-1', 'sonnet', ?, 200, 80, 0, 0, 0.00080)`,
		base, base.Add(time.Minute)); err != nil {
		t.Fatalf("seed token_ledger: %v", err)
	}

	// Attribute only msg-a to a workunit; msg-b is left unattributed.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_attribution
			(message_id, entity_type, entity_id, weight, method, computed_at)
		VALUES ('msg-a', 'workunit', 'wu-test', 1.0, 'manual', ?)`,
		base); err != nil {
		t.Fatalf("seed usage_attribution: %v", err)
	}

	// Views must not exist before v29.
	if viewExists(t, db, "cost_facts") {
		t.Fatal("cost_facts view should not exist before v29")
	}
	if viewExists(t, db, "entity_tags_resolved") {
		t.Fatal("entity_tags_resolved view should not exist before v29")
	}

	// Apply v29.
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v29: %v", err)
	}

	// v29 recorded.
	var v29Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 29`).Scan(&v29Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v29Recorded != 1 {
		t.Fatalf("v29 recorded %d times in schema_version, want 1", v29Recorded)
	}

	// Both views exist.
	if !viewExists(t, db, "cost_facts") {
		t.Error("cost_facts view not created by v29")
	}
	if !viewExists(t, db, "entity_tags_resolved") {
		t.Error("entity_tags_resolved view not created by v29")
	}

	// Conservation: SUM(cost_usd) over cost_facts must equal SUM over token_ledger.
	// The LEFT JOIN means unattributed rows appear with entity '' and weight 1,
	// so no cost is lost.
	var sumView, sumLedger float64
	if err := db.QueryRowContext(ctx,
		`SELECT SUM(cost_usd) FROM cost_facts`).Scan(&sumView); err != nil {
		t.Fatalf("SUM cost_facts: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT SUM(cost_usd) FROM token_ledger`).Scan(&sumLedger); err != nil {
		t.Fatalf("SUM token_ledger: %v", err)
	}
	// Allow for floating-point rounding at the 8th decimal place.
	if diff := sumView - sumLedger; diff < -1e-8 || diff > 1e-8 {
		t.Errorf("cost_facts SUM(cost_usd) = %.8f, token_ledger SUM = %.8f (diff %.2e; must be zero)",
			sumView, sumLedger, diff)
	}

	// The unattributed msg-b must appear in cost_facts with entity_type='' and weight=1.
	var unattribCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cost_facts WHERE message_id = 'msg-b' AND entity_type = '' AND weight = 1`).Scan(&unattribCount); err != nil {
		t.Fatalf("query unattributed row: %v", err)
	}
	if unattribCount != 1 {
		t.Errorf("unattributed msg-b: got %d rows with entity_type='' weight=1, want 1", unattribCount)
	}
}

// TestMigrateV29_Idempotent re-runs migrate() on an already-v29 schema and
// asserts it is a clean no-op: CREATE OR REPLACE VIEW is itself idempotent so
// re-running must not error, and schema_version must be unchanged.
func TestMigrateV29_Idempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion) // fully migrated, incl. v29
	ctx := context.Background()

	before := schemaVersionRows(t, db)

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op (CREATE OR REPLACE VIEW is idempotent), got: %v", err)
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

	// Views survived the no-op re-run.
	if !viewExists(t, db, "cost_facts") {
		t.Error("cost_facts view missing after idempotent re-run")
	}
	if !viewExists(t, db, "entity_tags_resolved") {
		t.Error("entity_tags_resolved view missing after idempotent re-run")
	}
}

// viewExists reports whether the named view exists in the current schema.
// It uses information_schema.VIEWS so base tables with the same name are not
// counted (a view and a table cannot share a name in MySQL/MariaDB anyway).
func viewExists(t *testing.T, db *sql.DB, view string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.VIEWS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, view).Scan(&n); err != nil {
		t.Fatalf("view-exists check %s: %v", view, err)
	}
	return n > 0
}
