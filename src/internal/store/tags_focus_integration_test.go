package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

// openScratchStore spins up a throwaway teamster_test_* schema (migrated) and
// returns the store. Skips when TEAMSTER_TEST_MYSQL_DSN is unset. Never touches
// the live database.
func openScratchStore(t *testing.T) *mysql.Store {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}
	schema := fmt.Sprintf("teamster_test_p3_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
	if err := mysqlEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN, err := mysqlRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	st, err := mysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		_ = mysqlDropSchema(dsn, schema)
	})
	return st
}

// TestTagEntity verifies the tag-application path: seed tags exist, applying a
// seed tag links the entity, applying a new key:value creates a non-seed tag,
// and re-tagging is idempotent.
func TestTagEntity(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()
	db := st.DB()

	// Migration v12 seeds 20 starter tags.
	tags, err := st.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) < 20 {
		t.Fatalf("expected >=20 seed tags, got %d", len(tags))
	}
	seedSeen := false
	for _, tg := range tags {
		if tg.Key == "phase" && tg.Value == "build" {
			if !tg.IsSeed {
				t.Errorf("phase=build should be a seed tag")
			}
			seedSeen = true
		}
	}
	if !seedSeen {
		t.Fatalf("expected seed tag phase=build in ListTags")
	}

	// Apply a seed tag to an outcome (v3 entity type; no description needed — it already has one).
	if err := st.TagEntity(ctx, "outcome", "o1", "phase", "build", "manual", ""); err != nil {
		t.Fatalf("TagEntity seed: %v", err)
	}
	// Apply a brand-new key:value with a description — must create a non-seed
	// tag and STORE the description for discovery.
	if err := st.TagEntity(ctx, "outcome", "o1", "squad", "alpha", "manual", "the alpha squad owns this"); err != nil {
		t.Fatalf("TagEntity new: %v", err)
	}
	// Idempotent re-tag (same entity, same key/value, different source). A new
	// description here must NOT clobber the existing one.
	if err := st.TagEntity(ctx, "outcome", "o1", "phase", "build", "classifier", "SHOULD NOT OVERWRITE SEED DESC"); err != nil {
		t.Fatalf("TagEntity re-tag: %v", err)
	}

	// entity_tags should have exactly 2 rows for o1 (phase=build, squad=alpha),
	// the re-tag updated in place rather than duplicating.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE entity_type='outcome' AND entity_id='o1'`).Scan(&n); err != nil {
		t.Fatalf("count entity_tags: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 entity_tags rows for o1, got %d", n)
	}

	// The new squad=alpha tag is non-seed.
	var isSeed int
	if err := db.QueryRowContext(ctx,
		`SELECT is_seed FROM tags WHERE tag_key='squad' AND tag_value='alpha'`).Scan(&isSeed); err != nil {
		t.Fatalf("query squad tag: %v", err)
	}
	if isSeed != 0 {
		t.Errorf("operator-defined tag squad=alpha should have is_seed=0, got %d", isSeed)
	}

	// Source was refreshed by the re-tag.
	var src string
	if err := db.QueryRowContext(ctx,
		`SELECT et.source FROM entity_tags et JOIN tags t ON t.id=et.tag_id
		 WHERE et.entity_type='outcome' AND et.entity_id='o1' AND t.tag_key='phase' AND t.tag_value='build'`).Scan(&src); err != nil {
		t.Fatalf("query source: %v", err)
	}
	if src != "classifier" {
		t.Errorf("re-tag should refresh source to 'classifier', got %q", src)
	}

	// The new squad=alpha tag stored its description (dynamic + self-describing).
	var squadDesc string
	if err := db.QueryRowContext(ctx,
		`SELECT description FROM tags WHERE tag_key='squad' AND tag_value='alpha'`).Scan(&squadDesc); err != nil {
		t.Fatalf("query squad desc: %v", err)
	}
	if squadDesc != "the alpha squad owns this" {
		t.Errorf("new tag should store its description, got %q", squadDesc)
	}

	// The seed tag's description was NOT clobbered by the re-tag's description.
	var phaseBuildDesc string
	if err := db.QueryRowContext(ctx,
		`SELECT description FROM tags WHERE tag_key='phase' AND tag_value='build'`).Scan(&phaseBuildDesc); err != nil {
		t.Fatalf("query phase=build desc: %v", err)
	}
	if phaseBuildDesc == "SHOULD NOT OVERWRITE SEED DESC" || phaseBuildDesc == "" {
		t.Errorf("re-tag must not clobber existing description, got %q", phaseBuildDesc)
	}

	// Unknown entity type is rejected.
	if err := st.TagEntity(ctx, "squadron", "x", "k", "v", "manual", ""); err == nil {
		t.Errorf("expected error for unknown entity type")
	}
}

