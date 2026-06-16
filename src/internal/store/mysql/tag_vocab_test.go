package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests cover Stream A's store spine: the v18 tag-vocab-prune migration,
// the per-key cardinality write-guard in TagEntity, and the non-destructive
// ReconcileVocabulary / DefineTag / RetireTag methods.
//
// They reuse the shared harness (freshBackfillDB + migrateUpTo, per-schema
// isolation) and SKIP when TEAMSTER_TEST_MYSQL_DSN is unset. Use the mysql://
// URL DSN form — the tcp(...) driver form makes these tests silently skip
// (vacuous green). See the Stream-A build plan §5 for the original design rationale.

// currentSchemaVersion is the highest migration version; tests migrate fully.
// Derived from the migrations table so it tracks new migrations automatically
// instead of going stale (it last pinned v25 while v26-v28 already shipped).
var currentSchemaVersion = highestKnownVersion()

// newTestStore returns a *Store backed by a fresh fully-migrated schema, plus an
// outcome id that is a valid tag target (validTagEntityType allows only
// outcome/workunit).
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	db := freshBackfillDB(t, currentSchemaVersion)
	s := &Store{db: db}
	ctx := context.Background()
	now := time.Now().UTC()
	const oid = "out-test"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status,
			focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, 'Test outcome', '', 'pending', '', '', '', '', '', ?, ?)`,
		oid, now, now); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	return s, oid
}

// boundValues returns the tag values bound to an entity for one key.
func boundValues(t *testing.T, s *Store, entityType, entityID, key string) []string {
	t.Helper()
	tags, err := s.GetEntityTags(context.Background(), entityType, entityID)
	if err != nil {
		t.Fatalf("GetEntityTags: %v", err)
	}
	var out []string
	for _, et := range tags {
		if et.TagKey == key {
			out = append(out, et.TagValue)
		}
	}
	return out
}

// seedRow reports is_seed and existence for one (key,value).
func seedRow(t *testing.T, s *Store, key, value string) (exists bool, isSeed bool) {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT is_seed FROM tags WHERE tag_key = ? AND tag_value = ?`, key, value,
	).Scan(&n); err != nil {
		return false, false
	}
	return true, n != 0
}

// Test #6: the v18 migration demotes scope/team/release (is_seed=0, rows kept),
// adds the cardinality column, and sets product/priority single, others multi.
// (v18 sets project single; v27 later renames project→product, so a fully
// migrated schema carries the single cardinality under product.)
func TestV18_TagVocabPrune(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for _, key := range []string{"scope", "team", "release"} {
		var seedCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE tag_key = ? AND is_seed = 1`, key,
		).Scan(&seedCount); err != nil {
			t.Fatalf("count %s seeds: %v", key, err)
		}
		if seedCount != 0 {
			t.Errorf("v18: %s should have no is_seed=1 rows, got %d", key, seedCount)
		}
		// Rows must still EXIST (demote-not-delete).
		var total int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE tag_key = ?`, key,
		).Scan(&total); err != nil {
			t.Fatalf("count %s rows: %v", key, err)
		}
		if total == 0 {
			t.Errorf("v18: %s rows were DELETED — must be demoted, not deleted", key)
		}
	}

	cardOf := func(key string) string {
		var c string
		if err := s.db.QueryRowContext(ctx,
			`SELECT cardinality FROM tags WHERE tag_key = ? LIMIT 1`, key,
		).Scan(&c); err != nil {
			t.Fatalf("cardinality %s: %v", key, err)
		}
		return c
	}
	if got := cardOf("product"); got != "single" {
		t.Errorf("product cardinality = %q, want single", got)
	}
	if got := cardOf("priority"); got != "single" {
		t.Errorf("priority cardinality = %q, want single", got)
	}
	if got := cardOf("work-type"); got != "multi" {
		t.Errorf("work-type cardinality = %q, want multi (default)", got)
	}
	// v18 leaves phase at the multi default; v27 later forces phase/resolution/
	// lifecycle single, so a fully migrated schema carries phase as single.
	if got := cardOf("phase"); got != "single" {
		t.Errorf("phase cardinality = %q, want single (v27 lifecycle cardinality)", got)
	}
}

