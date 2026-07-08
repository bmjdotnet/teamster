package store_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/rollup"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// TestReallocateClearsSweepSkippedTrap is the regression for the reallocate
// attribution race proven live on the chunk test VM: after an agent-identity
// backfill, a manual --reallocate must recover a message even when the
// automatic sweep already relabeled its unallocated row to 'sweep_skipped'.
//
// The trap: 'sweep_skipped' rows carry entity_type='' (they are a "tried, still
// unallocatable" marker MarkSessionSweepSkipped/applySkip write off an
// unallocated row without setting an entity), but the OLD Reallocate cleared
// only method='unallocated'. So a sweep_skipped row was cleared by neither
// Reallocate (wrong method) nor re-derived by Allocate (its anti-join skips any
// message that already holds an attribution row) — --reallocate silently no-op'd
// on exactly the rows the operator ran it to recover. The fix scopes the
// clear-set to entity_type='' (ClearUnallocatedAttribution), covering both the
// 'unallocated' and 'sweep_skipped' arms of the trap class.
//
// The test also guards the hard invariant in the SAME pass: a row carrying a
// REAL entity (here a gap_recovery attribution, method != unallocated/
// sweep_skipped) is never cleared, so reallocate cannot disturb good
// attribution. Under the old method='unallocated' scope the m2 assertion fails.
//
// Skips when TEAMSTER_TEST_MYSQL_DSN is unset. Never touches the live DB.
func TestReallocateClearsSweepSkippedTrap(t *testing.T) {
	st := storetest.Open(t, "teamster_test_realloc_race")
	ctx := context.Background()
	db := st
	base := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)

	// Two @spine messages, no covering focus interval yet — both allocate to
	// the unallocated bucket on the first pass.
	insertLedger(t, db, ctx, "m1", "s1", "spine", base.Add(5*time.Minute), 10.0)
	insertLedger(t, db, ctx, "m2", "s1", "spine", base.Add(6*time.Minute), 20.0)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := rollup.NewRunner(st, st, st, st, st, st, nil, discard)

	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("initial run: %v", err)
	}
	assertMethod(t, db, ctx, "m1", "unallocated", "", "")
	assertMethod(t, db, ctx, "m2", "unallocated", "", "")

	// m1 is the GUARD row: a prior recovery pass attributed it to a real entity
	// (a gap_recovery row on outcome o1). Reallocate must never clear it.
	mustExec(t, db, ctx,
		`UPDATE usage_attribution SET method='gap_recovery', entity_type='outcome', entity_id='o1' WHERE message_id='m1'`)
	// m2 is the TRAP row: the automatic sweep found no local transcript and
	// relabeled its unallocated row to 'sweep_skipped' (entity_type still '').
	mustExec(t, db, ctx,
		`UPDATE usage_attribution SET method='sweep_skipped' WHERE message_id='m2'`)
	assertMethod(t, db, ctx, "m1", "gap_recovery", "outcome", "o1")
	assertMethod(t, db, ctx, "m2", "sweep_skipped", "", "")

	// Identity backfill: a re-scrape now materializes @spine's focus on o1 for
	// the whole window, so m2's timestamp is finally covered.
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"s1", "@spine", "outcome", "o1", base, nil)

	m1Before := computedAt(t, db, ctx, "m1")

	// --reallocate: clears the entity-less trap row (m2), re-derives it onto o1;
	// leaves the real-entity guard row (m1) untouched.
	if err := r.Run(ctx, true); err != nil {
		t.Fatalf("reallocate run: %v", err)
	}
	assertRowCount(t, db, ctx, "usage_attribution", 2) // no duplicate, no loss
	assertMethod(t, db, ctx, "m1", "gap_recovery", "outcome", "o1")   // guard: untouched
	assertMethod(t, db, ctx, "m2", "temporal_join", "outcome", "o1")  // trap: recovered
	if got := computedAt(t, db, ctx, "m1"); !got.Equal(m1Before) {
		t.Fatalf("guard row m1 was rewritten by reallocate: computed_at %v -> %v (real allocation cleared)", m1Before, got)
	}
	assertConserved(t, db, ctx, 30.0)

	// Idempotency: a second reallocate re-derives to the identical state.
	if err := r.Run(ctx, true); err != nil {
		t.Fatalf("second reallocate run: %v", err)
	}
	assertRowCount(t, db, ctx, "usage_attribution", 2)
	assertMethod(t, db, ctx, "m1", "gap_recovery", "outcome", "o1")
	assertMethod(t, db, ctx, "m2", "temporal_join", "outcome", "o1")
	assertConserved(t, db, ctx, 30.0)

	// SUM(weight)=1 per message must still hold everywhere.
	var bad int
	storetest.QueryRow(t, ctx, db,
		`SELECT COUNT(*) FROM (SELECT message_id, SUM(weight) s FROM usage_attribution
		 GROUP BY message_id HAVING ABS(s-1.0) > 1e-6) x`, nil, &bad)
	if bad != 0 {
		t.Fatalf("weight invariant violated after reallocate: %d messages", bad)
	}
}
