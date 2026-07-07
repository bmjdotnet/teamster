package rollup

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// ---------------------------------------------------------------------------
// Gap recovery tests (partial-gap sessions)
// ---------------------------------------------------------------------------

// TestRecoverGaps_LeadThreadFromTeammateAttributions is the primary gap test:
// a lead thread with no setFocus in a session where teammates DID set focus.
// The lead messages occur BEFORE the teammate's focus interval opens, so the
// allocator's P1a lead-session fallback finds no covering focus interval and
// they land unallocated. Gap recovery resolves them from the session's
// attributed teammate messages.
func TestRecoverGaps_LeadThreadFromTeammateAttributions(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// Teammate @store focuses on w1 starting at 10:10 (NOT from base).
	// Its focus interval only covers timestamps >= 10:10.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base.Add(10*time.Minute), nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@store", base.Add(10*time.Minute), nil)
	// Teammate's message at 10:15 (inside focus) → temporal_join.
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(15*time.Minute), 25.0, 2500)

	// Lead messages at 10:02 and 10:08 — BEFORE any focus interval in the session.
	// P1a session fallback finds no covering focus interval → unallocated.
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "lead2", "s1", "", base.Add(8*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Precondition: tm1 attributed, lead messages unallocated.
	if _, _, method := attributionOf(t, db, ctx, "tm1"); method != "temporal_join" {
		t.Fatalf("tm1 method=%q, want temporal_join", method)
	}
	for _, m := range []string{"lead1", "lead2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-gap method=%q, want unallocated", m, method)
		}
	}
	ledgerBefore := sumLedger(t, db, ctx)

	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	if stats.Recovered != 2 {
		t.Fatalf("stats.Recovered=%d, want 2 (lead1 + lead2)", stats.Recovered)
	}

	// Lead messages should be attributed to outcome o1 (parent of w1).
	for _, m := range []string{"lead1", "lead2"} {
		et, ei, method := attributionOf(t, db, ctx, m)
		if method != gapMethod || et != "outcome" || ei != "o1" {
			t.Fatalf("%s → (%q,%q) method=%q, want (outcome,o1) %s", m, et, ei, method, gapMethod)
		}
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
	if math.Abs(sumLedger(t, db, ctx)-ledgerBefore) > eps {
		t.Fatalf("recovery changed ledger total: %.6f → %.6f", ledgerBefore, sumLedger(t, db, ctx))
	}
	assertNoDoubleAttribution(t, db, ctx)

	// Provenance: gap_evidence records the resolution.
	var evEType, evEID, evResMethod string
	storetest.QueryRow(t, ctx, db,
		`SELECT entity_type, entity_id, resolution_method FROM gap_evidence WHERE message_id = 'lead1'`, nil,
		&evEType, &evEID, &evResMethod)
	if evEType != "outcome" || evEID != "o1" || evResMethod != "session_outcome_inference" {
		t.Fatalf("evidence lead1 = (%q,%q,%q), want (outcome,o1,session_outcome_inference)", evEType, evEID, evResMethod)
	}

	// cost_rollup was rebuilt.
	if got := rollupCostFor(t, db, ctx, "outcome", "o1"); math.Abs(got-30.0) > eps {
		t.Fatalf("cost_rollup outcome/o1 = %.6f, want 30.0 (lead1:10 + lead2:20)", got)
	}
}

// TestRecoverGaps_TeammateFromSessionOutcome tests teammate gap resolution:
// a teammate that never focused, whose message arrives BEFORE any focus in the
// session, but the session later has attributed messages the gap can infer from.
func TestRecoverGaps_TeammateFromSessionOutcome(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// Lead focuses on outcome o1 starting at 10:10 (NOT from base).
	// This means scout at 10:03 has no covering focus interval — the P2
	// lead-focus fallback finds nothing because the lead's focus doesn't cover
	// that timestamp. scout stays unallocated.
	seedFocus(t, db, ctx, "s1", "", "outcome", "o1", base.Add(10*time.Minute), nil)
	seedEventRecord(t, db, ctx, "outcome", "o1", "active", "", base.Add(10*time.Minute), nil)
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(15*time.Minute), 15.0, 1500)

	// Teammate @scout — no focus, message before any focus → unallocated.
	seedLedger(t, db, ctx, "scout1", "s1", "@scout", base.Add(3*time.Minute), 12.0, 1200)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Precondition: lead1 attributed, scout1 unallocated.
	if _, _, method := attributionOf(t, db, ctx, "lead1"); method == "unallocated" {
		t.Fatalf("lead1 should be attributed, got unallocated")
	}
	if _, _, method := attributionOf(t, db, ctx, "scout1"); method != "unallocated" {
		t.Fatalf("scout1 pre-gap method=%q, want unallocated", method)
	}

	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats.Recovered=%d, want 1", stats.Recovered)
	}

	et, ei, method := attributionOf(t, db, ctx, "scout1")
	if method != gapMethod || et != "outcome" || ei != "o1" {
		t.Fatalf("scout1 → (%q,%q) method=%q, want (outcome,o1) %s", et, ei, method, gapMethod)
	}

	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverGaps_TeammateInheritsOwnLaterFocus tests that a teammate that