// TestOpenFocusIntervalGuard verifies the same-entity guard: re-focusing the
// same entity is a no-op (no degenerate interval), while focusing a different
// entity closes the prior interval and opens a new one.
func TestOpenFocusIntervalGuard(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()
	db := st.DB()
	key := store.SessionKey{SessionID: "sess1", AgentName: "@spine"}

	// First focus → one open interval.
	if err := st.OpenFocusInterval(ctx, key, "task", "t1"); err != nil {
		t.Fatalf("open t1: %v", err)
	}
	// Re-focus the SAME entity → guard makes this a no-op (still one row).
	if err := st.OpenFocusInterval(ctx, key, "task", "t1"); err != nil {
		t.Fatalf("re-open t1: %v", err)
	}
	if got := countIntervals(t, db, "sess1", "@spine"); got != 1 {
		t.Fatalf("same-entity re-focus should not add a row; got %d intervals", got)
	}
	var openCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess1' AND agent_name='@spine' AND ended_at IS NULL`).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected exactly 1 open interval, got %d", openCount)
	}

	// Focus a DIFFERENT entity → closes t1, opens t2 (2 rows, 1 open).
	if err := st.OpenFocusInterval(ctx, key, "task", "t2"); err != nil {
		t.Fatalf("open t2: %v", err)
	}
	if got := countIntervals(t, db, "sess1", "@spine"); got != 2 {
		t.Fatalf("switching entity should add a row; got %d intervals", got)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess1' AND agent_name='@spine' AND ended_at IS NULL`).Scan(&openCount); err != nil {
		t.Fatalf("count open after switch: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("after switch expected 1 open interval, got %d", openCount)
	}
}

