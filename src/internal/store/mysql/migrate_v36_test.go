package mysql

import (
	"context"
	"testing"
)

// TestMigrateV36_SeedUserTagKey verifies migration v36 seeds the `user` tag key
// as a context/single classifier on a fresh DB, so the wms-mcp auto-user-tag
// (wu-user-identity Part B) has the key with the correct cardinality without
// relying on a runtime wms_defineTag. The key must be absent at v35 and present
// at v36 with category='context', cardinality='single', is_seed=1.
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so it does not contend with other schema-creating suites on the migrate
// advisory lock.
func TestMigrateV36_SeedUserTagKey(t *testing.T) {
	// Migrate up to v35 so the tags table exists without the user-key seed.
	db := freshBackfillDB(t, 35)
	ctx := context.Background()

	var pre int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_key = 'user'`).Scan(&pre); err != nil {
		t.Fatalf("pre-v36 count user key: %v", err)
	}
	if pre != 0 {
		t.Fatalf("pre-v36 user key already present (%d rows); v36 would be a no-op for the wrong reason", pre)
	}

	// Apply v36.
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v36: %v", err)
	}

	// v36 recorded exactly once.
	var v36Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 36`).Scan(&v36Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v36Recorded != 1 {
		t.Fatalf("v36 recorded %d times, want 1", v36Recorded)
	}

	// The user key is now present as context/single, is_seed=1, empty stub value.
	var category, cardinality string
	var isSeed int
	if err := db.QueryRowContext(ctx,
		`SELECT category, cardinality, is_seed FROM tags WHERE tag_key = 'user' AND tag_value = ''`).
		Scan(&category, &cardinality, &isSeed); err != nil {
		t.Fatalf("read seeded user key: %v", err)
	}
	if category != "context" {
		t.Errorf("user key category = %q, want context", category)
	}
	if cardinality != "single" {
		t.Errorf("user key cardinality = %q, want single (NOT the multi default)", cardinality)
	}
	if isSeed != 1 {
		t.Errorf("user key is_seed = %d, want 1", isSeed)
	}
}

// TestMigrateV36_Idempotent re-runs migrate() on an already-current schema and
// asserts v36's INSERT IGNORE seed is a clean no-op: no error, schema_version
// unchanged, exactly one user-key stub row.
func TestMigrateV36_Idempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion)
	ctx := context.Background()

	before := schemaVersionRows(t, db)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op (INSERT IGNORE), got: %v", err)
	}
	after := schemaVersionRows(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before max=v%d count=%d, after max=v%d count=%d",
			before.maxV, before.count, after.maxV, after.count)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_key = 'user' AND tag_value = ''`).Scan(&n); err != nil {
		t.Fatalf("count user stub after idempotent re-run: %v", err)
	}
	if n != 1 {
		t.Fatalf("user-key stub rows after re-run = %d, want exactly 1", n)
	}
}