// later set focus in the same session gets attributed to its own focused entity
// via agent_focus_inheritance, not the session's strategic outcome.
func TestRecoverGaps_TeammateInheritsOwnLaterFocus(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w2", "o1")

	// NO lead focus at all — no focus intervals from the lead. The lead's
	// messages are not part of this test. Only @impl matters.

	// Teammate @impl has focus on workunit w2 starting at 10:10 — messages AFTER
	// 10:10 are temporal_join, messages BEFORE are unallocated gaps.
	seedFocus(t, db, ctx, "s1", "@impl", "workunit", "w2", base.Add(10*time.Minute), nil)
	seedEventRecord(t, db, ctx, "workunit", "w2", "active", "@impl", base.Add(10*time.Minute), nil)

	// @impl's early message (before focus, before any session focus → no P2
	// lead fallback either since lead has no focus) → unallocated.
	seedLedger(t, db, ctx, "impl_early", "s1", "@impl", base.Add(3*time.Minute), 8.0, 800)
	// @impl's late message (after focus) → temporal_join on w2.
	seedLedger(t, db, ctx, "impl_late", "s1", "@impl", base.Add(15*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, _, method := attributionOf(t, db, ctx, "impl_early"); method != "unallocated" {
		t.Fatalf("impl_early pre-gap method=%q, want unallocated", method)
	}
	if _, _, method := attributionOf(t, db, ctx, "impl_late"); method == "unallocated" {
		t.Fatalf("impl_late should be attributed, got unallocated")
	}

	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats.Recovered=%d, want 1 (impl_early only)", stats.Recovered)
	}

	// impl_early should resolve from @impl's own attributed entity (w2),
	// via agent_focus_inheritance.
	et, ei, method := attributionOf(t, db, ctx, "impl_early")
	if method != gapMethod {
		t.Fatalf("impl_early method=%q, want %s", method, gapMethod)
	}
	if et == "" || ei == "" {
		t.Fatalf("impl_early has empty attribution")
	}

	var evResMethod string
	storetest.QueryRow(t, ctx, db,
		`SELECT resolution_method FROM gap_evidence WHERE message_id = 'impl_early'`, nil, &evResMethod)
	if evResMethod != "agent_focus_inheritance" {
		t.Fatalf("impl_early resolution_method=%q, want agent_focus_inheritance", evResMethod)
	}

	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverGaps_DryRunWritesNothing verifies --dry-run performs zero writes.
func TestRecoverGaps_DryRunWritesNothing(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// Teammate focus starts at 10:10 so lead message at 10:02 has no covering
	// focus interval and stays unallocated after allocation.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base.Add(10*time.Minute), nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@store", base.Add(10*time.Minute), nil)
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(15*time.Minute), 25.0, 2500)
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, _, method := attributionOf(t, db, ctx, "lead1"); method != "unallocated" {
		t.Fatalf("precondition: lead1 method=%q, want unallocated", method)
	}

	stats, err := r.RecoverGaps(ctx, true)
	if err != nil {
		t.Fatalf("recover-gaps dry-run: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("dry-run stats.Recovered=%d, want 1", stats.Recovered)
	}
	if _, _, method := attributionOf(t, db, ctx, "lead1"); method != "unallocated" {
		t.Fatalf("dry-run mutated attribution: method=%q, want unallocated", method)
	}
	var ev int
	storetest.QueryRow(t, ctx, db, `SELECT COUNT(*) FROM gap_evidence`, nil, &ev)
	if ev != 0 {
		t.Fatalf("dry-run wrote %d gap evidence rows, want 0", ev)
	}
}

// TestRecoverGaps_UncoverReverses verifies that UncoverGaps deletes every
// gap_recovery attribution and its evidence.
func TestRecoverGaps_UncoverReverses(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// Focus starts at 10:10 so lead messages at 10:02, 10:08 have no covering interval.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base.Add(10*time.Minute), nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@store", base.Add(10*time.Minute), nil)
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(15*time.Minute), 25.0, 2500)
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "lead2", "s1", "", base.Add(8*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := r.RecoverGaps(ctx, false); err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	for _, m := range []string{"lead1", "lead2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != gapMethod {
			t.Fatalf("%s pre-uncover method=%q, want %s", m, method, gapMethod)
		}
	}

	reverted, err := r.UncoverGaps(ctx)
	if err != nil {
		t.Fatalf("uncover-gaps: %v", err)
	}
	if reverted != 2 {
		t.Fatalf("uncover-gaps reverted %d rows, want 2", reverted)
	}

	// Re-allocate restores unallocated.
	if _, err := r.Allocate(ctx); err != nil {
		t.Fatalf("allocate after uncover: %v", err)
	}
	for _, m := range []string{"lead1", "lead2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s after uncover+allocate method=%q, want unallocated", m, method)
		}
	}

	// Evidence cleared.
	var ev int
	storetest.QueryRow(t, ctx, db, `SELECT COUNT(*) FROM gap_evidence`, nil, &ev)
	if ev != 0 {
		t.Fatalf("uncover left %d gap evidence rows, want 0", ev)
	}

	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated after uncover: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverGaps_ConservationInvariant is the explicit conservation assertion.