// TestCloseFocusInterval verifies the pure close: it ends the open interval
// WITHOUT inserting a new (phantom empty-entity) row, and is a no-op when
// nothing is open.
func TestCloseFocusInterval(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()
	db := st.DB()
	key := store.SessionKey{SessionID: "sess1", AgentName: "@spine"}

	// No-op when nothing is open: must not error and must not create a row.
	if err := st.CloseFocusInterval(ctx, key); err != nil {
		t.Fatalf("close with nothing open: %v", err)
	}
	if got := countIntervals(t, db, "sess1", "@spine"); got != 0 {
		t.Fatalf("close-when-empty must not create a row; got %d", got)
	}

	// Open one, then close it.
	if err := st.OpenFocusInterval(ctx, key, "outcome", "out1"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.CloseFocusInterval(ctx, key); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Exactly one row total (the closed one) — NO phantom empty-entity interval.
	if got := countIntervals(t, db, "sess1", "@spine"); got != 1 {
		t.Fatalf("close must not insert a new row; got %d intervals", got)
	}
	var openCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess1' AND agent_name='@spine' AND ended_at IS NULL`).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 0 {
		t.Fatalf("after close expected 0 open intervals, got %d", openCount)
	}
	// The closed interval still points at the real entity, not an empty one.
	var entityType, entityID string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id FROM wms_intervals WHERE kind='focus' AND session_id='sess1' AND agent_name='@spine'`).Scan(&entityType, &entityID); err != nil {
		t.Fatalf("read closed interval: %v", err)
	}
	if entityType != "outcome" || entityID != "out1" {
		t.Errorf("closed interval should retain its entity, got %s/%s", entityType, entityID)
	}

	// Idempotent: closing again is a no-op.
	if err := st.CloseFocusInterval(ctx, key); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got := countIntervals(t, db, "sess1", "@spine"); got != 1 {
		t.Fatalf("second close must not add a row; got %d", got)
	}
}

// TestCloseFocusIntervalForEntity is the P2 close-on-done regression: the
// entity-scoped close must end the open interval ONLY when its entity matches.
// Reproduces the bug it fixes — a `done` for a child entity (B) while the agent
// is focused on a parent (A) must leave A's interval open; only a `done` for A
// closes A. This is what stops a lead's parent-Outcome focus from being orphaned
// (and all subsequent coordination cost going unallocated) when a child WorkUnit
// completes.
func TestCloseFocusIntervalForEntity(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()
	db := st.DB()
	key := store.SessionKey{SessionID: "sess1", AgentName: "@spine"}

	openCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess1' AND agent_name='@spine' AND ended_at IS NULL`).Scan(&n); err != nil {
			t.Fatalf("count open: %v", err)
		}
		return n
	}

	// No-op when nothing is open: must not error and must not create a row.
	if err := st.CloseFocusIntervalForEntity(ctx, key, "outcome", "out-A"); err != nil {
		t.Fatalf("close-for-entity with nothing open: %v", err)
	}
	if got := countIntervals(t, db, "sess1", "@spine"); got != 0 {
		t.Fatalf("no-op close must not create a row; got %d", got)
	}

	// Agent is focused on entity A (the parent Outcome).
	if err := st.OpenFocusInterval(ctx, key, "outcome", "out-A"); err != nil {
		t.Fatalf("open A: %v", err)
	}

	// A `done` for a DIFFERENT entity B (the child WorkUnit) must be a no-op:
	// A stays open. This is the bug — the old unconditional close ended A here.
	if err := st.CloseFocusIntervalForEntity(ctx, key, "workunit", "wu-B"); err != nil {
		t.Fatalf("close-for-entity B: %v", err)
	}
	if got := openCount(); got != 1 {
		t.Fatalf("done(B) must leave A's focus open; got %d open intervals", got)
	}
	// Same-id but different type must also miss (scoping is on the full pair).
	if err := st.CloseFocusIntervalForEntity(ctx, key, "workunit", "out-A"); err != nil {
		t.Fatalf("close-for-entity type-mismatch: %v", err)
	}
	if got := openCount(); got != 1 {
		t.Fatalf("type-mismatched done must leave A open; got %d open intervals", got)
	}

	// A `done` for entity A itself closes A — the intended behavior is preserved.
	if err := st.CloseFocusIntervalForEntity(ctx, key, "outcome", "out-A"); err != nil {
		t.Fatalf("close-for-entity A: %v", err)
	}
	if got := openCount(); got != 0 {
		t.Fatalf("done(A) must close A's focus; got %d open intervals", got)
	}
	// Exactly one row total — the closed A interval; no phantom rows from the misses.
	if got := countIntervals(t, db, "sess1", "@spine"); got != 1 {
		t.Fatalf("close-for-entity must not insert rows; got %d intervals", got)
	}
	// The closed interval retains A's identity.
	var entityType, entityID string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id FROM wms_intervals WHERE kind='focus' AND session_id='sess1' AND agent_name='@spine'`).Scan(&entityType, &entityID); err != nil {
		t.Fatalf("read closed interval: %v", err)
	}
	if entityType != "outcome" || entityID != "out-A" {
		t.Errorf("closed interval should retain entity A, got %s/%s", entityType, entityID)
	}
}

func countIntervals(t *testing.T, db *sql.DB, sessionID, agent string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=?`,
		sessionID, agent).Scan(&n); err != nil {
		t.Fatalf("count intervals: %v", err)
	}
	return n
}

