package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// ref is a terse store.EntityRef constructor for test tables, keeping struct
// literals keyed (go vet composites) without a wall of field names.
func ref(entityType, entityID string) store.EntityRef {
	return store.EntityRef{EntityType: entityType, EntityID: entityID}
}

func TestMostSpecific(t *testing.T) {
	tests := []struct {
		name     string
		cands    []store.EntityRef
		wantType string
		wantID   string
		wantOK   bool
	}{
		{
			name:   "empty yields unallocated",
			cands:  nil,
			wantOK: false,
		},
		{
			name:     "single task",
			cands:    []store.EntityRef{ref("task", "t1")},
			wantType: "task", wantID: "t1", wantOK: true,
		},
		{
			name: "task beats its goal and project",
			cands: []store.EntityRef{
				ref("project", "p1"), ref("goal", "g1"), ref("task", "t1"),
			},
			wantType: "task", wantID: "t1", wantOK: true,
		},
		{
			name: "workitem is most specific",
			cands: []store.EntityRef{
				ref("goal", "g1"), ref("workitem", "w1"), ref("task", "t1"),
			},
			wantType: "workitem", wantID: "w1", wantOK: true,
		},
		{
			name:   "unknown entity type ignored",
			cands:  []store.EntityRef{ref("squad", "s1")},
			wantOK: false,
		},
		{
			name: "goal chosen over project when no task",
			cands: []store.EntityRef{
				ref("project", "p1"), ref("goal", "g1"),
			},
			wantType: "goal", wantID: "g1", wantOK: true,
		},
		{
			name:     "single v3 outcome attributes (not dropped)",
			cands:    []store.EntityRef{ref("outcome", "o1")},
			wantType: "outcome", wantID: "o1", wantOK: true,
		},
		{
			name: "v3 workunit beats its outcome",
			cands: []store.EntityRef{
				ref("outcome", "o1"), ref("workunit", "w1"),
			},
			wantType: "workunit", wantID: "w1", wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := mostSpecific(tc.cands)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (ref.EntityType != tc.wantType || ref.EntityID != tc.wantID) {
				t.Fatalf("got (%q,%q), want (%q,%q)", ref.EntityType, ref.EntityID, tc.wantType, tc.wantID)
			}
		})
	}
}

// TestWeightInvariant documents the structural guarantee: the allocator emits
// exactly one usage_attribution row per message with weight 1.0 — either the
// most-specific focused entity (temporal_join) or the unallocated bucket. There
// is no code path that emits a partial or summed-over-1 weight, so
// SUM(weight)=1 per message_id holds by construction.
func TestWeightInvariant(t *testing.T) {
	// A focused message → one attributed row, weight 1.0.
	if got, ok := mostSpecific([]store.EntityRef{ref("task", "t1")}); !ok || got.EntityType != "task" {
		t.Fatalf("focused message must attribute to its entity")
	}
	// An unfocused message → unallocated, still exactly one row of weight 1.0.
	if _, ok := mostSpecific(nil); ok {
		t.Fatalf("unfocused message must fall to the unallocated bucket")
	}
}

// --- DB-backed interval-cost harness (B2-rollup) -----------------------------
//
// These tests exercise the interval-cost assembly against a throwaway schema and
// are SKIPPED when TEAMSTER_TEST_MYSQL_DSN is unset. The schema/store harness
// lives in internal/store/storetest (shared with internal/store's own
// conformance suite); fixture shapes with no Store-method equivalent (exact
// historical timestamps, specific row ids) go through store.RawExecutor via
// storetest.Exec/QueryRow.

// rollupTestStore opens a fresh, fully-migrated throwaway schema (so v19/v20
// columns exist). Skips when the DSN is unset/unreachable.
func rollupTestStore(t *testing.T) store.Store {
	return storetest.Open(t, "teamster_test_ivl")
}