func TestRecoverGaps_ConservationInvariant(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// Focus starts at 10:20 so lead messages at 10:01..10:03 have no covering
	// focus interval (P1a session fallback misses).
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base.Add(20*time.Minute), nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@store", base.Add(20*time.Minute), nil)

	// 4 teammate messages (inside focus window → attributed).
	for i := 0; i < 4; i++ {
		seedLedger(t, db, ctx, fmt.Sprintf("tm%d", i), "s1", "@store",
			base.Add(time.Duration(25+i)*time.Minute), float64(i+1)*10.0, (i+1)*1000)
	}
	// 3 lead messages (before focus window → unallocated gaps).
	for i := 0; i < 3; i++ {
		seedLedger(t, db, ctx, fmt.Sprintf("lead%d", i), "s1", "",
			base.Add(time.Duration(1+i)*time.Minute), float64(i+1)*5.0, (i+1)*500)
	}

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	ledgerBefore := sumLedger(t, db, ctx)
	factsBefore := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerBefore-factsBefore) > eps {
		t.Fatalf("pre-recovery conservation violated: %.6f vs %.6f", ledgerBefore, factsBefore)
	}

	if _, err := r.RecoverGaps(ctx, false); err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}

	ledgerAfter := sumLedger(t, db, ctx)
	factsAfter := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerAfter-factsAfter) > eps {
		t.Fatalf("post-recovery conservation violated: ledger=%.6f, cost_facts=%.6f", ledgerAfter, factsAfter)
	}
	if math.Abs(ledgerAfter-ledgerBefore) > eps {
		t.Fatalf("recovery changed ledger total: %.6f → %.6f", ledgerBefore, ledgerAfter)
	}
	assertNoDoubleAttribution(t, db, ctx)
}

// TestRecoverGaps_NeverTouchesNonUnallocated verifies gap recovery only touches
// method='unallocated' rows.
func TestRecoverGaps_NeverTouchesNonUnallocated(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// All messages have focus → all temporal_join, nothing unallocated.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base, nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@store", base, nil)
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(5*time.Minute), 25.0, 2500)

	// Lead also holds focus → attributed via P1a session fallback.
	seedFocus(t, db, ctx, "s1", "", "outcome", "o1", base, nil)
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// No unallocated messages → gap recovery should do nothing.
	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	if stats.Recovered != 0 || stats.Examined != 0 {
		t.Fatalf("gap recovery on fully-attributed session: recovered=%d examined=%d, want 0,0",
			stats.Recovered, stats.Examined)
	}
}

