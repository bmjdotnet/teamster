package mysql

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestMigrateV71_OnHoldAbandonedSeed verifies migration v71 (WP9-state-model)
// seeds exactly the 28 new transition_rules rows for on_hold, abandoned, and
// the done->review reopen edge, on top of a v70 schema that has none of them.
//
// Counts the true row delta (COUNT(*) before/after) rather than WP9's own
// AC3 literal SQL (`new_status IN ('on_hold','abandoned') OR (old_status=
// 'done' AND new_status='review')`) — that predicate undercounts to 20
// against WP9's own 28-row INSERT block, because it never matches the 8
// "resume from on_hold" rows (old_status='on_hold', new_status one of
// pending/active/review/blocked — the target is a normal status, not
// on_hold/abandoned, and old_status isn't 'done'). Verified independently:
// of the 28 rows WP9's Migration section specifies verbatim, exactly those
// 8 fall outside its own AC3 predicate. This is a kit defect in the
// acceptance-criteria query, not in the migration — flagged to the lead,
// not "fixed" by matching the wrong number.
func TestMigrateV71_OnHoldAbandonedSeed(t *testing.T) {
	db := freshBackfillDB(t, 70)
	ctx := context.Background()

	var pre int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transition_rules`).Scan(&pre); err != nil {
		t.Fatalf("pre-v71 count: %v", err)
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to v71: %v", err)
	}

	var v71Recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version = 71`).Scan(&v71Recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v71Recorded != 1 {
		t.Fatalf("v71 recorded %d times, want 1", v71Recorded)
	}

	var post int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transition_rules`).Scan(&post); err != nil {
		t.Fatalf("post-v71 count: %v", err)
	}
	if post-pre != 28 {
		t.Fatalf("v71 inserted %d rows, want 28", post-pre)
	}

	// Every one of the 8 "resume from on_hold" rows the AC3 predicate above
	// misses is genuinely present.
	for _, tc := range []struct{ entityType, new string }{
		{"outcome", "pending"}, {"outcome", "active"}, {"outcome", "review"}, {"outcome", "blocked"},
		{"workunit", "pending"}, {"workunit", "active"}, {"workunit", "review"}, {"workunit", "blocked"},
	} {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM transition_rules
			WHERE entity_type = ? AND old_status = 'on_hold' AND new_status = ?`,
			tc.entityType, tc.new,
		).Scan(&n); err != nil {
			t.Fatalf("lookup %s on_hold->%s: %v", tc.entityType, tc.new, err)
		}
		if n != 1 {
			t.Errorf("%s on_hold->%s rows = %d, want 1", tc.entityType, tc.new, n)
		}
	}

	// Spot-check the reopen edge and one of each new-status direction, both
	// entity types, all with required_role='*'.
	for _, tc := range []struct{ entityType, old, new string }{
		{"outcome", "done", "review"},
		{"workunit", "done", "review"},
		{"outcome", "pending", "on_hold"},
		{"workunit", "on_hold", "active"},
		{"outcome", "active", "abandoned"},
		{"workunit", "on_hold", "abandoned"},
	} {
		var role string
		if err := db.QueryRowContext(ctx, `
			SELECT required_role FROM transition_rules
			WHERE entity_type = ? AND old_status = ? AND new_status = ?`,
			tc.entityType, tc.old, tc.new,
		).Scan(&role); err != nil {
			t.Fatalf("lookup %s %s->%s: %v", tc.entityType, tc.old, tc.new, err)
		}
		if role != "*" {
			t.Errorf("%s %s->%s required_role = %q, want *", tc.entityType, tc.old, tc.new, role)
		}
	}
}

// legacyAbandonedDescription is the post-v71/v70 wording for the
// resolution:abandoned tag row (redteam finding, ruled): the value itself
// stays seeded — historical entities carry it and retiring would orphan
// them — but it no longer describes the live mechanism, since a new
// abandonment is recorded as status=abandoned, not status=done plus this
// tag. Shared with the sqlite mirror test so the two backends are checked
// against literally the same string, not two independently-typed copies.
const legacyAbandonedDescription = "Legacy (pre-v0.3.0): abandonment was recorded as status=done plus this tag. New abandonments use status=abandoned; do not apply this value going forward."

