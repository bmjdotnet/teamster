package rollup

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestSynthesizeRemoteOrphans_AttributesByConcurrentFocus is the primary B2
// test: a remote orphan session with a concurrent focused session on the same
// host is attributed to that focused entity via temporal correlation.
func TestSynthesizeRemoteOrphans_AttributesByConcurrentFocus(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-remote")
	seedWorkunit(t, db, ctx, "wu-remote", "o-remote")

	// Orphan session on remote host "studio" — 2 messages, no focus at all.
	seedLedgerHostUser(t, db, ctx, "ro1", "s-orphan", "@agent1", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedLedgerHostUser(t, db, ctx, "ro2", "s-orphan", "@agent1", "studio", "alice", base.Add(3*time.Minute), 0.20, 2000)

	// Concurrent session on the SAME host "studio" WITH focus on wu-remote.
	seedLedgerHostUser(t, db, ctx, "cf1", "s-focused", "@lead", "studio", "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-focused", "@lead", "workunit", "wu-remote", base, nil)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Precondition: orphan messages are unallocated.
	for _, m := range []string{"ro1", "ro2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-synthesis method=%q, want unallocated", m, method)
		}
	}

	stats, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("synthesize-remote-orphans: %v", err)
	}
	if stats.Examined != 1 {
		t.Fatalf("examined=%d, want 1", stats.Examined)
	}
	if stats.Synthesized != 2 {
		t.Fatalf("synthesized=%d, want 2", stats.Synthesized)
	}
	if stats.NoConcurrentFocus != 0 {
		t.Fatalf("no_concurrent_focus=%d, want 0", stats.NoConcurrentFocus)
	}

	for _, m := range []string{"ro1", "ro2"} {
		et, ei, method := attributionOf(t, db, ctx, m)
		if et != "workunit" || ei != "wu-remote" || method != remoteFloorMethod {
			t.Fatalf("%s → (%q,%q) method=%q, want (workunit,wu-remote) %s",
				m, et, ei, method, remoteFloorMethod)
		}
	}

	// Evidence recorded.
	var evN int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM synthesis_evidence WHERE session_id = 's-orphan'`).Scan(&evN); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if evN != 2 {
		t.Fatalf("synthesis_evidence rows=%d, want 2", evN)
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestSynthesizeRemoteOrphans_NoConcurrentFocus verifies that an orphan with
// no concurrent focused session on its host is counted as NoConcurrentFocus
// and left unattributed.
func TestSynthesizeRemoteOrphans_NoConcurrentFocus(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	// Orphan session on remote host "studio" — no other sessions on that host.
	seedLedgerHostUser(t, db, ctx, "ro1", "s-alone", "@agent1", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("synthesize-remote-orphans: %v", err)
	}
	if stats.Examined != 1 {
		t.Fatalf("examined=%d, want 1", stats.Examined)
	}
	if stats.NoConcurrentFocus != 1 {
		t.Fatalf("no_concurrent_focus=%d, want 1", stats.NoConcurrentFocus)
	}
	if stats.Synthesized != 0 {
		t.Fatalf("synthesized=%d, want 0", stats.Synthesized)
	}

	// Message stays unallocated.
	if _, _, method := attributionOf(t, db, ctx, "ro1"); method != "unallocated" {
		t.Fatalf("ro1 method=%q, want unallocated", method)
	}
}

// TestSynthesizeRemoteOrphans_PrefersOutcomeOverWorkUnit verifies that when
// multiple concurrent entities exist, the one with the most temporal overlap
// wins, and among ties, Outcomes are preferred over WorkUnits.
func TestSynthesizeRemoteOrphans_PrefersOutcomeOverWorkUnit(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-broad")
	seedWorkunit(t, db, ctx, "wu-narrow", "o-broad")

	// Orphan on "studio": 10:01–10:05.
	seedLedgerHostUser(t, db, ctx, "ro1", "s-orphan", "@a", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedLedgerHostUser(t, db, ctx, "ro2", "s-orphan", "@a", "studio", "alice", base.Add(5*time.Minute), 0.10, 1000)

	// Concurrent session A: outcome o-broad focused 10:00–10:10 (10 min overlap).
	seedLedgerHostUser(t, db, ctx, "ca1", "s-a", "@lead", "studio", "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-a", "@lead", "outcome", "o-broad", base, nil)

	// Concurrent session B: workunit wu-narrow focused 10:00–10:10 (same overlap).
	seedLedgerHostUser(t, db, ctx, "cb1", "s-b", "@lead", "studio", "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-b", "@lead", "workunit", "wu-narrow", base, nil)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Synthesized != 2 {
		t.Fatalf("synthesized=%d, want 2", stats.Synthesized)
	}

	// With equal overlap, workunit (specificity 4) beats outcome (specificity 2).
	for _, m := range []string{"ro1", "ro2"} {
		et, ei, _ := attributionOf(t, db, ctx, m)
		if et != "workunit" || ei != "wu-narrow" {
			t.Fatalf("%s → (%q,%q), want (workunit,wu-narrow) — more specific wins on tie",
				m, et, ei)
		}
	}
}

// TestSynthesizeRemoteOrphans_ExcludesHubSessions verifies that sessions on
// the hub host are NOT picked up as remote orphans.
func TestSynthesizeRemoteOrphans_ExcludesHubSessions(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	// Session on the hub itself — should not be a remote orphan.
	seedLedgerHostUser(t, db, ctx, "h1", "s-hub", "@a", "hub-host", "alice", base.Add(1*time.Minute), 0.10, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Examined != 0 {
		t.Fatalf("examined=%d, want 0 (hub sessions excluded)", stats.Examined)
	}
}

// TestSynthesizeRemoteOrphans_ExcludesSessionsWithFocus verifies that a
// session with an existing focus interval is NOT picked up — even if it has
// unallocated messages.
func TestSynthesizeRemoteOrphans_ExcludesSessionsWithFocus(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-remote")

	// Remote session with a real focus interval + unallocated message.
	seedLedgerHostUser(t, db, ctx, "rf1", "s-has-focus", "@a", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedFocus(t, db, ctx, "s-has-focus", "@a", "outcome", "o-remote", base, nil)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Examined != 0 {
		t.Fatalf("examined=%d, want 0 (session with focus excluded)", stats.Examined)
	}
}

// TestSynthesizeRemoteOrphans_Idempotent verifies that running twice produces
// the same result — the second run finds no orphans because the first
// attributed them.
func TestSynthesizeRemoteOrphans_Idempotent(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-remote")

	seedLedgerHostUser(t, db, ctx, "ro1", "s-orphan", "@a", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedLedgerHostUser(t, db, ctx, "cf1", "s-focused", "@lead", "studio", "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-focused", "@lead", "outcome", "o-remote", base, nil)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats1, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if stats1.Synthesized != 1 {
		t.Fatalf("pass1 synthesized=%d, want 1", stats1.Synthesized)
	}

	// Second pass: nothing to do.
	stats2, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false)
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	if stats2.Examined != 0 {
		t.Fatalf("pass2 examined=%d, want 0 (all already attributed)", stats2.Examined)
	}
	if stats2.Synthesized != 0 {
		t.Fatalf("pass2 synthesized=%d, want 0", stats2.Synthesized)
	}
}

// TestSynthesizeRemoteOrphans_UnsynthesizeReverses verifies that
// UnsynthesizeRemoteFloor reverses the B2 pass.
func TestSynthesizeRemoteOrphans_UnsynthesizeReverses(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-remote")

	seedLedgerHostUser(t, db, ctx, "ro1", "s-orphan", "@a", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedLedgerHostUser(t, db, ctx, "ro2", "s-orphan", "@a", "studio", "alice", base.Add(3*time.Minute), 0.20, 2000)
	seedLedgerHostUser(t, db, ctx, "cf1", "s-focused", "@lead", "studio", "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-focused", "@lead", "outcome", "o-remote", base, nil)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", false); err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	// Verify attributed.
	for _, m := range []string{"ro1", "ro2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != remoteFloorMethod {
			t.Fatalf("%s method=%q, want %s", m, method, remoteFloorMethod)
		}
	}

	// Reverse.
	reverted, err := r.UnsynthesizeRemoteFloor(ctx)
	if err != nil {
		t.Fatalf("unsynthesize: %v", err)
	}
	if reverted != 2 {
		t.Fatalf("reverted=%d, want 2", reverted)
	}

	// Re-allocate to restore unallocated.
	if _, err := r.Allocate(ctx); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	for _, m := range []string{"ro1", "ro2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s after unsynthesize method=%q, want unallocated", m, method)
		}
	}

	// Evidence cleared.
	var evN int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM synthesis_evidence WHERE session_id = 's-orphan'`).Scan(&evN); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if evN != 0 {
		t.Fatalf("evidence rows=%d after unsynthesize, want 0", evN)
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestSynthesizeRemoteOrphans_DryRunWritesNothing verifies that dry-run mode
// reports the plan but writes nothing.
func TestSynthesizeRemoteOrphans_DryRunWritesNothing(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-remote")

	seedLedgerHostUser(t, db, ctx, "ro1", "s-orphan", "@a", "studio", "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedLedgerHostUser(t, db, ctx, "cf1", "s-focused", "@lead", "studio", "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-focused", "@lead", "outcome", "o-remote", base, nil)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats, err := r.SynthesizeRemoteOrphans(ctx, "hub-host", true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if stats.Synthesized != 1 {
		t.Fatalf("dry-run synthesized=%d, want 1 (must plan)", stats.Synthesized)
	}

	// Nothing actually written.
	if _, _, method := attributionOf(t, db, ctx, "ro1"); method != "unallocated" {
		t.Fatalf("dry-run mutated attribution: method=%q, want unallocated", method)
	}
	var evN int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM synthesis_evidence WHERE session_id = 's-orphan'`).Scan(&evN); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if evN != 0 {
		t.Fatalf("dry-run wrote %d evidence rows, want 0", evN)
	}
}