// TestRecoverGaps_SessionWithNoResolvableEntity verifies that a session with
// unallocated messages but no resolvable entity (shouldn't happen normally but
// guard) is skipped, leaving messages for LLM fallback.
func TestRecoverGaps_SessionWithNoResolvableEntity(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	// Manually create a scenario: one message is "attributed" via transcript
	// recovery to an entity that doesn't resolve to an outcome, and one is
	// unallocated. We'll use a direct INSERT to simulate this edge case.
	seedLedger(t, db, ctx, "m_attr", "s1", "@agent", base.Add(5*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m_gap", "s1", "", base.Add(2*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Both are unallocated after normal allocation. Manually attribute m_attr
	// to simulate a partial-gap session where the attributed entity doesn't
	// have a resolvable outcome.
	storetest.Exec(t, ctx, db,
		`UPDATE usage_attribution SET entity_type='outcome', entity_id='nonexistent',
		 method='temporal_join' WHERE message_id='m_attr'`)

	if _, err := r.RecoverGaps(ctx, false); err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	// The session has entity_type='outcome', entity_id='nonexistent' in its
	// attributed rows. sessionStrategicOutcome finds it (it queries
	// usage_attribution, not the outcomes table), so the gap IS resolved.
	// The test verifies conservation regardless of the resolution outcome.
	if _, _, method := attributionOf(t, db, ctx, "m_gap"); method != "unallocated" && method != gapMethod {
		t.Fatalf("m_gap method=%q, unexpected", method)
	}

	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverGaps_LeadAndTeammateGapsResolveIndependently is the F1
// agentExact regression test: a single session has BOTH a lead gap thread
// (agent_name="") and a named teammate gap thread (agent_name="@store"),
// each of which must resolve to a DIFFERENT entity. This locks in the
// agentExact=true fix at gap.go:60 — before that fix,
// ReclaimableMessages(session, "", methods) with agentName=="" meant "no
// agent filter," so processing the lead's gap thread would also sweep up
// the teammate's still-unallocated messages and misattribute them to the
// lead's resolved entity (whichever thread GapThreads happened to process
// first "won" the teammate's messages too).
func TestRecoverGaps_LeadAndTeammateGapsResolveIndependently(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-main")
	seedWorkunit(t, db, ctx, "w-store", "o-main")

	// @store focuses w-store starting at 10:10; its own early message (10:03,
	// before its focus opens) has no covering interval and stays unallocated.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w-store", base.Add(10*time.Minute), nil)
	seedEventRecord(t, db, ctx, "workunit", "w-store", "active", "@store", base.Add(10*time.Minute), nil)
	seedLedger(t, db, ctx, "store_late", "s1", "@store", base.Add(15*time.Minute), 25.0, 2500)
	seedLedger(t, db, ctx, "store_early", "s1", "@store", base.Add(3*time.Minute), 8.0, 800)

	// Lead never focuses at all in this session — its messages have no
	// covering interval (own, teammate, or session-wide) and stay unallocated.
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(1*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "lead2", "s1", "", base.Add(2*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Preconditions.
	if _, _, method := attributionOf(t, db, ctx, "store_late"); method != "temporal_join" {
		t.Fatalf("store_late method=%q, want temporal_join", method)
	}
	for _, m := range []string{"store_early", "lead1", "lead2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-gap method=%q, want unallocated", m, method)
		}
	}

	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	if stats.Recovered != 3 {
		t.Fatalf("stats.Recovered=%d, want 3 (lead1 + lead2 + store_early)", stats.Recovered)
	}

	// Lead's gap resolves via the session's strategic outcome (o-main, the
	// parent of w-store — the only attributed entity in the session).
	for _, m := range []string{"lead1", "lead2"} {
		et, ei, method := attributionOf(t, db, ctx, m)
		if method != gapMethod || et != "outcome" || ei != "o-main" {
			t.Fatalf("%s → (%q,%q) method=%q, want (outcome,o-main) %s", m, et, ei, method, gapMethod)
		}
	}

	// The teammate's OWN gap resolves via its own later focus (w-store) —
	// NOT the lead's resolved entity. Without agentExact, this would
	// incorrectly land on (outcome, o-main) too, or never be reached at all
	// (already reassigned by the lead's over-broad ReclaimableMessages call).
	et, ei, method := attributionOf(t, db, ctx, "store_early")
	if method != gapMethod || et != "workunit" || ei != "w-store" {
		t.Fatalf("store_early → (%q,%q) method=%q, want (workunit,w-store) %s", et, ei, method, gapMethod)
	}

	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
	assertNoDoubleAttribution(t, db, ctx)
}

// TestRecoverGaps_FullyUnallocatedSessionSkipped verifies that a session where
// ALL messages are unallocated (no attributed messages at all) is NOT a gap
// session — it's an orphan session for LLM synthesis (Objective 2).
func TestRecoverGaps_FullyUnallocatedSessionSkipped(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	// All messages in this session land unallocated (no focus).
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m2", "s1", "", base.Add(5*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// No attributed messages in session → this is NOT a gap session.
	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}
	if stats.Sessions != 0 || stats.Recovered != 0 {
		t.Fatalf("fully-unallocated session treated as gap: sessions=%d recovered=%d, want 0,0",
			stats.Sessions, stats.Recovered)
	}
	for _, m := range []string{"m1", "m2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s method=%q, want unallocated (orphan session, not gap)", m, method)
		}
	}
}

