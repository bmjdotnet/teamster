package rollup

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// seedBriefDirective inserts a kind='focus' wms_intervals row with
// identity_source='brief_directive', exactly as the hub's
// writeBriefDirectiveInterval writes when the remote scraper ships a focus-less
// teammate's dispatch-brief directive. Open-ended (no ended_at): the directive
// is the teammate's first instruction, so it covers the whole session.
func seedBriefDirective(t *testing.T, db store.Store, ctx context.Context, session, agent, etype, eid string, start time.Time) {
	t.Helper()
	storetest.Exec(t, ctx, db,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at)
		 VALUES ('focus','brief_directive',?,?,?,?,?)`,
		session, agent, etype, eid, start)
}

// markSweepSkipped flips a message's attribution method to 'sweep_skipped',
// emulating the LLM sweep having examined a focus-less remote session and given
// up (it could not read the Mac transcript). RecoverDirective must reclaim it.
func markSweepSkipped(t *testing.T, db store.Store, ctx context.Context, msgID string) {
	t.Helper()
	storetest.Exec(t, ctx, db, `UPDATE usage_attribution SET method='sweep_skipped' WHERE message_id=?`, msgID)
}

// TestRecoverDirective_AttributesFocusLessTeammate is the primary test: a
// focus-less remote teammate session — one unallocated message and one already
// marked sweep_skipped — is attributed to the workunit its brief named.
func TestRecoverDirective_AttributesFocusLessTeammate(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-sieve")
	seedWorkunit(t, db, ctx, "wu-review", "o-sieve")

	// A focus-less @PizzaHut session on a remote Mac: 3 messages, no real focus.
	seedLedgerHostUser(t, db, ctx, "ph1", "s-ph", "@PizzaHut", "studio", "alice", base.Add(28*time.Second), 0.06, 600)
	seedLedgerHostUser(t, db, ctx, "ph2", "s-ph", "@PizzaHut", "studio", "alice", base.Add(51*time.Second), 0.19, 1900)
	seedLedgerHostUser(t, db, ctx, "ph3", "s-ph", "@PizzaHut", "studio", "alice", base.Add(92*time.Second), 0.07, 700)

	// The brief named workunit wu-review; the scraper shipped it as a directive
	// interval at the session's first message.
	seedBriefDirective(t, db, ctx, "s-ph", "@PizzaHut", "workunit", "wu-review", base.Add(28*time.Second))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Allocate must NOT have consumed the directive interval — all 3 stay
	// unallocated (the directive is excluded from focusAt/focusInSession).
	for _, m := range []string{"ph1", "ph2", "ph3"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s post-Run method=%q, want unallocated (directive must not feed Allocate)", m, method)
		}
	}
	// Emulate the LLM sweep having skipped one of them.
	markSweepSkipped(t, db, ctx, "ph3")

	stats, err := r.RecoverDirective(ctx, false)
	if err != nil {
		t.Fatalf("recover-directive: %v", err)
	}
	if stats.Sessions != 1 || stats.Recovered != 3 || stats.NoEntity != 0 {
		t.Fatalf("stats=%+v, want Sessions=1 Recovered=3 NoEntity=0", stats)
	}

	// All 3 (the 2 unallocated + the 1 sweep_skipped) → wu-review / directive method.
	for _, m := range []string{"ph1", "ph2", "ph3"} {
		et, ei, method := attributionOf(t, db, ctx, m)
		if et != "workunit" || ei != "wu-review" || method != directiveMethod {
			t.Fatalf("%s → (%q,%q) method=%q, want (workunit,wu-review) %s", m, et, ei, method, directiveMethod)
		}
	}

	// Evidence recorded for each.
	var evN int
	storetest.QueryRow(t, ctx, db, `SELECT COUNT(*) FROM directive_evidence`, nil, &evN)
	if evN != 3 {
		t.Fatalf("directive_evidence rows=%d, want 3", evN)
	}
}

// TestRecoverDirective_Reverses verifies UncoverDirective returns the messages to
// unallocated and deletes the evidence, while leaving the directive interval in
// place (durable provenance).
func TestRecoverDirective_Reverses(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-sieve")
	seedWorkunit(t, db, ctx, "wu-review", "o-sieve")
	seedLedgerHostUser(t, db, ctx, "ph1", "s-ph", "@PizzaHut", "studio", "alice", base.Add(28*time.Second), 0.06, 600)
	seedLedgerHostUser(t, db, ctx, "ph2", "s-ph", "@PizzaHut", "studio", "alice", base.Add(51*time.Second), 0.19, 1900)
	seedBriefDirective(t, db, ctx, "s-ph", "@PizzaHut", "workunit", "wu-review", base.Add(28*time.Second))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := r.RecoverDirective(ctx, false); err != nil {
		t.Fatalf("recover-directive: %v", err)
	}

	n, err := r.UncoverDirective(ctx)
	if err != nil {
		t.Fatalf("uncover-directive: %v", err)
	}
	if n != 2 {
		t.Fatalf("reverted=%d, want 2", n)
	}
	// UncoverDirective deletes the attribution rows; a follow-up Allocate re-derives
	// them as unallocated (the brief_directive interval is excluded from focusAt, so
	// Allocate finds no focus and the anti-join re-picks them). This mirrors the CLI:
	// --unrecover-directives runs before the normal Run pass.
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("re-run after uncover: %v", err)
	}
	for _, m := range []string{"ph1", "ph2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s post-uncover+reallocate method=%q, want unallocated", m, method)
		}
	}
	var evN int
	storetest.QueryRow(t, ctx, db, `SELECT COUNT(*) FROM directive_evidence`, nil, &evN)
	if evN != 0 {
		t.Fatalf("directive_evidence rows=%d after uncover, want 0", evN)
	}
	// The directive interval is durable provenance — left in place.
	var ivN int
	storetest.QueryRow(t, ctx, db,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND identity_source='brief_directive'`, nil, &ivN)
	if ivN != 1 {
		t.Fatalf("brief_directive intervals=%d after uncover, want 1 (provenance retained)", ivN)
	}
}

