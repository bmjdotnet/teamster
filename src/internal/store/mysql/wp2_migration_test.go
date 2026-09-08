package mysql

import (
	"context"
	"database/sql"
	"testing"
)

// These tests exercise v66 (wp2-phase-worktype-weed) against seeded pre-
// migration data — freshBackfillDB(t, 65) stops one version short, the test
// seeds fixtures that mimic live pre-migration state (including a work-type
// double-tag collision, the case the migration's delete-conflicting-row guard
// exists for), then migrateUpTo(66) applies v66 and the test asserts on the
// result. The rest of this package's suite only proves the migration applies
// without erroring on an empty schema; these tests prove the backfill/
// reclassify/collision logic is correct against actual rows.

func wp2SeedOutcomeAndWorkUnit(t *testing.T, db *sql.DB, outcomeID, wuID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status, focus,
			origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, 'o', '', 'active', '', '', '', '', '', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))`,
		outcomeID); err != nil {
		t.Fatalf("seed outcome %s: %v", outcomeID, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workunits (id, outcome_id, title, description, status, prior_status,
			agent_id, focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, ?, 'w', '', 'active', '', '', '', '', '', '', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))`,
		wuID, outcomeID); err != nil {
		t.Fatalf("seed workunit %s: %v", wuID, err)
	}
}

// wp2TagID looks up an existing tag row's id, failing the test if absent.
func wp2TagID(t *testing.T, db *sql.DB, key, value string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT id FROM tags WHERE tag_key = ? AND tag_value = ?`, key, value,
	).Scan(&id); err != nil {
		t.Fatalf("lookup tag %s:%s: %v", key, value, err)
	}
	return id
}

func wp2BindTag(t *testing.T, db *sql.DB, entityType, entityID string, tagID int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		VALUES (?, ?, ?, 'manual', UTC_TIMESTAMP(6))`,
		entityType, entityID, tagID); err != nil {
		t.Fatalf("bind tag %d to %s %s: %v", tagID, entityType, entityID, err)
	}
}