// Test #3: single-value guard replaces the prior value of the key on the entity.
func TestTagEntity_SingleValueReplace(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "priority", "p1", "manual", ""); err != nil {
		t.Fatalf("tag p1: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "priority", "p2", "manual", ""); err != nil {
		t.Fatalf("tag p2: %v", err)
	}
	got := boundValues(t, s, wms.EntityOutcome, oid, "priority")
	if len(got) != 1 || got[0] != "p2" {
		t.Errorf("priority bindings = %v, want [p2] (single-value replace)", got)
	}
}

// Test #3b (regression for the create-on-apply blocker): a single-value key whose
// VALUES are created on first use (product — seeded only as a ” stub) must still
// enforce single-value. The guard resolves cardinality at KEY grain, so a freshly
// minted value row inherits 'single' instead of the column DEFAULT 'multi'.
// (Uses product, the single-cardinality area-of-work key after the v27
// project→product rename — it ships with only an empty-value stub.)
func TestTagEntity_SingleValueReplace_CreateOnApply(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	// product has no pre-seeded values — alpha/beta are minted by these calls.
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "product", "alpha", "manual", ""); err != nil {
		t.Fatalf("tag alpha: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "product", "beta", "manual", ""); err != nil {
		t.Fatalf("tag beta: %v", err)
	}
	got := boundValues(t, s, wms.EntityOutcome, oid, "product")
	if len(got) != 1 || got[0] != "beta" {
		t.Errorf("product bindings = %v, want [beta] (create-on-apply single-value replace)", got)
	}

	// Column consistency: no value of a single key may carry a non-'single'
	// cardinality, or listTags/dashboards show a key with mixed cardinality.
	var mixed int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_key = 'product' AND cardinality <> 'single'`,
	).Scan(&mixed); err != nil {
		t.Fatalf("mixed-cardinality count: %v", err)
	}
	if mixed != 0 {
		t.Errorf("product has %d value rows with non-single cardinality — must be uniform", mixed)
	}
}

// Test #4: multi-value keys accumulate — the guard does not fire.
func TestTagEntity_MultiValueCoexist(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "work-type", "feature", "classifier", ""); err != nil {
		t.Fatalf("tag feature: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "work-type", "experiment", "classifier", ""); err != nil {
		t.Fatalf("tag experiment: %v", err)
	}
	got := boundValues(t, s, wms.EntityOutcome, oid, "work-type")
	if len(got) != 2 {
		t.Errorf("work-type bindings = %v, want 2 values (multi-value coexist)", got)
	}
}

// Test #5: the single-value replace is source-agnostic (latest-write-wins).
func TestTagEntity_SingleValueReplaceCrossSource(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "priority", "p1", "manual", ""); err != nil {
		t.Fatalf("tag p1 manual: %v", err)
	}
	// A later write from a different source replaces the manual value.
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "priority", "p2", "classifier", ""); err != nil {
		t.Fatalf("tag p2 classifier: %v", err)
	}
	got := boundValues(t, s, wms.EntityOutcome, oid, "priority")
	if len(got) != 1 || got[0] != "p2" {
		t.Errorf("priority bindings = %v, want [p2] (cross-source replace)", got)
	}
}

// Test #1: ReconcileVocabulary is non-destructive — demoting a key keeps its
// row and its bindings; re-declaring re-promotes it.
func TestReconcileVocabulary_NonDestructive(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	// Declare scope and bind a value to the entity.
	if err := s.ReconcileVocabulary(ctx, []wms.TagSpec{
		{Key: "scope", Category: "context", Cardinality: "multi", Values: []string{"strategic"}},
	}); err != nil {
		t.Fatalf("reconcile declare scope: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "scope", "strategic"); !exists || !isSeed {
		t.Fatalf("after declare: scope:strategic exists=%v isSeed=%v, want true/true", exists, isSeed)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "scope", "strategic", "manual", ""); err != nil {
		t.Fatalf("bind scope: %v", err)
	}

	// Reconcile WITHOUT scope → demote (is_seed=0) but keep the row + binding.
	if err := s.ReconcileVocabulary(ctx, []wms.TagSpec{
		{Key: "project", Category: "context", Cardinality: "single"},
	}); err != nil {
		t.Fatalf("reconcile drop scope: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "scope", "strategic"); !exists || isSeed {
		t.Errorf("after demote: scope:strategic exists=%v isSeed=%v, want true/false", exists, isSeed)
	}
	if got := boundValues(t, s, wms.EntityOutcome, oid, "scope"); len(got) != 1 || got[0] != "strategic" {
		t.Errorf("binding after demote = %v, want [strategic] (bindings must survive)", got)
	}

	// Re-declare scope → re-promote.
	if err := s.ReconcileVocabulary(ctx, []wms.TagSpec{
		{Key: "scope", Category: "context", Cardinality: "multi", Values: []string{"strategic"}},
	}); err != nil {
		t.Fatalf("reconcile re-declare scope: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "scope", "strategic"); !exists || !isSeed {
		t.Errorf("after re-declare: scope:strategic exists=%v isSeed=%v, want true/true (reversible)", exists, isSeed)
	}
}

// Test #2: ReconcileVocabulary never touches the writer-coupled lifecycle keys,
// even when a config omits them — guards the v15 wrong-category bug class.
func TestReconcileVocabulary_ExcludesLifecycleKeys(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Reconcile a config that mentions NONE of the lifecycle keys.
	if err := s.ReconcileVocabulary(ctx, []wms.TagSpec{
		{Key: "project", Category: "context", Cardinality: "single"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, key := range []string{"phase", "work-type", "resolution", "lifecycle"} {
		var seedCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE tag_key = ? AND is_seed = 1`, key,
		).Scan(&seedCount); err != nil {
			t.Fatalf("count %s seeds: %v", key, err)
		}
		if seedCount == 0 {
			t.Errorf("lifecycle key %s was demoted by reconcile — must be untouchable", key)
		}
		// Category must remain lifecycle (not flipped to the context default).
		var category string
		if err := s.db.QueryRowContext(ctx,
			`SELECT category FROM tags WHERE tag_key = ? LIMIT 1`, key,
		).Scan(&category); err != nil {
			t.Fatalf("category %s: %v", key, err)
		}
		if category != "lifecycle" {
			t.Errorf("lifecycle key %s category = %q, want lifecycle (v15-class drift)", key, category)
		}
	}
}

