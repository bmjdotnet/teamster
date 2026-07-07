package rollup

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// insertInvertedFocus inserts a kind='focus' interval with an explicit
// (started_at, ended_at) so a test can stage the negative-width rows the
// dual-writer/async race produced (ended_at < started_at).
func insertInvertedFocus(t *testing.T, db store.Store, ctx context.Context, session, agent, etype, eid string, startedAt time.Time, endedAt *time.Time) uint64 {
	t.Helper()
	res := storetest.Exec(t, ctx, db, `
		INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		VALUES ('focus','direct',?,?,?,?,?,?)`,
		session, agent, etype, eid, startedAt, endedAt)
	id, _ := res.LastInsertId()
	return uint64(id)
}

func intervalBounds(t *testing.T, db store.Store, ctx context.Context, id uint64) (started time.Time, ended sql.NullTime) {
	t.Helper()
	storetest.QueryRow(t, ctx, db,
		`SELECT started_at, ended_at FROM wms_intervals WHERE id=?`, []any{id}, &started, &ended)
	return started, ended
}

// TestRepairFocusIntervals_FixesChain is the primary repair test: a (session,agent)
// focus chain where the FIRST interval is inverted (ended_at < started_at) is
// repaired to end at the SECOND interval's start; the last (also inverted) interval
// is reopened.
func TestRepairFocusIntervals_FixesChain(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)

	t1 := base
	t2 := base.Add(10 * time.Minute)
	// First focus: inverted (ended before it started — the bug). Successor is t2.
	bad1 := t1.Add(-5 * time.Minute)
	id1 := insertInvertedFocus(t, db, ctx, "s1", "@a", "workunit", "wu-1", t1, &bad1)
	// Second focus: inverted, and it is the LAST in the chain → should reopen.
	bad2 := t2.Add(-3 * time.Minute)
	id2 := insertInvertedFocus(t, db, ctx, "s1", "@a", "workunit", "wu-2", t2, &bad2)

	r := newTestRunner(db)
	stats, err := r.RepairFocusIntervals(ctx, false)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if stats.Inverted != 2 || stats.Repaired != 1 || stats.Reopened != 1 {
		t.Fatalf("stats=%+v, want Inverted=2 Repaired=1 Reopened=1", stats)
	}

	// id1 now ends at t2 (its successor's start).
	if _, ended := intervalBounds(t, db, ctx, id1); !ended.Valid || !ended.Time.Equal(t2) {
		t.Fatalf("id1 ended_at=%v, want %v", ended, t2)
	}
	// id2 reopened (NULL ended_at).
	if _, ended := intervalBounds(t, db, ctx, id2); ended.Valid {
		t.Fatalf("id2 ended_at=%v, want NULL (reopened)", ended)
	}
	// No inverted rows remain.
	var inverted int
	storetest.QueryRow(t, ctx, db,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND ended_at IS NOT NULL AND ended_at < started_at`, nil, &inverted)
	if inverted != 0 {
		t.Fatalf("inverted rows after repair=%d, want 0", inverted)
	}
}

// TestRepairFocusIntervals_Idempotent verifies a second pass is a no-op.
func TestRepairFocusIntervals_Idempotent(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	bad := base.Add(-time.Minute)
	insertInvertedFocus(t, db, ctx, "s1", "@a", "workunit", "wu-1", base, &bad)

	r := newTestRunner(db)
	if _, err := r.RepairFocusIntervals(ctx, false); err != nil {
		t.Fatalf("repair 1: %v", err)
	}
	stats, err := r.RepairFocusIntervals(ctx, false)
	if err != nil {
		t.Fatalf("repair 2: %v", err)
	}
	if stats.Inverted != 0 || stats.Repaired != 0 || stats.Reopened != 0 {
		t.Fatalf("second pass stats=%+v, want all zero (idempotent)", stats)
	}
}

// TestRepairFocusIntervals_Reverses verifies UnrepairFocusIntervals restores the
// prior (bad) ended_at — a true undo of the data change.
func TestRepairFocusIntervals_Reverses(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	t2 := base.Add(10 * time.Minute)
	bad1 := base.Add(-5 * time.Minute)
	id1 := insertInvertedFocus(t, db, ctx, "s1", "@a", "workunit", "wu-1", base, &bad1)
	insertInvertedFocus(t, db, ctx, "s1", "@a", "workunit", "wu-2", t2, &t2) // zero-width-ish successor, not inverted; just provides a successor start

	r := newTestRunner(db)
	if _, err := r.RepairFocusIntervals(ctx, false); err != nil {
		t.Fatalf("repair: %v", err)
	}
	// id1 was repaired to end at t2.
	if _, ended := intervalBounds(t, db, ctx, id1); !ended.Valid || !ended.Time.Equal(t2) {
		t.Fatalf("id1 ended_at=%v post-repair, want %v", ended, t2)
	}

	n, err := r.UnrepairFocusIntervals(ctx)
	if err != nil {
		t.Fatalf("unrepair: %v", err)
	}
	if n != 1 {
		t.Fatalf("unrepair reverted=%d, want 1", n)
	}
	// id1 restored to its prior (bad) ended_at.
	if _, ended := intervalBounds(t, db, ctx, id1); !ended.Valid || !ended.Time.Equal(bad1) {
		t.Fatalf("id1 ended_at=%v post-unrepair, want prior %v", ended, bad1)
	}
	var ev int
	storetest.QueryRow(t, ctx, db, `SELECT COUNT(*) FROM focus_interval_repair`, nil, &ev)
	if ev != 0 {
		t.Fatalf("focus_interval_repair rows=%d after unrepair, want 0", ev)
	}
}

// TestRepairFocusIntervals_RecoversDroppedCost is the end-to-end validation: a
// message that fell to 'unallocated' because its only covering focus interval was
// negative-width gets attributed to that entity once the interval is repaired.
func TestRepairFocusIntervals_RecoversDroppedCost(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-1")
	seedWorkunit(t, db, ctx, "wu-1", "o-1")

	// A message at base+5m. Its focus interval SHOULD cover it, but the interval is
	// inverted (started base, ended base-5m) so focusAt misses → unallocated.
	seedLedgerHostUser(t, db, ctx, "m1", "s1", "@a", "studio", "alice", base.Add(5*time.Minute), 0.50, 5000)
	bad := base.Add(-5 * time.Minute)
	insertInvertedFocus(t, db, ctx, "s1", "@a", "workunit", "wu-1", base, &bad)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("m1 pre-repair method=%q, want unallocated (inverted interval drops it)", method)
	}

	// Repair reopens the (last-in-chain) interval, then reallocates.
	stats, err := r.RepairFocusIntervals(ctx, false)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if stats.Reopened != 1 {
		t.Fatalf("stats=%+v, want Reopened=1", stats)
	}
	// m1 now attributes to wu-1 via the now-open focus interval (temporal_join).
	if et, ei, method := attributionOf(t, db, ctx, "m1"); et != "workunit" || ei != "wu-1" || method != "temporal_join" {
		t.Fatalf("m1 post-repair → (%q,%q) %q, want (workunit,wu-1) temporal_join", et, ei, method)
	}
}
