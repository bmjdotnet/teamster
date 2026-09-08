package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// TestMigrateV70_OnHoldAbandonedSeed is the sqlite mirror of mysql's
// TestMigrateV71_OnHoldAbandonedSeed (WP9-state-model): verifies migration
// v70 seeds exactly the 28 new transition_rules rows for on_hold, abandoned,
// and the done->review reopen edge, on top of a v69 schema that has none of
// them. Uses filteredMigrator (migrations_test.go) to seed to v69, the same
// role mysql's freshBackfillDB(t, maxVersion) plays.
//
// Counts the true row delta (COUNT(*) before/after) rather than WP9's own
// AC3 literal SQL (`new_status IN ('on_hold','abandoned') OR (old_status=
// 'done' AND new_status='review')`) — that predicate undercounts to 20
// against WP9's own 28-row INSERT block: it never matches the 8 "resume
// from on_hold" rows (old_status='on_hold', new_status one of
// pending/active/review/blocked). Kit defect in the acceptance-criteria
// query, not in the migration — see mysql's migrate_v71_test.go for the
// full note; flagged to the lead rather than "fixed" by matching the wrong
// number.
func TestMigrateV70_OnHoldAbandonedSeed(t *testing.T) {
	db := freshMemDB(t)
	ctx := context.Background()

	seed := &filteredMigrator{sqliteMigrator: newSQLiteMigrator(db), cutVersion: 69}
	if err := store.RunMigrations(ctx, seed); err != nil {
		t.Fatalf("seed to v69: %v", err)
	}

	var pre int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transition_rules`).Scan(&pre); err != nil {
		t.Fatalf("pre-v70 count: %v", err)
	}

	if err := store.RunMigrations(ctx, newSQLiteMigrator(db)); err != nil {
		t.Fatalf("RunMigrations forward from v69: %v", err)
	}

	st := readSchemaVersionState(t, db)
	if st.maxV != highestKnownVersion() {
		t.Errorf("schema_version max = v%d, want v%d", st.maxV, highestKnownVersion())
	}

	var post int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transition_rules`).Scan(&post); err != nil {
		t.Fatalf("post-v70 count: %v", err)
	}
	if post-pre != 28 {
		t.Fatalf("v70 inserted %d rows, want 28", post-pre)
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

// legacyAbandonedDescription mirrors mysql's constant of the same name
// (migrate_v71_test.go) — kept as a literal duplicate rather than a shared
// import since the two packages don't share test code, but the string
// itself must be byte-identical across backends.
const legacyAbandonedDescription = "Legacy (pre-v0.3.0): abandonment was recorded as status=done plus this tag. New abandonments use status=abandoned; do not apply this value going forward."

// TestMigrateV70_ResolutionAbandonedGoesLegacy is the sqlite mirror of
// mysql's TestMigrateV71_ResolutionAbandonedGoesLegacy: v70's UPDATE retires
// resolution:abandoned's original description to the legacy wording above,
// on both an upgrade from v69 and a fresh install, and resolution:achieved
// (which never referenced "abandoned") is left untouched.
func TestMigrateV70_ResolutionAbandonedGoesLegacy(t *testing.T) {
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

	t.Run("upgrade_from_v69", func(t *testing.T) {
		db := freshMemDB(t)
		ctx := context.Background()

		seed := &filteredMigrator{sqliteMigrator: newSQLiteMigrator(db), cutVersion: 69}
		if err := store.RunMigrations(ctx, seed); err != nil {
			t.Fatalf("seed to v69: %v", err)
		}

		_, preAbandoned := readDescriptions(t, db)
		if preAbandoned == legacyAbandonedDescription {
			t.Fatalf("pre-v70 resolution:abandoned already carries the legacy text; v70 would be a no-op for the wrong reason")
		}

		if err := store.RunMigrations(ctx, newSQLiteMigrator(db)); err != nil {
			t.Fatalf("RunMigrations forward from v69: %v", err)
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
		db := freshMemDB(t)
		if err := store.RunMigrations(context.Background(), newSQLiteMigrator(db)); err != nil {
			t.Fatalf("fresh install: %v", err)
		}
		achieved, abandoned := readDescriptions(t, db)
		if abandoned != legacyAbandonedDescription {
			t.Errorf("fresh-install resolution:abandoned description = %q, want %q", abandoned, legacyAbandonedDescription)
		}
		if strings.Contains(strings.ToLower(achieved), "abandoned") {
			t.Errorf("resolution:achieved description references abandoned, want untouched: %q", achieved)
		}
	})
}

// TestMigrateV70_Idempotent re-runs RunMigrations on an already-current
// schema and asserts v70's seed is a clean no-op.
func TestMigrateV70_Idempotent(t *testing.T) {
	db := freshMemDB(t)
	ctx := context.Background()
	if err := store.RunMigrations(ctx, newSQLiteMigrator(db)); err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	before := readSchemaVersionState(t, db)

	if err := store.RunMigrations(ctx, newSQLiteMigrator(db)); err != nil {
		t.Fatalf("second RunMigrations must be a no-op, got: %v", err)
	}
	after := readSchemaVersionState(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before %+v, after %+v", before, after)
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