// RetireTag is the per-key demote used by the wms_retireTag admin tool.
func TestRetireTag_NonDestructive(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "scope", Category: "context", Values: []string{"strategic"}}); err != nil {
		t.Fatalf("define scope: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, oid, "scope", "strategic", "manual", ""); err != nil {
		t.Fatalf("bind scope: %v", err)
	}
	if err := s.RetireTag(ctx, "scope"); err != nil {
		t.Fatalf("retire scope: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "scope", "strategic"); !exists || isSeed {
		t.Errorf("after retire: scope:strategic exists=%v isSeed=%v, want true/false", exists, isSeed)
	}
	if got := boundValues(t, s, wms.EntityOutcome, oid, "scope"); len(got) != 1 {
		t.Errorf("binding after retire = %v, want preserved", got)
	}
}

// N1: RetireTag refuses writer-coupled lifecycle keys (would re-create the v15
// wrong-category bug) but demotes user-vocabulary keys fine.
func TestRetireTag_RefusesLifecycleKeys(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.RetireTag(ctx, "work-type"); err == nil {
		t.Errorf("RetireTag(work-type) should error — lifecycle key is system-managed")
	}
	// work-type untouched: still seeded and still lifecycle.
	var seedCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_key = 'work-type' AND is_seed = 1 AND category = 'lifecycle'`,
	).Scan(&seedCount); err != nil {
		t.Fatalf("count work-type seeds: %v", err)
	}
	if seedCount == 0 {
		t.Errorf("work-type was demoted/re-categorized by a refused RetireTag")
	}

	// A user-vocabulary key retires cleanly.
	if err := s.RetireTag(ctx, "scope"); err != nil {
		t.Errorf("RetireTag(scope) should succeed for a user-vocab key: %v", err)
	}
}

// Hardening: DefineTag refuses a system-managed key and leaves it untouched —
// the config/admin layer cannot clobber a lifecycle key's category/cardinality.
func TestDefineTag_RefusesSystemKeys(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{
		Key: "work-type", Category: "context", Cardinality: "single", Values: []string{"feature"},
	}); err == nil {
		t.Errorf("DefineTag(work-type) should error — system-managed key")
	}
	// work-type:feature stays lifecycle + multi (not flipped to the spec's context/single).
	var category, cardinality string
	if err := s.db.QueryRowContext(ctx,
		`SELECT category, cardinality FROM tags WHERE tag_key = 'work-type' AND tag_value = 'feature'`,
	).Scan(&category, &cardinality); err != nil {
		t.Fatalf("read work-type:feature: %v", err)
	}
	if category != "lifecycle" || cardinality != "multi" {
		t.Errorf("work-type:feature = (%s,%s), want (lifecycle,multi) — refused DefineTag must not write", category, cardinality)
	}
}

// Hardening: a config declaring a system-managed key with a DRIFTED category is
// SKIPPED (not an error) while the valid keys reconcile — guards v15 drift via
// the config path.
func TestReconcileVocabulary_SkipsSystemKeyDrift(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.ReconcileVocabulary(ctx, []wms.TagSpec{
		{Key: "work-type", Category: "context", Cardinality: "single"}, // drift attempt — must be skipped
		{Key: "project", Category: "context", Cardinality: "single"},   // valid — must reconcile
	}); err != nil {
		t.Fatalf("reconcile (with skipped system key) should not error: %v", err)
	}
	// work-type unchanged.
	var category, cardinality string
	if err := s.db.QueryRowContext(ctx,
		`SELECT category, cardinality FROM tags WHERE tag_key = 'work-type' LIMIT 1`,
	).Scan(&category, &cardinality); err != nil {
		t.Fatalf("read work-type: %v", err)
	}
	if category != "lifecycle" || cardinality != "multi" {
		t.Errorf("work-type = (%s,%s), want (lifecycle,multi) — drift must be skipped", category, cardinality)
	}
	// project reconciled (seeded single).
	var pcard string
	if err := s.db.QueryRowContext(ctx,
		`SELECT cardinality FROM tags WHERE tag_key = 'project' LIMIT 1`,
	).Scan(&pcard); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if pcard != "single" {
		t.Errorf("project cardinality = %q, want single (valid keys still reconcile)", pcard)
	}
}

// Hardening: a NEW user-defined key (not in the shipped vocabulary) is retirable
// — the deny-list only protects system keys, so the allow-list's wrong block on
// new keys is gone.
func TestRetireTag_NewUserKeyRetirable(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "topic", Category: "context", Values: []string{"auth"}}); err != nil {
		t.Fatalf("define topic: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "topic", "auth"); !exists || !isSeed {
		t.Fatalf("topic:auth exists=%v isSeed=%v, want true/true", exists, isSeed)
	}
	if err := s.RetireTag(ctx, "topic"); err != nil {
		t.Errorf("RetireTag(topic) should succeed for a new user key: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "topic", "auth"); !exists || isSeed {
		t.Errorf("after retire: topic:auth exists=%v isSeed=%v, want true/false", exists, isSeed)
	}
}

// Hardening: the reconcile demote sweep demotes a dropped USER key (read from
// the DB, not a fixed list) but never a lifecycle key.
func TestReconcileVocabulary_DemoteSweepUserKeyNotLifecycle(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Seed a user key 'topic', then reconcile WITHOUT it → it demotes.
	if err := s.DefineTag(ctx, wms.TagSpec{Key: "topic", Category: "context", Values: []string{"auth"}}); err != nil {
		t.Fatalf("define topic: %v", err)
	}
	if err := s.ReconcileVocabulary(ctx, []wms.TagSpec{
		{Key: "project", Category: "context", Cardinality: "single"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if exists, isSeed := seedRow(t, s, "topic", "auth"); !exists || isSeed {
		t.Errorf("topic dropped from config: exists=%v isSeed=%v, want true/false (demote sweep)", exists, isSeed)
	}
	// Lifecycle keys untouched by the same sweep.
	for _, key := range []string{"phase", "work-type", "resolution", "lifecycle"} {
		var seedCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE tag_key = ? AND is_seed = 1`, key,
		).Scan(&seedCount); err != nil {
			t.Fatalf("count %s seeds: %v", key, err)
		}
		if seedCount == 0 {
			t.Errorf("demote sweep demoted lifecycle key %s — must be excluded", key)
		}
	}
}
