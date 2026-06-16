package mysql

import (
	"context"
	"testing"
)

// TestMigrateV35_RecoveryEvidence verifies migration v35 (recovery-evidence):
// it creates the recovery_evidence audit side table, records v35 in
// schema_version, and the table accepts a provenance row keyed by message_id.
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so it does not contend with other schema-creating suites on the migrate
// advisory lock.
func TestMigrateV35_RecoveryEvidence(t *testing.T) {
	// Migrate up to v34 so the table does not yet exist.
	db := freshBackfillDB(t, 34)
	ctx := context.Background()

	if tableExists(t, db, "recovery_evidence") {
		t.Fatal("recovery_evidence should not exist before v35")
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v35: %v", err)
	}

	var v35 int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 35`).Scan(&v35); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v35 != 1 {
		t.Fatalf("v35 recorded %d times, want 1", v35)
	}
	if !tableExists(t, db, "recovery_evidence") {
		t.Fatal("recovery_evidence not created by v35")
	}

	// The table accepts a provenance row and round-trips it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO recovery_evidence
			(message_id, entity_type, entity_id, setfocus_at, recovered_at)
		VALUES ('m1|r1', 'outcome', 'o1', '2026-06-09 20:21:23.243000', '2026-06-09 20:30:00.000000')`); err != nil {
		t.Fatalf("insert recovery_evidence row: %v", err)
	}
	var et, ei string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id FROM recovery_evidence WHERE message_id = 'm1|r1'`).
		Scan(&et, &ei); err != nil {
		t.Fatalf("read back evidence: %v", err)
	}
	if et != "outcome" || ei != "o1" {
		t.Fatalf("evidence round-trip = (%q,%q), want (outcome,o1)", et, ei)
	}
}

// TestMigrateV35_Idempotent re-runs migrate() on an already-current schema and
// asserts v35's CREATE TABLE IF NOT EXISTS is a clean no-op: no error,
// schema_version unchanged, table still present.
func TestMigrateV35_Idempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion)
	ctx := context.Background()

	before := schemaVersionRows(t, db)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op (CREATE TABLE IF NOT EXISTS), got: %v", err)
	}
	after := schemaVersionRows(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before max=v%d count=%d, after max=v%d count=%d",
			before.maxV, before.count, after.maxV, after.count)
	}
	if !tableExists(t, db, "recovery_evidence") {
		t.Fatal("recovery_evidence missing after idempotent re-run")
	}
}