// TestTagCategoriesAndGetEntityTags verifies the v14 tag-category schema: seed
// tags carry their category, GetEntityTags returns the binding source plus the
// tag's category, the 'progress' vocabulary is gone, and a new operator key
// defaults to category 'context'.
func TestTagCategoriesAndGetEntityTags(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	tags, err := st.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	byKV := map[string]string{} // "key=value" -> category
	for _, tg := range tags {
		byKV[tg.Key+"="+tg.Value] = tg.Category
		if tg.Key == "progress" {
			t.Errorf("progress vocabulary should be dropped by v14, found %s=%s", tg.Key, tg.Value)
		}
	}
	if got := byKV["phase=build"]; got != "lifecycle" {
		t.Errorf("phase=build category = %q, want lifecycle", got)
	}
	if got := byKV["scope=strategic"]; got != "context" {
		t.Errorf("scope=strategic category = %q, want context", got)
	}
	// v27 renamed the legacy 'project' context key to 'product'; v36 added 'user'.
	// On a fully-migrated fresh DB the durable area-of-work context seed is
	// 'product' (the rename survivor), not 'project'.
	if got, ok := byKV["product="]; !ok || got != "context" {
		t.Errorf("expected 'product' context seed in ListTags (v27 project→product rename), got category %q present=%v", got, ok)
	}
	if got, ok := byKV["user="]; !ok || got != "context" {
		t.Errorf("expected 'user' context seed in ListTags (v36), got category %q present=%v", got, ok)
	}
	if _, ok := byKV["project="]; ok {
		t.Errorf("'project' context seed should be gone after v27 project→product rename, but found it")
	}

	// A new operator key defaults to category 'context'.
	if err := st.TagEntity(ctx, "outcome", "o1", "release", "v0.2", "manual", "the next release"); err != nil {
		t.Fatalf("TagEntity new: %v", err)
	}
	if err := st.TagEntity(ctx, "outcome", "o1", "phase", "build", "classifier", ""); err != nil {
		t.Fatalf("TagEntity seed: %v", err)
	}

	ets, err := st.GetEntityTags(ctx, "outcome", "o1")
	if err != nil {
		t.Fatalf("GetEntityTags: %v", err)
	}
	if len(ets) != 2 {
		t.Fatalf("expected 2 entity tags, got %d", len(ets))
	}
	seen := map[string]struct{}{}
	for _, et := range ets {
		seen[et.TagKey+"="+et.TagValue] = struct{}{}
		if et.AppliedAt.IsZero() {
			t.Errorf("%s=%s: applied_at should be populated", et.TagKey, et.TagValue)
		}
		switch et.TagKey {
		case "release":
			if et.Category != "context" || et.Source != "manual" {
				t.Errorf("release tag: category=%q source=%q, want context/manual", et.Category, et.Source)
			}
			if et.Description != "the next release" {
				t.Errorf("release description = %q, want 'the next release'", et.Description)
			}
		case "phase":
			if et.Category != "lifecycle" || et.Source != "classifier" {
				t.Errorf("phase tag: category=%q source=%q, want lifecycle/classifier", et.Category, et.Source)
			}
		}
	}
	if _, ok := seen["release=v0.2"]; !ok {
		t.Errorf("GetEntityTags missing release=v0.2")
	}
}

// TestTagDescriptionBackfill verifies §4 description ordering: an empty
// description is backfilled by a later caller that supplies one, but a
// non-empty description is never overwritten.
func TestTagDescriptionBackfill(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()
	db := st.DB()

	descOf := func(key, val string) string {
		t.Helper()
		var d string
		if err := db.QueryRowContext(ctx,
			`SELECT description FROM tags WHERE tag_key=? AND tag_value=?`, key, val).Scan(&d); err != nil {
			t.Fatalf("query desc %s=%s: %v", key, val, err)
		}
		return d
	}

	// Create with empty description, then backfill it.
	if err := st.TagEntity(ctx, "outcome", "o1", "area", "billing", "manual", ""); err != nil {
		t.Fatalf("create empty desc: %v", err)
	}
	if got := descOf("area", "billing"); got != "" {
		t.Fatalf("expected empty description initially, got %q", got)
	}
	if err := st.TagEntity(ctx, "outcome", "o2", "area", "billing", "classifier", "billing subsystem"); err != nil {
		t.Fatalf("backfill desc: %v", err)
	}
	if got := descOf("area", "billing"); got != "billing subsystem" {
		t.Errorf("description should be backfilled, got %q", got)
	}

	// A later non-empty description must NOT overwrite the existing one.
	if err := st.TagEntity(ctx, "outcome", "o3", "area", "billing", "manual", "WRONG"); err != nil {
		t.Fatalf("re-tag: %v", err)
	}
	if got := descOf("area", "billing"); got != "billing subsystem" {
		t.Errorf("existing description must not be clobbered, got %q", got)
	}
}