func newTestRunner(db store.Store) *Runner {
	return NewRunner(db, db, db, db, db, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// seedFocus opens (and optionally closes) a focus interval for an agent.
func seedFocus(t *testing.T, db store.Store, ctx context.Context, session, agent, etype, eid string, start time.Time, end *time.Time) {
	t.Helper()
	storetest.Exec(t, ctx, db,
		`INSERT INTO wms_intervals (kind, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus',?,?,?,?,?,?)`,
		session, agent, etype, eid, start, end)
}

// seedEventRecord opens (and optionally closes) a state interval on an entity and
// returns its id (the cost-attribution target).
func seedEventRecord(t *testing.T, db store.Store, ctx context.Context, etype, eid, state, agent string, start time.Time, end *time.Time) uint64 {
	t.Helper()
	res := storetest.Exec(t, ctx, db,
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, agent_name)
		 VALUES ('state',?,?,?,?,?,?)`,
		etype, eid, state, start, end, agent)
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("event_record last id: %v", err)
	}
	return uint64(id)
}

func seedLedger(t *testing.T, db store.Store, ctx context.Context, msgID, session, agent string, ts time.Time, cost float64, tokens int) {
	t.Helper()
	storetest.SeedLedger(t, ctx, db, store.TelemetryRow{
		SessionID:  session,
		MessageID:  msgID,
		AgentName:  agent,
		Host:       "testhost",
		Model:      "claude-opus-4-8",
		TotalInput: int64(tokens),
		CostUSD:    cost,
		Timestamp:  ts,
	})
}

// intervalCost reads the assembled cost on one wms_intervals (kind='state') row;
// ok=false when its cost_usd is NULL (not assembled / cleared).
func intervalCost(t *testing.T, db store.Store, ctx context.Context, id uint64) (cost float64, ok bool) {
	t.Helper()
	var c sql.NullFloat64
	storetest.QueryRow(t, ctx, db, `SELECT cost_usd FROM wms_intervals WHERE id=?`, []any{id}, &c)
	return c.Float64, c.Valid
}

// sumIntervalCost is the left-hand side of the conservation invariant.
func sumIntervalCost(t *testing.T, db store.Store, ctx context.Context) float64 {
	t.Helper()
	var s float64
	storetest.QueryRow(t, ctx, db,
		`SELECT COALESCE(SUM(cost_usd),0) FROM wms_intervals WHERE kind='state' AND cost_usd IS NOT NULL`, nil, &s)
	return s
}

// sumLedgerForAttributedIntervals is the right-hand side: Σ weight·cost over the
// attribution rows that DID land on an interval (interval_id <> 0).
func sumLedgerForAttributedIntervals(t *testing.T, db store.Store, ctx context.Context) float64 {
	t.Helper()
	var s float64
	storetest.QueryRow(t, ctx, db, `
		SELECT COALESCE(SUM(t.cost_usd * ua.weight),0)
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.interval_id <> 0`, nil, &s)
	return s
}

func sumRollup(t *testing.T, db store.Store, ctx context.Context) float64 {
	t.Helper()
	var s float64
	storetest.QueryRow(t, ctx, db, `SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup`, nil, &s)
	return s
}

func intervalIDOf(t *testing.T, db store.Store, ctx context.Context, msgID string) uint64 {
	t.Helper()
	var id uint64
	storetest.QueryRow(t, ctx, db, `SELECT interval_id FROM usage_attribution WHERE message_id=?`, []any{msgID}, &id)
	return id
}

// attributionOf reads the resolved entity + method for a message's
// usage_attribution row.
func attributionOf(t *testing.T, db store.Store, ctx context.Context, msgID string) (etype, eid, method string) {
	t.Helper()
	storetest.QueryRow(t, ctx, db,
		`SELECT entity_type, entity_id, method FROM usage_attribution WHERE message_id=?`, []any{msgID}, &etype, &eid, &method)
	return etype, eid, method
}

const eps = 1e-4

// TestAssembleIntervalCost_Conservation is T-B2.2: after a full Run, the sum of
// assembled interval cost equals the ledger weight·cost over interval-attributed
// messages, AND cost_rollup's entity total is unchanged (no regression to the
// existing per-entity dashboards).
func TestAssembleIntervalCost_Conservation(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// @spine focuses workunit w1 for the whole window; w1 has one open state
	// interval covering it. Three costed messages land on that interval; one
	// legacy message carries the never-attributable 'unknown' sentinel → it stays
	// unallocated, interval_id=0. (It must NOT be agent_name='' here: the P1a
	// lead-session fallback would attribute a lead message to the session's w1
	// focus, which is correct behavior but defeats this test's "one unallocated
	// message" intent — so we use the sentinel isAttributable short-circuits.)
	seedFocus(t, db, ctx, "s1", "@spine", "workunit", "w1", base, nil)
	ivl := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@spine", base, nil)

	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(1*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m2", "s1", "spine", base.Add(2*time.Minute), 20.0, 2000)
	seedLedger(t, db, ctx, "m3", "s1", "spine", base.Add(3*time.Minute), 30.0, 3000)
	seedLedger(t, db, ctx, "legacy-1", "s1", "unknown", base.Add(4*time.Minute), 5.0, 500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// All three costed messages map to the single interval; the legacy row stays 0.
	for _, m := range []string{"m1", "m2", "m3"} {
		if got := intervalIDOf(t, db, ctx, m); got != ivl {
			t.Fatalf("%s: interval_id=%d, want %d", m, got, ivl)
		}
	}
	if got := intervalIDOf(t, db, ctx, "legacy-1"); got != 0 {
		t.Fatalf("legacy-1: interval_id=%d, want 0 (unallocated)", got)
	}

	// Conservation LHS == RHS.
	lhs := sumIntervalCost(t, db, ctx)
	rhs := sumLedgerForAttributedIntervals(t, db, ctx)
	if math.Abs(lhs-rhs) > eps {
		t.Fatalf("conservation violated: Σ interval cost=%.6f, Σ ledger·weight=%.6f", lhs, rhs)
	}
	// The interval holds exactly its three messages' cost.
	if cost, ok := intervalCost(t, db, ctx, ivl); !ok || math.Abs(cost-60.0) > eps {
		t.Fatalf("interval cost=%.6f (ok=%v), want 60.0", cost, ok)
	}
	// cost_rollup entity total is unchanged: still the full ledger (65.0).
	if got := sumRollup(t, db, ctx); math.Abs(got-65.0) > eps {
		t.Fatalf("cost_rollup total=%.6f, want 65.0 (entity grain must not regress)", got)
	}
}

// TestAssembleIntervalCost_Idempotent is T-B2.3: a second identical Run produces
// identical interval cost, AND — the SB-3 case — when an attribution drops out of
// the source, a re-assemble clears the now-orphaned interval's stale cost back to
// NULL (true idempotency, not a partial UPDATE...JOIN that leaves it stale).
func TestAssembleIntervalCost_Idempotent(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	seedFocus(t, db, ctx, "s1", "@spine", "workunit", "w1", base, nil)
	ivl := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@spine", base, nil)
	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(1*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m2", "s1", "spine", base.Add(2*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run1: %v", err)
	}
	c1, ok := intervalCost(t, db, ctx, ivl)
	if !ok || math.Abs(c1-30.0) > eps {
		t.Fatalf("after run1 interval cost=%.6f (ok=%v), want 30.0", c1, ok)
	}

	// Second identical run: unchanged.
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run2: %v", err)
	}
	if c2, ok := intervalCost(t, db, ctx, ivl); !ok || math.Abs(c2-c1) > eps {
		t.Fatalf("re-run not idempotent: %.6f then %.6f", c1, c2)
	}

	// SB-3 source-changed case: remove m1/m2's attribution from the source so the
	// interval no longer matches, then re-assemble — its stale cost must clear to
	// NULL (a plain UPDATE...JOIN would leave the old 30.0 behind).
	storetest.Exec(t, ctx, db, `DELETE FROM usage_attribution WHERE message_id IN ('m1','m2')`)
	if _, err := r.AssembleIntervalCost(ctx); err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if cost, ok := intervalCost(t, db, ctx, ivl); ok {
		t.Fatalf("stale interval cost not cleared: got %.6f (want NULL)", cost)
	}
}

// TestAssembleIntervalCost_NoDoubleCountAcrossPhases is T-B2.4: a workunit with
// two state intervals in different phases — each interval holds ONLY its own
// messages' cost, and SUM(cost_usd) GROUP BY phase equals the partition, never
// the doubled total. This is the structural anti-fan-out guarantee.
func TestAssembleIntervalCost_NoDoubleCountAcrossPhases(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// w1 focus is open the whole window. Two NON-overlapping state intervals:
	//   ivlBuild  [base, base+10m)  phase=build
	//   ivlReview [base+10m, open)  phase=review
	seedFocus(t, db, ctx, "s1", "@spine", "workunit", "w1", base, nil)
	end := base.Add(10 * time.Minute)
	ivlBuild := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@spine", base, &end)
	ivlReview := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@spine", base.Add(10*time.Minute), nil)

	// Stamp the phase column (B1) directly so cost-by-phase can group on it.
	storetest.Exec(t, ctx, db, `UPDATE wms_intervals SET phase='build',  phase_source='declared' WHERE id=?`, ivlBuild)
	storetest.Exec(t, ctx, db, `UPDATE wms_intervals SET phase='review', phase_source='declared' WHERE id=?`, ivlReview)

	// mb1/mb2 fall in the build window; mr1 falls in the review window.
	seedLedger(t, db, ctx, "mb1", "s1", "spine", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "mb2", "s1", "spine", base.Add(5*time.Minute), 15.0, 1500)
	seedLedger(t, db, ctx, "mr1", "s1", "spine", base.Add(20*time.Minute), 40.0, 4000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Each message lands on the interval whose window covers its ts.
	if got := intervalIDOf(t, db, ctx, "mb1"); got != ivlBuild {
		t.Fatalf("mb1 → interval %d, want build %d", got, ivlBuild)
	}
	if got := intervalIDOf(t, db, ctx, "mb2"); got != ivlBuild {
		t.Fatalf("mb2 → interval %d, want build %d", got, ivlBuild)
	}
	if got := intervalIDOf(t, db, ctx, "mr1"); got != ivlReview {
		t.Fatalf("mr1 → interval %d, want review %d", got, ivlReview)
	}

	// Each interval holds only its own cost — no message counted twice.
	if c, ok := intervalCost(t, db, ctx, ivlBuild); !ok || math.Abs(c-25.0) > eps {
		t.Fatalf("build interval cost=%.6f (ok=%v), want 25.0", c, ok)
	}
	if c, ok := intervalCost(t, db, ctx, ivlReview); !ok || math.Abs(c-40.0) > eps {
		t.Fatalf("review interval cost=%.6f (ok=%v), want 40.0", c, ok)
	}

	// cost-by-phase GROUP BY equals the disjoint partition (25 / 40), total 65 —
	// never the doubled 130 a fan-out join would produce.
	phaseCost := map[string]float64{}
	storetest.Query(t, ctx, db,
		`SELECT phase, SUM(cost_usd) FROM wms_intervals WHERE kind='state' AND cost_usd IS NOT NULL GROUP BY phase`, nil,
		func(scan func(dest ...any) error) {
			var phase string
			var c float64
			if err := scan(&phase, &c); err != nil {
				t.Fatalf("scan phase row: %v", err)
			}
			phaseCost[phase] = c
		})
	if math.Abs(phaseCost["build"]-25.0) > eps {
		t.Fatalf("phase=build cost=%.6f, want 25.0", phaseCost["build"])
	}
	if math.Abs(phaseCost["review"]-40.0) > eps {
		t.Fatalf("phase=review cost=%.6f, want 40.0", phaseCost["review"])
	}
	total := phaseCost["build"] + phaseCost["review"]
	if math.Abs(total-65.0) > eps {
		t.Fatalf("cost-by-phase total=%.6f, want 65.0 (double-count would give 130)", total)
	}
}

// TestReassembleIntervals_Backfill is the SB-1 opt-in backfill test: the normal
// pass is forward-only, so an attribution row written before the interval existed
// keeps interval_id=0 and shows no phase cost. ReassembleIntervals re-resolves it
// and populates historical cost-by-phase. The default Run must NOT do this (it is
// forward-only by design).
func TestReassembleIntervals_Backfill(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// First pass: focus + ledger exist, but NO state interval yet. The message is
	// attributed to the entity (temporal_join) with interval_id=0.
	seedFocus(t, db, ctx, "s1", "@spine", "workunit", "w1", base, nil)
	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(1*time.Minute), 12.0, 1200)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if got := intervalIDOf(t, db, ctx, "m1"); got != 0 {
		t.Fatalf("m1 interval_id=%d before backfill, want 0", got)
	}

	// Now the state interval appears (covers m1's ts). A normal forward-only Run
	// does NOT touch the already-attributed m1, so it stays at 0 — proving the
	// default is forward-only.
	ivl := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@spine", base, nil)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run2: %v", err)
	}
	if got := intervalIDOf(t, db, ctx, "m1"); got != 0 {
		t.Fatalf("forward-only Run wrongly backfilled m1: interval_id=%d, want 0", got)
	}
	if _, ok := intervalCost(t, db, ctx, ivl); ok {
		t.Fatalf("interval has cost before opt-in backfill (should be NULL)")
	}

	// Opt-in backfill: re-resolve interval_id for historical rows + reassemble.
	updated, err := r.ReassembleIntervals(ctx)
	if err != nil {
		t.Fatalf("reassemble intervals: %v", err)
	}
	if updated != 1 {
		t.Fatalf("backfill updated %d rows, want 1", updated)
	}
	if got := intervalIDOf(t, db, ctx, "m1"); got != ivl {
		t.Fatalf("after backfill m1 interval_id=%d, want %d", got, ivl)
	}
	if c, ok := intervalCost(t, db, ctx, ivl); !ok || math.Abs(c-12.0) > eps {
		t.Fatalf("after backfill interval cost=%.6f (ok=%v), want 12.0", c, ok)
	}
	// Idempotent: a second backfill updates nothing more.
	updated2, err := r.ReassembleIntervals(ctx)
	if err != nil {
		t.Fatalf("reassemble intervals 2: %v", err)
	}
	if updated2 != 0 {
		t.Fatalf("second backfill updated %d rows, want 0 (idempotent)", updated2)
	}
}

// TestAllocate_LeadEmptyAgent is the P1 regression test (the B0-twin): a solo
// lead's message carries agent_name='' and the lead opens a ''-keyed focus
// interval. The message MUST attribute to that focused entity (method
// temporal_join), NOT the unallocated bucket.
//
// This path is the one the old isAttributable (`agentName != ""`) silently
// dropped: every pre-existing DB-backed test uses a NAMED agent (@spine) or an
// agentless legacy row with NO focus, so the empty-agent-WITH-focus path was
// vacuously green. Run against the pre-P1 source (restore the `!= ""` clause)
// and this fails — m1 lands unallocated; with P1 it attributes to w1.
func TestAllocate_LeadEmptyAgent(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// The solo lead ('') focuses workunit w1 and opens its state interval — exactly
	// what wms_setFocus + updateWorkUnitStatus do, with agent_name=''.
	seedFocus(t, db, ctx, "s1", "", "workunit", "w1", base, nil)
	ivl := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "", base, nil)
	// The lead's own cost message, agent_name='' (the canonical lead identity).
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(1*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	etype, eid, method := attributionOf(t, db, ctx, "m1")
	if method != "temporal_join" {
		t.Fatalf("lead message method=%q, want temporal_join (pre-P1 this was 'unallocated')", method)
	}
	if etype != "workunit" || eid != "w1" {
		t.Fatalf("lead message attributed to (%q,%q), want (workunit,w1)", etype, eid)
	}
	if got := intervalIDOf(t, db, ctx, "m1"); got != ivl {
		t.Fatalf("lead message interval_id=%d, want %d", got, ivl)
	}
}

// TestAllocate_SubagentLeadFallback is the P2 test: an ephemeral subagent
// (agent_name='@general-purpose') has NO focus interval of its own, but the lead
// ('') focus covers ts in the same session. The subagent message MUST inherit the
// lead's focused entity via the fallback (method temporal_join_lead_fallback)
// rather than dropping to unallocated.
func TestAllocate_SubagentLeadFallback(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Only the lead has a focus interval (on w1); the subagent has none.
	seedFocus(t, db, ctx, "s1", "", "workunit", "w1", base, nil)
	ivl := seedEventRecord(t, db, ctx, "workunit", "w1", "active", "", base, nil)
	// Subagent cost message — no own focus interval exists for it.
	seedLedger(t, db, ctx, "sub1", "s1", "@general-purpose", base.Add(1*time.Minute), 25.0, 2500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	etype, eid, method := attributionOf(t, db, ctx, "sub1")
	if method != "temporal_join_lead_fallback" {
		t.Fatalf("subagent message method=%q, want temporal_join_lead_fallback", method)
	}
	if etype != "workunit" || eid != "w1" {
		t.Fatalf("subagent message attributed to (%q,%q), want lead's (workunit,w1)", etype, eid)
	}
	// It still resolves the entity's covering interval for cost-by-phase.
	if got := intervalIDOf(t, db, ctx, "sub1"); got != ivl {
		t.Fatalf("subagent message interval_id=%d, want %d", got, ivl)
	}
}

// TestAllocate_TeammateOwnFocusNoFallback is the P2 no-regression test: a named
// teammate that set its OWN focus must attribute to its own entity, never the
// lead's. It has a covering interval, so it takes the direct temporal_join branch
// and never reaches the fallback — even though a lead focus also covers ts.
func TestAllocate_TeammateOwnFocusNoFallback(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Lead focuses outcome o1; teammate @store focuses its own workunit w2. Both
	// intervals cover ts. The teammate's message must land on w2, not o1.
	seedFocus(t, db, ctx, "s1", "", "outcome", "o1", base, nil)
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w2", base, nil)
	seedEventRecord(t, db, ctx, "outcome", "o1", "active", "", base, nil)
	ivlW2 := seedEventRecord(t, db, ctx, "workunit", "w2", "active", "@store", base, nil)
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(1*time.Minute), 30.0, 3000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	etype, eid, method := attributionOf(t, db, ctx, "tm1")
	if method != "temporal_join" {
		t.Fatalf("teammate message method=%q, want temporal_join (own focus, no fallback)", method)
	}
	if etype != "workunit" || eid != "w2" {
		t.Fatalf("teammate message attributed to (%q,%q), want its own (workunit,w2) — fallback would have given outcome/o1", etype, eid)
	}
	if got := intervalIDOf(t, db, ctx, "tm1"); got != ivlW2 {
		t.Fatalf("teammate message interval_id=%d, want %d", got, ivlW2)
	}
}

// sumLedger / sumCostFacts are the two sides of the conservation invariant
// SUM(token_ledger.cost_usd) == SUM(cost_facts.cost_usd). Recovery and the
// lead-session fallback only move a dollar from the unallocated entity to a real
// one, so this delta must stay $0.00 across any allocation change.
func sumLedger(t *testing.T, db store.Store, ctx context.Context) float64 {
	t.Helper()
	var s float64
	storetest.QueryRow(t, ctx, db, `SELECT COALESCE(SUM(cost_usd),0) FROM token_ledger`, nil, &s)
	return s
}

func sumCostFacts(t *testing.T, db store.Store, ctx context.Context) float64 {
	t.Helper()
	var s float64
	storetest.QueryRow(t, ctx, db, `SELECT COALESCE(SUM(cost_usd),0) FROM cost_facts`, nil, &s)
	return s
}

// TestAllocate_LeadSessionFallback_PrefersOutcome is the P1a test: the LEAD
// (agent_name='') has NO focus interval of its own, but the session has focus on
// an Outcome (held by the lead earlier / nominally) AND a teammate's narrow
// WorkUnit, both covering ts. The lead's message must attribute to the OUTCOME
// (strategic tier, §7.3 "prefer Outcome"), via method
// temporal_join_lead_session_fallback — NOT to the teammate's WorkUnit, and NOT
// to the unallocated bucket. Conservation (ledger == cost_facts) must hold.
func TestAllocate_LeadSessionFallback_PrefersOutcome(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// The session is focused on outcome o1 (under teammate @planner) and on a
	// narrow workunit w1 (under teammate @store). The LEAD ('') has NO focus
	// interval of its own — exactly the dominant unallocated bucket.
	seedFocus(t, db, ctx, "s1", "@planner", "outcome", "o1", base, nil)
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base, nil)
	// The lead's coordination message, agent_name='' — no covering interval for it.
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(1*time.Minute), 10.0, 1000)
	// A teammate message that DID hold its own focus (regression guard: it must
	// NOT be affected by the lead-session fallback).
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(2*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	etype, eid, method := attributionOf(t, db, ctx, "lead1")
	if method != "temporal_join_lead_session_fallback" {
		t.Fatalf("lead message method=%q, want temporal_join_lead_session_fallback (pre-P1a this was 'unallocated')", method)
	}
	if etype != "outcome" || eid != "o1" {
		t.Fatalf("lead message attributed to (%q,%q), want strategic (outcome,o1) — a child WU would be a mis-attribution", etype, eid)
	}

	// Regression: the teammate's own-focus message still attributes to its own WU.
	tmEtype, tmEID, tmMethod := attributionOf(t, db, ctx, "tm1")
	if tmMethod != "temporal_join" || tmEtype != "workunit" || tmEID != "w1" {
		t.Fatalf("teammate message attributed to (%q,%q) method=%q, want (workunit,w1) temporal_join", tmEtype, tmEID, tmMethod)
	}

	// Conservation: every dollar still accounted for, just moved off ''.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestAllocate_LeadSessionFallback_NoOutcomeFallsToWorkUnit is the P1a
// secondary path: when NO strategic-tier interval (outcome/goal/project) covers
// ts, the lead-session fallback uses the most-specific covering entity of any
// type — here a teammate's WorkUnit — rather than leaving the lead message
// unallocated. Still better than the unallocated bucket; conservation holds.
func TestAllocate_LeadSessionFallback_NoOutcomeFallsToWorkUnit(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Only a teammate WorkUnit holds focus in the session — no Outcome at all.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base, nil)
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(1*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	etype, eid, method := attributionOf(t, db, ctx, "lead1")
	if method != "temporal_join_lead_session_fallback" {
		t.Fatalf("lead message method=%q, want temporal_join_lead_session_fallback", method)
	}
	if etype != "workunit" || eid != "w1" {
		t.Fatalf("lead message attributed to (%q,%q), want (workunit,w1) — the only covering entity", etype, eid)
	}
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestAllocate_LeadOwnFocusNoSessionFallback is the P1a no-regression test: when
// the LEAD DID open its own focus interval, it must take the direct
// temporal_join branch and NEVER reach the session fallback — even though other
// agents' intervals also cover ts. (The session fallback would prefer a
// different entity; the lead's own declared focus must win.)
func TestAllocate_LeadOwnFocusNoSessionFallback(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// The lead focuses outcome o1 itself; a teammate focuses workunit w1. The
	// lead's message must land on o1 via temporal_join (its OWN focus), not via
	// the session fallback.
	seedFocus(t, db, ctx, "s1", "", "outcome", "o1", base, nil)
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base, nil)
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(1*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	etype, eid, method := attributionOf(t, db, ctx, "lead1")
	if method != "temporal_join" {
		t.Fatalf("lead message method=%q, want temporal_join (lead has own focus)", method)
	}
	if etype != "outcome" || eid != "o1" {
		t.Fatalf("lead message attributed to (%q,%q), want its own (outcome,o1)", etype, eid)
	}
}

// TestAllocate_LeadNoSessionFocusStaysUnallocated is the P1a floor test: when the
// session has NO focus interval at all (no agent ever set focus), the lead's
// message has nothing to attribute to and correctly stays unallocated.
func TestAllocate_LeadNoSessionFocusStaysUnallocated(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// No focus intervals seeded at all.
	seedLedger(t, db, ctx, "lead1", "s1", "", base.Add(1*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, _, method := attributionOf(t, db, ctx, "lead1")
	if method != "unallocated" {
		t.Fatalf("lead message method=%q, want unallocated (session never focused)", method)
	}
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestStrategicCandidates is a pure-function unit test (no DB) for the
// prefer-strategic-tier filter used by the lead-session fallback.
func TestStrategicCandidates(t *testing.T) {
	in := []store.EntityRef{
		ref("workunit", "w1"), ref("outcome", "o1"), ref("task", "t1"), ref("goal", "g1"), ref("project", "p1"),
	}
	out := strategicCandidates(in)
	// Only outcome/goal/project survive.
	if len(out) != 3 {
		t.Fatalf("strategicCandidates kept %d, want 3 (outcome/goal/project)", len(out))
	}
	for _, c := range out {
		switch c.EntityType {
		case "outcome", "goal", "project":
		default:
			t.Fatalf("strategicCandidates kept non-strategic %q", c.EntityType)
		}
	}
	// mostSpecific over the strategic set prefers outcome (rank 2) over project (1).
	if ref, ok := mostSpecific(out); !ok || ref.EntityType != "outcome" {
		t.Fatalf("mostSpecific(strategic) = (%q, ok=%v), want outcome", ref.EntityType, ok)
	}
	// Empty when no strategic entity is present.
	if got := strategicCandidates([]store.EntityRef{ref("workunit", "w1"), ref("task", "t1")}); got != nil {
		t.Fatalf("strategicCandidates over leaves = %v, want nil", got)
	}
}

// TestAllocate_UnknownAgentUnallocated is the P1 elision-preserved test: the
// "unknown" sentinel (legacy backfill placeholder) never opens a focus interval,
// so it stays unallocated even when a lead focus covers ts — isAttributable still
// short-circuits it, preserving the ~13K-query elision.
func TestAllocate_UnknownAgentUnallocated(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// A lead focus exists, but the message's agent is the "unknown" sentinel.
	seedFocus(t, db, ctx, "s1", "", "workunit", "w1", base, nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "", base, nil)
	seedLedger(t, db, ctx, "u1", "s1", "unknown", base.Add(1*time.Minute), 7.0, 700)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, _, method := attributionOf(t, db, ctx, "u1")
	if method != "unallocated" {
		t.Fatalf("unknown-agent message method=%q, want unallocated (elision must be preserved)", method)
	}
	if got := intervalIDOf(t, db, ctx, "u1"); got != 0 {
		t.Fatalf("unknown-agent message interval_id=%d, want 0", got)
	}
}

// --- OTel / Reconcile tests ---------------------------------------------------

// mockOTel is a trivial OTelSource that returns a fixed cost map.
type mockOTel struct {
	costs map[string]float64
}

func (m *mockOTel) SessionCosts(_ context.Context) (map[string]float64, error) {
	return m.costs, nil
}

// TestSessionCosts_QueryUsesMaxOverTime verifies that SessionCosts issues a
// max_over_time range-aware query rather than a plain instant query. Without
// this, sessions whose OTel exporter has already exited appear absent from
// Prometheus (series age out of the ~5-min scrape window) and reconciliation
// records otel_cost_usd=0 for every completed session.
func TestSessionCosts_QueryUsesMaxOverTime(t *testing.T) {
	var capturedQuery string
	srv := &http.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query().Get("query")
		// Return a minimal valid Prometheus response with one result.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"session_id":"s1"},"value":[1,"1.23"]}]}}`)
	})
	srv.Handler = mux

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Close() })

	p := NewPromOTel("http://" + ln.Addr().String())
	costs, err := p.SessionCosts(context.Background())
	if err != nil {
		t.Fatalf("SessionCosts: %v", err)
	}
	if costs["s1"] != 1.23 {
		t.Fatalf("costs[s1] = %.2f, want 1.23", costs["s1"])
	}

	if !strings.Contains(capturedQuery, "max_over_time") {
		t.Fatalf("query %q does not use max_over_time", capturedQuery)
	}
	if !strings.Contains(capturedQuery, otelCostLookback) {
		t.Fatalf("query %q does not contain lookback %q", capturedQuery, otelCostLookback)
	}
}