// TestRecoverDirective_LeavesRealFocusUntouched verifies a session that DID set a
// real focus (remote_scraper interval) is attributed by Allocate as temporal_join
// and is NEVER touched by directive recovery — the load-bearing "did set focus is
// unaffected" guarantee.
func TestRecoverDirective_LeavesRealFocusUntouched(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-sieve")
	seedWorkunit(t, db, ctx, "wu-build", "o-sieve")

	// Real-focus session: a remote_scraper focus interval covers the message.
	seedLedgerHostUser(t, db, ctx, "rf1", "s-real", "@PizzaDude", "studio", "alice", base.Add(60*time.Second), 0.10, 1000)
	seedRemoteFocus(t, db, ctx, "s-real", "@PizzaDude", "workunit", "wu-build", base.Add(30*time.Second))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Allocate attributes it via the real focus interval.
	if et, ei, method := attributionOf(t, db, ctx, "rf1"); et != "workunit" || ei != "wu-build" || method != "temporal_join" {
		t.Fatalf("rf1 post-Run → (%q,%q) %q, want (workunit,wu-build) temporal_join", et, ei, method)
	}

	stats, err := r.RecoverDirective(ctx, false)
	if err != nil {
		t.Fatalf("recover-directive: %v", err)
	}
	// No brief_directive interval exists, so nothing to recover; real attribution
	// is untouched.
	if stats.Recovered != 0 || stats.Sessions != 0 {
		t.Fatalf("stats=%+v, want zero (no directive sessions)", stats)
	}
	if et, ei, method := attributionOf(t, db, ctx, "rf1"); method != "temporal_join" {
		t.Fatalf("rf1 after directive pass → (%q,%q) %q, want temporal_join unchanged", et, ei, method)
	}
}