// TestMigrateV71_ResolutionAbandonedGoesLegacy verifies v71's UPDATE retires
// resolution:abandoned's Version-13 description to the legacy wording above,
// on both a DB that already has the Version-13 seed (the upgrade path this
// UPDATE exists for) and a fresh install (v13 then v71 in the same run,
// proving the two paths converge on identical text). resolution:achieved
// never referenced "abandoned" in its own description, so it must be
// untouched by this migration.
func TestMigrateV71_ResolutionAbandonedGoesLegacy(t *testing.T) {
	readDescriptions := func(t *testing.T, db *sql.DB) (achieved, abandoned string) {
		t.Helper()
		if err := db.QueryRowContext(context.Background(),
			`SELECT description FROM tags WHERE tag_key = 'resolution' AND tag_value = 'achieved'`,
		).Scan(&achieved); err != nil {
			t.Fatalf("read resolution:achieved description: %v", err)
		}
		if err := db.QueryRowContext(context.Background(),
			`SELECT description FROM tags WHERE tag_key = 'resolution' AND tag_value = 'abandoned'`,
		).Scan(&abandoned); err != nil {
			t.Fatalf("read resolution:abandoned description: %v", err)
		}
		return achieved, abandoned
	}

	t.Run("upgrade_from_v70", func(t *testing.T) {
		db := freshBackfillDB(t, 70)
		ctx := context.Background()

		_, preAbandoned := readDescriptions(t, db)
		if preAbandoned == legacyAbandonedDescription {
			t.Fatalf("pre-v71 resolution:abandoned already carries the legacy text; v71 would be a no-op for the wrong reason")
		}

		if err := migrate(ctx, db); err != nil {
			t.Fatalf("migrate to v71: %v", err)
		}

		achieved, abandoned := readDescriptions(t, db)
		if abandoned != legacyAbandonedDescription {
			t.Errorf("resolution:abandoned description = %q, want %q", abandoned, legacyAbandonedDescription)
		}
		if strings.Contains(strings.ToLower(achieved), "abandoned") {
			t.Errorf("resolution:achieved description references abandoned, want untouched: %q", achieved)
		}
	})

	t.Run("fresh_install", func(t *testing.T) {
		db := freshBackfillDB(t, currentSchemaVersion)
		achieved, abandoned := readDescriptions(t, db)
		if abandoned != legacyAbandonedDescription {
			t.Errorf("fresh-install resolution:abandoned description = %q, want %q", abandoned, legacyAbandonedDescription)
		}
		if strings.Contains(strings.ToLower(achieved), "abandoned") {
			t.Errorf("resolution:achieved description references abandoned, want untouched: %q", achieved)
		}
	})
}

// TestMigrateV71_Idempotent re-runs migrate() on an already-current schema
// and asserts v71's seed is a clean no-op.
func TestMigrateV71_Idempotent(t *testing.T) {
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
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM transition_rules
		WHERE old_status = 'on_hold' OR new_status IN ('on_hold','abandoned')
		   OR (old_status='done' AND new_status='review')`,
	).Scan(&n); err != nil {
		t.Fatalf("count after re-run: %v", err)
	}
	if n != 28 {
		t.Fatalf("on_hold/abandoned/reopen rows after re-run = %d, want exactly 28 (no duplication)", n)
	}

	var abandoned string
	if err := db.QueryRowContext(ctx,
		`SELECT description FROM tags WHERE tag_key = 'resolution' AND tag_value = 'abandoned'`,
	).Scan(&abandoned); err != nil {
		t.Fatalf("read resolution:abandoned description after re-run: %v", err)
	}
	if abandoned != legacyAbandonedDescription {
		t.Errorf("resolution:abandoned description after re-run = %q, want %q (UPDATE must not have reverted)", abandoned, legacyAbandonedDescription)
	}
}