// TestReconcile_NoOverwriteWithZero is the regression test for the staleness
// bug: a session that has a previously recorded non-zero otel_cost_usd must NOT
// have that value reset to 0 when the session is absent from the current
// Prometheus result (series aged out of retention). The GREATEST guard in the
// upsert must keep the last good reading.
func TestReconcile_NoOverwriteWithZero(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Seed a ledger entry so the session appears in the ledger union.
	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(time.Minute), 5.0, 500)

	// First reconcile: Prometheus reports otel_cost_usd=10 for s1.
	r1 := NewRunner(db, db, db, db, db, db, &mockOTel{costs: map[string]float64{"s1": 10.0}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r1.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}
	var otel1 float64
	storetest.QueryRow(t, ctx, db, `SELECT otel_cost_usd FROM session_reconciliation WHERE session_id='s1'`, nil, &otel1)
	if math.Abs(otel1-10.0) > eps {
		t.Fatalf("after reconcile1 otel_cost_usd=%.4f, want 10.0", otel1)
	}

	// Second reconcile: Prometheus returns nothing for s1 (series aged out).
	// The previously recorded 10.0 must be preserved — not overwritten with 0.
	r2 := NewRunner(db, db, db, db, db, db, &mockOTel{costs: map[string]float64{}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	var otel2 float64
	storetest.QueryRow(t, ctx, db, `SELECT otel_cost_usd FROM session_reconciliation WHERE session_id='s1'`, nil, &otel2)
	if math.Abs(otel2-10.0) > eps {
		t.Fatalf("after reconcile2 (absent from Prometheus) otel_cost_usd=%.4f, want 10.0 (must not overwrite with 0)", otel2)
	}
}

// TestReconcile_LiveValueBeatsStale verifies the converse: when Prometheus
// returns a real (non-zero) value in a subsequent reconcile, it overwrites any
// previously recorded value — the GREATEST guard only protects against going
// from non-zero to zero, not from lower to higher.
func TestReconcile_LiveValueBeatsStale(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(time.Minute), 5.0, 500)

	// First reconcile with 10.0.
	r1 := NewRunner(db, db, db, db, db, db, &mockOTel{costs: map[string]float64{"s1": 10.0}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r1.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}

	// Second reconcile with 15.0 (higher live reading).
	r2 := NewRunner(db, db, db, db, db, db, &mockOTel{costs: map[string]float64{"s1": 15.0}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	var otel2 float64
	storetest.QueryRow(t, ctx, db, `SELECT otel_cost_usd FROM session_reconciliation WHERE session_id='s1'`, nil, &otel2)
	if math.Abs(otel2-15.0) > eps {
		t.Fatalf("live value 15.0 did not update stored 10.0: otel_cost_usd=%.4f", otel2)
	}
}