// TestRecoverDirective_DanglingEntitySkipped verifies a directive naming an entity
// that does not exist is skipped (NoEntity) and leaves the cost unallocated rather
// than inventing attribution to a ghost.
func TestRecoverDirective_DanglingEntitySkipped(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedLedgerHostUser(t, db, ctx, "gh1", "s-ghost", "@Ghost", "studio", "alice", base.Add(28*time.Second), 0.06, 600)
	seedBriefDirective(t, db, ctx, "s-ghost", "@Ghost", "workunit", "wu-does-not-exist", base.Add(28*time.Second))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	stats, err := r.RecoverDirective(ctx, false)
	if err != nil {
		t.Fatalf("recover-directive: %v", err)
	}
	if stats.NoEntity != 1 || stats.Recovered != 0 || stats.Sessions != 0 {
		t.Fatalf("stats=%+v, want NoEntity=1 Recovered=0 Sessions=0", stats)
	}
	if _, _, method := attributionOf(t, db, ctx, "gh1"); method != "unallocated" {
		t.Fatalf("gh1 method=%q, want unallocated (dangling entity not invented)", method)
	}
}

// TestRecoverDirective_DryRunWritesNothing verifies --dry-run performs zero writes.
func TestRecoverDirective_DryRunWritesNothing(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-sieve")
	seedWorkunit(t, db, ctx, "wu-review", "o-sieve")
	seedLedgerHostUser(t, db, ctx, "ph1", "s-ph", "@PizzaHut", "studio", "alice", base.Add(28*time.Second), 0.06, 600)
	seedBriefDirective(t, db, ctx, "s-ph", "@PizzaHut", "workunit", "wu-review", base.Add(28*time.Second))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	stats, err := r.RecoverDirective(ctx, true)
	if err != nil {
		t.Fatalf("recover-directive dry-run: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("dry-run Recovered=%d, want 1 (counts only)", stats.Recovered)
	}
	if _, _, method := attributionOf(t, db, ctx, "ph1"); method != "unallocated" {
		t.Fatalf("ph1 method=%q after dry-run, want unallocated (no writes)", method)
	}
	var evN int
	storetest.QueryRow(t, ctx, db, `SELECT COUNT(*) FROM directive_evidence`, nil, &evN)
	if evN != 0 {
		t.Fatalf("dry-run wrote %d evidence rows, want 0", evN)
	}
}

// TestRecoverDirective_Conservation verifies SUM(ledger) == SUM(cost_facts) before
// and after directive recovery — no cost created or destroyed.
func TestRecoverDirective_Conservation(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-sieve")
	seedWorkunit(t, db, ctx, "wu-review", "o-sieve")
	for i, off := range []time.Duration{28, 51, 92} {
		seedLedgerHostUser(t, db, ctx, msgN(i), "s-ph", "@PizzaHut", "studio", "alice",
			base.Add(off*time.Second), float64(i+1)*0.1, (i+1)*1000)
	}
	seedBriefDirective(t, db, ctx, "s-ph", "@PizzaHut", "workunit", "wu-review", base.Add(28*time.Second))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	ledgerBefore := sumLedger(t, db, ctx)
	factsBefore := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerBefore-factsBefore) > eps {
		t.Fatalf("pre-recovery conservation violated: %.6f vs %.6f", ledgerBefore, factsBefore)
	}

	if _, err := r.RecoverDirective(ctx, false); err != nil {
		t.Fatalf("recover-directive: %v", err)
	}

	ledgerAfter := sumLedger(t, db, ctx)
	factsAfter := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerAfter-factsAfter) > eps {
		t.Fatalf("post-recovery conservation violated: ledger=%.6f facts=%.6f", ledgerAfter, factsAfter)
	}
	if math.Abs(ledgerAfter-ledgerBefore) > eps {
		t.Fatalf("recovery changed ledger total: %.6f → %.6f", ledgerBefore, ledgerAfter)
	}
	assertNoDoubleAttribution(t, db, ctx)
}

func msgN(i int) string {
	return "cm" + string(rune('0'+i))
}
