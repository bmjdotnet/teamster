package mysql

import (
	"context"
	"testing"
	"time"
)

// TestMigrateV25_ArchivesOldIntervalTables verifies migration v25 (B3 Wave 4):
// the two now-dead source tables are RENAMEd to archived_v2_* (never DROPped, the
// v17 precedent), wms_intervals is intact, and a usage_attribution.interval_id
// seeded BEFORE the migration still resolves to a wms_intervals row afterwards
// (the v23 remap repointed it, so the cost pointer survives the archive rename).
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so it does not contend with other schema-creating suites on the migrate
// advisory lock.
func TestMigrateV25_ArchivesOldIntervalTables(t *testing.T) {
	// Migrate to v23 first (wms_intervals exists, backfill+remap have run, but the
	// old tables are NOT yet archived) so we can seed a state interval + a
	// usage_attribution row pointing at it, prove the remap survives, THEN apply
	// v25 and re-check.
	db := freshBackfillDB(t, 23)
	ctx := context.Background()
	base := time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)

	// Seed a costed state interval directly in wms_intervals (the canonical store
	// post-W3) + a usage_attribution row whose interval_id points at it. This is
	// the cost pointer that must keep resolving after the v25 rename.
	res, err := db.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, started_at, ended_at, duration_ms)
		VALUES ('state', 'workunit', 'wu-v25', 'active', ?, ?, 60000)`,
		base, base.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("seed wms_intervals state row: %v", err)
	}
	intervalID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_attribution (message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
		VALUES ('msg-v25', 'workunit', 'wu-v25', 1.0, 'temporal_join_v3', ?, ?)`,
		base, intervalID); err != nil {
		t.Fatalf("seed usage_attribution: %v", err)
	}

	// Before v25 the old tables still exist (created v9/v12).
	if !tableExists(t, db, "wms_event_records") {
		t.Fatal("wms_event_records should exist before v25")
	}
	if !tableExists(t, db, "agent_focus_intervals") {
		t.Fatal("agent_focus_intervals should exist before v25")
	}

	// Apply v25 (the archive rename).
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v25: %v", err)
	}

	// v25 recorded.
	var v25Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 25`).Scan(&v25Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v25Recorded != 1 {
		t.Fatalf("v25 recorded %d times in schema_version, want 1", v25Recorded)
	}

	// (i) archived_v2_* destinations EXIST.
	if !tableExists(t, db, "archived_v2_event_records") {
		t.Error("v25 did not create archived_v2_event_records (RENAME target)")
	}
	if !tableExists(t, db, "archived_v2_focus_intervals") {
		t.Error("v25 did not create archived_v2_focus_intervals (RENAME target)")
	}

	// (ii) the old names are GONE (renamed away, not still present — RENAME, not COPY).
	if tableExists(t, db, "wms_event_records") {
		t.Error("wms_event_records still exists after v25 — RENAME should move it away")
	}
	if tableExists(t, db, "agent_focus_intervals") {
		t.Error("agent_focus_intervals still exists after v25 — RENAME should move it away")
	}

	// (iii) wms_intervals is intact (the unify target, untouched by the archive).
	if !tableExists(t, db, "wms_intervals") {
		t.Fatal("wms_intervals missing after v25 — the archive must not touch it")
	}

	// (iv) the cost pointer survives: the usage_attribution row seeded before v25
	// still resolves 1:1 to its wms_intervals row (the v23 remap pointed it there;
	// the archive rename of the OLD table does not disturb it).
	var iid int64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id = 'msg-v25'`).Scan(&iid); err != nil {
		t.Fatalf("read interval_id after v25: %v", err)
	}
	if iid != intervalID {
		t.Errorf("interval_id = %d, want %d (archive must not disturb the cost pointer)", iid, intervalID)
	}
	var joined int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_attribution ua
		JOIN wms_intervals wi ON wi.id = ua.interval_id AND wi.kind = 'state'
		WHERE ua.message_id = 'msg-v25' AND wi.entity_id = 'wu-v25'`).Scan(&joined); err != nil {
		t.Fatalf("join usage_attribution → wms_intervals after v25: %v", err)
	}
	if joined != 1 {
		t.Fatalf("cost pointer must still join 1:1 onto the wu-v25 wms_intervals row after the archive; got %d", joined)
	}
}

// TestMigrateV25_Idempotent re-runs migrate() on an already-v25 schema and asserts
// it is a clean no-op: RENAME TABLE is NOT itself idempotent (a second run would
// 1146 on the now-renamed name), so the safety comes from the runMigrations
// schema_version gate skipping the already-applied version — exactly as for the
// v17 rename. A second migrate() must not re-fire the rename, not error, and leave
// schema_version unchanged.
func TestMigrateV25_Idempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion) // fully migrated, incl. v25
	ctx := context.Background()

	before := schemaVersionRows(t, db)

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op (v25 rename must not re-fire), got: %v", err)
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

	// The archive landed and survived the no-op re-run; the old names stay gone.
	if !tableExists(t, db, "archived_v2_event_records") || !tableExists(t, db, "archived_v2_focus_intervals") {
		t.Error("archived_v2_* tables missing after idempotent re-run")
	}
	if tableExists(t, db, "wms_event_records") || tableExists(t, db, "agent_focus_intervals") {
		t.Error("old interval table names reappeared after idempotent re-run")
	}
}