func wp2EntityWorkTypeValues(t *testing.T, db *sql.DB, entityID string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT t.tag_value FROM entity_tags et JOIN tags t ON t.id = et.tag_id
		WHERE et.entity_type = 'workunit' AND et.entity_id = ? AND t.tag_key = 'work-type'
		ORDER BY t.tag_value`, entityID)
	if err != nil {
		t.Fatalf("query work-type values for %s: %v", entityID, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func wp2TagRetired(t *testing.T, db *sql.DB, key, value string) bool {
	t.Helper()
	var retired int
	if err := db.QueryRowContext(context.Background(),
		`SELECT retired FROM tags WHERE tag_key = ? AND tag_value = ?`, key, value,
	).Scan(&retired); err != nil {
		t.Fatalf("lookup retired %s:%s: %v", key, value, err)
	}
	return retired != 0
}

// TestWP2Migration_PhaseBackfillAndRename covers §3/§4 of the phase package:
// the wms_intervals bulk rename, the entity_tags mirror remap, the
// warmup_recovery exclusion, the off-vocab retire, and the old seed retire.
func TestWP2Migration_PhaseBackfillAndRename(t *testing.T) {
	db := freshBackfillDB(t, 65)
	ctx := context.Background()

	wp2SeedOutcomeAndWorkUnit(t, db, "wp2-o1", "wp2-wu1")

	// A closed rework interval on the workunit — the thing the bulk UPDATE
	// must rename to 'iterate'.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, phase, phase_source)
		VALUES ('state', 'workunit', 'wp2-wu1', 'active', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'rework', 'classifier')`); err != nil {
		t.Fatalf("seed rework interval: %v", err)
	}
	// A warmup_recovery interval — hardcoded phase='admin' in production, but
	// seed it explicitly here so the exclusion guard has something concrete
	// to prove it does not touch.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, phase, phase_source)
		VALUES ('state', 'workunit', 'wp2-wu1', 'active', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'admin', 'warmup_recovery')`); err != nil {
		t.Fatalf("seed warmup interval: %v", err)
	}

	// Bind the entity_tags phase:rework tag directly (Rule 2's entity-wide
	// write, pre-reconciliation) — this is what the mirror-remap step must
	// re-point at phase:iterate.
	reworkTagID := wp2TagID(t, db, "phase", "rework")
	wp2BindTag(t, db, "workunit", "wp2-wu1", reworkTagID)

	// An off-vocabulary phase value, agent free-typed — must be retired
	// dynamically, not via a hardcoded list.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
		VALUES ('phase', 'exec', 0, 'lifecycle', 'single', '')`); err != nil {
		t.Fatalf("seed off-vocab phase:exec: %v", err)
	}

	if err := migrateUpTo(ctx, db, 66); err != nil {
		t.Fatalf("migrate to v66: %v", err)
	}

	// wms_intervals: rework -> iterate.
	var phase string
	if err := db.QueryRowContext(ctx, `
		SELECT phase FROM wms_intervals WHERE entity_id = 'wp2-wu1' AND phase_source = 'classifier'`,
	).Scan(&phase); err != nil {
		t.Fatalf("read backfilled interval phase: %v", err)
	}
	if phase != "iterate" {
		t.Errorf("classifier interval phase = %q, want iterate", phase)
	}

	// warmup_recovery interval must be untouched.
	var warmupPhase string
	if err := db.QueryRowContext(ctx, `
		SELECT phase FROM wms_intervals WHERE entity_id = 'wp2-wu1' AND phase_source = 'warmup_recovery'`,
	).Scan(&warmupPhase); err != nil {
		t.Fatalf("read warmup interval phase: %v", err)
	}
	if warmupPhase != "admin" {
		t.Errorf("warmup_recovery interval phase = %q, want admin (must not be touched)", warmupPhase)
	}

	// entity_tags: the workunit's phase tag now resolves to iterate, not rework.
	values := func(key string) []string {
		rows, err := db.QueryContext(ctx, `
			SELECT t.tag_value FROM entity_tags et JOIN tags t ON t.id = et.tag_id
			WHERE et.entity_type = 'workunit' AND et.entity_id = 'wp2-wu1' AND t.tag_key = ?`, key)
		if err != nil {
			t.Fatalf("query %s tags: %v", key, err)
		}
		defer rows.Close() //nolint:errcheck
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		return out
	}
	phaseTags := values("phase")
	if len(phaseTags) != 1 || phaseTags[0] != "iterate" {
		t.Errorf("workunit phase tags = %v, want exactly [iterate]", phaseTags)
	}

	// The old rework seed row and the off-vocab exec value are both retired.
	if !wp2TagRetired(t, db, "phase", "rework") {
		t.Error("phase:rework should be retired after backfill")
	}
	if !wp2TagRetired(t, db, "phase", "exec") {
		t.Error("phase:exec (off-vocabulary) should be retired")
	}
	// The new value is not retired and is required (v40 requires every phase row).
	if wp2TagRetired(t, db, "phase", "iterate") {
		t.Error("phase:iterate must not be retired")
	}

	// Acceptance check from §3: zero rows anywhere still say 'rework'.
	var wiCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wms_intervals WHERE phase = 'rework'`).Scan(&wiCount); err != nil {
		t.Fatalf("count rework intervals: %v", err)
	}
	if wiCount != 0 {
		t.Errorf("wms_intervals rows with phase='rework' = %d, want 0", wiCount)
	}
	var etCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity_tags et JOIN tags t ON t.id = et.tag_id
		WHERE t.tag_key = 'phase' AND t.tag_value = 'rework'`).Scan(&etCount); err != nil {
		t.Fatalf("count rework entity_tags: %v", err)
	}
	if etCount != 0 {
		t.Errorf("entity_tags rows bound to phase:rework = %d, want 0", etCount)
	}

	// Audit trail captured the remap before it happened.
	var auditCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM wp2_migration_audit
		WHERE tag_key = 'phase' AND old_value = 'rework' AND new_value = 'iterate' AND entity_id = 'wp2-wu1'`,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count phase audit rows: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("wp2_migration_audit phase rows for wp2-wu1 = %d, want 1", auditCount)
	}
}

// TestWP2Migration_WorkTypeReclassifyAndCollision covers TAXONOMY.md §5/§6:
// the three reclassify targets, and specifically the collision guard — an
// entity that already carries BOTH the killed value and its target value
// (the D3 rider's double-tagged scenario) must end up with exactly one
// binding, not a duplicate-key error and not two rows.
func TestWP2Migration_WorkTypeReclassifyAndCollision(t *testing.T) {
	db := freshBackfillDB(t, 65)
	ctx := context.Background()

	// None of the 7 killed values are migration-seeded (TAXONOMY.md §5: all
	// are either create-on-apply-only with live bindings — rework, review,
	// security — or zero-binding values that were never used at all —
	// bugfix, build, design, ops) — seed all 7 as bare rows here, matching
	// live pre-migration state, so the retire assertions below have a row to
	// check regardless of which kind each was.
	for _, v := range []string{"bugfix", "build", "design", "review", "rework", "ops", "security"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			VALUES ('work-type', ?, 0, 'lifecycle', 'single', '')`, v); err != nil {
			t.Fatalf("seed work-type:%s: %v", v, err)
		}
	}

	// wu-clean: only work-type:rework bound -> should remap to bug in place.
	wp2SeedOutcomeAndWorkUnit(t, db, "wp2-o2", "wp2-wu-clean")
	wp2BindTag(t, db, "workunit", "wp2-wu-clean", wp2TagID(t, db, "work-type", "rework"))

	// wu-collide: BOTH work-type:rework AND work-type:bug already bound —
	// the double-tagged case the delete-conflicting-row guard exists for.
	wp2SeedOutcomeAndWorkUnit(t, db, "wp2-o3", "wp2-wu-collide")
	wp2BindTag(t, db, "workunit", "wp2-wu-collide", wp2TagID(t, db, "work-type", "rework"))
	wp2BindTag(t, db, "workunit", "wp2-wu-collide", wp2TagID(t, db, "work-type", "bug"))

	// wu-review: work-type:review -> should remap to admin (admin's row is
	// created by the migration itself, so no pre-seed needed here).
	wp2SeedOutcomeAndWorkUnit(t, db, "wp2-o4", "wp2-wu-review")
	wp2BindTag(t, db, "workunit", "wp2-wu-review", wp2TagID(t, db, "work-type", "review"))

	// wu-security: work-type:security -> should remap to investigation.
	wp2SeedOutcomeAndWorkUnit(t, db, "wp2-o5", "wp2-wu-security")
	wp2BindTag(t, db, "workunit", "wp2-wu-security", wp2TagID(t, db, "work-type", "security"))

	if err := migrateUpTo(ctx, db, 66); err != nil {
		t.Fatalf("migrate to v66: %v", err)
	}

	if got := wp2EntityWorkTypeValues(t, db, "wp2-wu-clean"); len(got) != 1 || got[0] != "bug" {
		t.Errorf("wp2-wu-clean work-type values = %v, want exactly [bug]", got)
	}
	// The collision case: exactly one row, 'bug' — the redundant rework
	// binding must be deleted, not left as a duplicate or causing a
	// duplicate-key error that would have failed the migration outright.
	if got := wp2EntityWorkTypeValues(t, db, "wp2-wu-collide"); len(got) != 1 || got[0] != "bug" {
		t.Errorf("wp2-wu-collide work-type values = %v, want exactly [bug] (collision must collapse to one row)", got)
	}
	if got := wp2EntityWorkTypeValues(t, db, "wp2-wu-review"); len(got) != 1 || got[0] != "admin" {
		t.Errorf("wp2-wu-review work-type values = %v, want exactly [admin]", got)
	}
	if got := wp2EntityWorkTypeValues(t, db, "wp2-wu-security"); len(got) != 1 || got[0] != "investigation" {
		t.Errorf("wp2-wu-security work-type values = %v, want exactly [investigation]", got)
	}

	// The seven killed values are all retired.
	for _, v := range []string{"bugfix", "build", "design", "review", "rework", "ops", "security"} {
		if !wp2TagRetired(t, db, "work-type", v) {
			t.Errorf("work-type:%s should be retired", v)
		}
	}

	// The four promoted values are seeded and required, per v30's contract.
	for _, v := range []string{"polish", "investigation", "admin", "processor"} {
		var isSeed, required int
		if err := db.QueryRowContext(ctx,
			`SELECT is_seed, required FROM tags WHERE tag_key = 'work-type' AND tag_value = ?`, v,
		).Scan(&isSeed, &required); err != nil {
			t.Fatalf("lookup work-type:%s: %v", v, err)
		}
		if isSeed == 0 {
			t.Errorf("work-type:%s is_seed = 0, want 1", v)
		}
		if required == 0 {
			t.Errorf("work-type:%s required = 0, want 1", v)
		}
	}

	// Audit trail: exactly one row per reclassified entity.
	for _, tc := range []struct{ entity, old, new string }{
		{"wp2-wu-clean", "rework", "bug"},
		{"wp2-wu-collide", "rework", "bug"},
		{"wp2-wu-review", "review", "admin"},
		{"wp2-wu-security", "security", "investigation"},
	} {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM wp2_migration_audit
			WHERE tag_key = 'work-type' AND entity_id = ? AND old_value = ? AND new_value = ?`,
			tc.entity, tc.old, tc.new,
		).Scan(&n); err != nil {
			t.Fatalf("count audit rows for %s: %v", tc.entity, err)
		}
		if n != 1 {
			t.Errorf("wp2_migration_audit rows for %s (%s->%s) = %d, want 1", tc.entity, tc.old, tc.new, n)
		}
	}
}

// TestWP2Migration_PolishSlugAndReworkSlugRetired covers §6.2's slug-key
// changes: polish:<slug> added (mirroring the v49 facet pattern exactly),
// rework:<slug> retired.
func TestWP2Migration_PolishSlugAndReworkSlugRetired(t *testing.T) {
	db := freshBackfillDB(t, 65)
	ctx := context.Background()

	// Seed the rework slug key's own key-metadata row (tag_value='', the same
	// empty-value stub the v49 facet pattern uses) plus a live rework:<slug>
	// value the way an agent would have via wms_tagEntity, to prove retiring
	// the key retires both the key row and its values.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description, scope, exclusion_group, interview, facet_source)
		VALUES ('rework', '', 1, 'context', 'single', 'Rework slug', 'outcome', 'work-scope', 'propose', 'work-type')`); err != nil {
		t.Fatalf("seed rework slug key stub: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
		VALUES ('rework', 'agent-naming', 0, 'context', 'single', '')`); err != nil {
		t.Fatalf("seed rework:agent-naming: %v", err)
	}

	if err := migrateUpTo(ctx, db, 66); err != nil {
		t.Fatalf("migrate to v66: %v", err)
	}

	var facetSource, scope, exclusionGroup string
	if err := db.QueryRowContext(ctx,
		`SELECT facet_source, scope, exclusion_group FROM tags WHERE tag_key = 'polish' AND tag_value = ''`,
	).Scan(&facetSource, &scope, &exclusionGroup); err != nil {
		t.Fatalf("lookup polish slug key: %v", err)
	}
	if facetSource != "work-type" || scope != "outcome" || exclusionGroup != "work-scope" {
		t.Errorf("polish slug key metadata = (facet_source=%q, scope=%q, exclusion_group=%q), want (work-type, outcome, work-scope)",
			facetSource, scope, exclusionGroup)
	}

	if !wp2TagRetired(t, db, "rework", "") {
		t.Error("rework slug key-metadata row should be retired")
	}
	if !wp2TagRetired(t, db, "rework", "agent-naming") {
		t.Error("rework:agent-naming should be retired (non-destructively)")
	}
}

// TestWP2Migration_HealsPreExistingIsSeedGap reproduces the exact historical
// race @go-code/@store found live: v15's `INSERT IGNORE INTO tags (...,
// is_seed, ...) VALUES ('work-type','test',1,...)` is a no-op when the row
// already exists — so an entity tagged work-type:test via create-on-apply
// BEFORE v15 ever ran leaves that row permanently is_seed=0, since no later
// migration before v66 re-asserted is_seed for it. Seeding at v14 (one
// version before v15 ships the seed) and inserting the row by hand
// reproduces that exact ordering; migrating on to v66 must heal it.
func TestWP2Migration_HealsPreExistingIsSeedGap(t *testing.T) {
	db := freshBackfillDB(t, 14)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO tags (tag_key, tag_value, is_seed, description)
		VALUES ('work-type', 'test', 0, '')`); err != nil {
		t.Fatalf("seed pre-v15 create-on-apply work-type:test: %v", err)
	}

	if err := migrateUpTo(ctx, db, 66); err != nil {
		t.Fatalf("migrate to v66: %v", err)
	}

	// Every one of the 11 surviving work-type values must end up is_seed=1,
	// not just the ones this migration freshly created or promoted.
	for _, v := range []string{"feature", "bug", "refactor", "polish", "docs", "test",
		"investigation", "research", "infra", "admin", "processor"} {
		var isSeed int
		if err := db.QueryRowContext(ctx,
			`SELECT is_seed FROM tags WHERE tag_key = 'work-type' AND tag_value = ?`, v,
		).Scan(&isSeed); err != nil {
			t.Fatalf("lookup work-type:%s: %v", v, err)
		}
		if isSeed == 0 {
			t.Errorf("work-type:%s is_seed = 0 after full migration, want 1", v)
		}
	}
}
