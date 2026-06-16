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
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

func TestMostSpecific(t *testing.T) {
	tests := []struct {
		name     string
		cands    []focusCandidate
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
			cands:    []focusCandidate{{"task", "t1"}},
			wantType: "task", wantID: "t1", wantOK: true,
		},
		{
			name: "task beats its goal and project",
			cands: []focusCandidate{
				{"project", "p1"}, {"goal", "g1"}, {"task", "t1"},
			},
			wantType: "task", wantID: "t1", wantOK: true,
		},
		{
			name: "workitem is most specific",
			cands: []focusCandidate{
				{"goal", "g1"}, {"workitem", "w1"}, {"task", "t1"},
			},
			wantType: "workitem", wantID: "w1", wantOK: true,
		},
		{
			name:   "unknown entity type ignored",
			cands:  []focusCandidate{{"squad", "s1"}},
			wantOK: false,
		},
		{
			name: "goal chosen over project when no task",
			cands: []focusCandidate{
				{"project", "p1"}, {"goal", "g1"},
			},
			wantType: "goal", wantID: "g1", wantOK: true,
		},
		{
			name:     "single v3 outcome attributes (not dropped)",
			cands:    []focusCandidate{{"outcome", "o1"}},
			wantType: "outcome", wantID: "o1", wantOK: true,
		},
		{
			name: "v3 workunit beats its outcome",
			cands: []focusCandidate{
				{"outcome", "o1"}, {"workunit", "w1"},
			},
			wantType: "workunit", wantID: "w1", wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			et, id, ok := mostSpecific(tc.cands)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (et != tc.wantType || id != tc.wantID) {
				t.Fatalf("got (%q,%q), want (%q,%q)", et, id, tc.wantType, tc.wantID)
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
	if et, _, ok := mostSpecific([]focusCandidate{{"task", "t1"}}); !ok || et != "task" {
		t.Fatalf("focused message must attribute to its entity")
	}
	// An unfocused message → unallocated, still exactly one row of weight 1.0.
	if _, _, ok := mostSpecific(nil); ok {
		t.Fatalf("unfocused message must fall to the unallocated bucket")
	}
}

// --- DB-backed interval-cost harness (B2-rollup) -----------------------------
//
// These tests exercise the interval-cost assembly against a throwaway schema and
// are SKIPPED when TEAMSTER_TEST_MYSQL_DSN is unset. Use the mysql:// URL form
// (mysql://root:test@127.0.0.1:13306/<schema>) — the tcp(...) form makes the
// store silently SKIP migrations the assembly depends on. The harness mirrors
// internal/store/rollup_integration_test.go; its helpers are duplicated here
// because that suite lives in package store_test (not importable).

var intervalSchemaCounter int64

// rollupTestDB opens a fresh, fully-migrated throwaway schema (so v19/v20 columns
// exist) and returns its *sql.DB. Skips when the DSN is unset/unreachable.
func rollupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}
	schema := fmt.Sprintf("teamster_test_ivl_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&intervalSchemaCounter, 1))
	if err := mysqlEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN, err := mysqlRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	st, err := mysql.New(schemaDSN) // runs all migrations on the fresh schema
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		_ = mysqlDropSchema(dsn, schema)
	})
	return st.DB()
}

func newTestRunner(db *sql.DB) *Runner {
	return New(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// seedFocus opens (and optionally closes) a focus interval for an agent.
func seedFocus(t *testing.T, db *sql.DB, ctx context.Context, session, agent, etype, eid string, start time.Time, end *time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO wms_intervals (kind, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus',?,?,?,?,?,?)`,
		session, agent, etype, eid, start, end); err != nil {
		t.Fatalf("seed focus %s/%s: %v", etype, eid, err)
	}
}

// seedEventRecord opens (and optionally closes) a state interval on an entity and
// returns its id (the cost-attribution target).
func seedEventRecord(t *testing.T, db *sql.DB, ctx context.Context, etype, eid, state, agent string, start time.Time, end *time.Time) uint64 {
	t.Helper()
	res, err := db.ExecContext(ctx,
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, agent_name)
		 VALUES ('state',?,?,?,?,?,?)`,
		etype, eid, state, start, end, agent)
	if err != nil {
		t.Fatalf("seed event_record %s/%s: %v", etype, eid, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("event_record last id: %v", err)
	}
	return uint64(id)
}

func seedLedger(t *testing.T, db *sql.DB, ctx context.Context, msgID, session, agent string, ts time.Time, cost float64, tokens int) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO token_ledger
			(session_id, message_id, agent_name, host, model, total_input, cost_usd, timestamp)
		 VALUES (?,?,?,?,?,?,?,?)`,
		session, msgID, agent, "testhost", "claude-opus-4-8", tokens, cost, ts); err != nil {
		t.Fatalf("seed ledger %s: %v", msgID, err)
	}
}

// intervalCost reads the assembled cost on one wms_intervals (kind='state') row;
// ok=false when its cost_usd is NULL (not assembled / cleared).
func intervalCost(t *testing.T, db *sql.DB, ctx context.Context, id uint64) (cost float64, ok bool) {
	t.Helper()
	var c sql.NullFloat64
	if err := db.QueryRowContext(ctx,
		`SELECT cost_usd FROM wms_intervals WHERE id=?`, id).Scan(&c); err != nil {
		t.Fatalf("read interval cost %d: %v", id, err)
	}
	return c.Float64, c.Valid
}

// sumIntervalCost is the left-hand side of the conservation invariant.
func sumIntervalCost(t *testing.T, db *sql.DB, ctx context.Context) float64 {
	t.Helper()
	var s float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM wms_intervals WHERE kind='state' AND cost_usd IS NOT NULL`).Scan(&s); err != nil {
		t.Fatalf("sum interval cost: %v", err)
	}
	return s
}

// sumLedgerForAttributedIntervals is the right-hand side: Σ weight·cost over the
// attribution rows that DID land on an interval (interval_id <> 0).
func sumLedgerForAttributedIntervals(t *testing.T, db *sql.DB, ctx context.Context) float64 {
	t.Helper()
	var s float64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(t.cost_usd * ua.weight),0)
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.interval_id <> 0`).Scan(&s); err != nil {
		t.Fatalf("sum ledger for attributed intervals: %v", err)
	}
	return s
}

func sumRollup(t *testing.T, db *sql.DB, ctx context.Context) float64 {
	t.Helper()
	var s float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup`).Scan(&s); err != nil {
		t.Fatalf("sum cost_rollup: %v", err)
	}
	return s
}

func intervalIDOf(t *testing.T, db *sql.DB, ctx context.Context, msgID string) uint64 {
	t.Helper()
	var id uint64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id=?`, msgID).Scan(&id); err != nil {
		t.Fatalf("read interval_id %s: %v", msgID, err)
	}
	return id
}

// attributionOf reads the resolved entity + method for a message's
// usage_attribution row.
func attributionOf(t *testing.T, db *sql.DB, ctx context.Context, msgID string) (etype, eid, method string) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id, method FROM usage_attribution WHERE message_id=?`, msgID).
		Scan(&etype, &eid, &method); err != nil {
		t.Fatalf("read attribution %s: %v", msgID, err)
	}
	return etype, eid, method
}

const eps = 1e-4

// TestAssembleIntervalCost_Conservation is T-B2.2: after a full Run, the sum of
// assembled interval cost equals the ledger weight·cost over interval-attributed
// messages, AND cost_rollup's entity total is unchanged (no regression to the
// existing per-entity dashboards).
func TestAssembleIntervalCost_Conservation(t *testing.T) {
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
	if _, err := db.ExecContext(ctx, `DELETE FROM usage_attribution WHERE message_id IN ('m1','m2')`); err != nil {
		t.Fatalf("delete attribution: %v", err)
	}
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
	db := rollupTestDB(t)
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
	if _, err := db.ExecContext(ctx,
		`UPDATE wms_intervals SET phase='build',  phase_source='declared' WHERE id=?`, ivlBuild); err != nil {
		t.Fatalf("set build phase: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE wms_intervals SET phase='review', phase_source='declared' WHERE id=?`, ivlReview); err != nil {
		t.Fatalf("set review phase: %v", err)
	}

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
	rows, err := db.QueryContext(ctx,
		`SELECT phase, SUM(cost_usd) FROM wms_intervals WHERE kind='state' AND cost_usd IS NOT NULL GROUP BY phase`)
	if err != nil {
		t.Fatalf("cost-by-phase query: %v", err)
	}
	for rows.Next() {
		var phase string
		var c float64
		if err := rows.Scan(&phase, &c); err != nil {
			rows.Close() //nolint:errcheck
			t.Fatalf("scan phase row: %v", err)
		}
		phaseCost[phase] = c
	}
	rows.Close() //nolint:errcheck
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
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
func sumLedger(t *testing.T, db *sql.DB, ctx context.Context) float64 {
	t.Helper()
	var s float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM token_ledger`).Scan(&s); err != nil {
		t.Fatalf("sum token_ledger: %v", err)
	}
	return s
}

func sumCostFacts(t *testing.T, db *sql.DB, ctx context.Context) float64 {
	t.Helper()
	var s float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_facts`).Scan(&s); err != nil {
		t.Fatalf("sum cost_facts: %v", err)
	}
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
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
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
	in := []focusCandidate{
		{"workunit", "w1"}, {"outcome", "o1"}, {"task", "t1"}, {"goal", "g1"}, {"project", "p1"},
	}
	out := strategicCandidates(in)
	// Only outcome/goal/project survive.
	if len(out) != 3 {
		t.Fatalf("strategicCandidates kept %d, want 3 (outcome/goal/project)", len(out))
	}
	for _, c := range out {
		switch c.entityType {
		case "outcome", "goal", "project":
		default:
			t.Fatalf("strategicCandidates kept non-strategic %q", c.entityType)
		}
	}
	// mostSpecific over the strategic set prefers outcome (rank 2) over project (1).
	if et, _, ok := mostSpecific(out); !ok || et != "outcome" {
		t.Fatalf("mostSpecific(strategic) = (%q, ok=%v), want outcome", et, ok)
	}
	// Empty when no strategic entity is present.
	if got := strategicCandidates([]focusCandidate{{"workunit", "w1"}, {"task", "t1"}}); got != nil {
		t.Fatalf("strategicCandidates over leaves = %v, want nil", got)
	}
}

// TestAllocate_UnknownAgentUnallocated is the P1 elision-preserved test: the
// "unknown" sentinel (legacy backfill placeholder) never opens a focus interval,
// so it stays unallocated even when a lead focus covers ts — isAttributable still
// short-circuits it, preserving the ~13K-query elision.
func TestAllocate_UnknownAgentUnallocated(t *testing.T) {
	db := rollupTestDB(t)
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
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Seed a ledger entry so the session appears in the ledger union.
	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(time.Minute), 5.0, 500)

	// First reconcile: Prometheus reports otel_cost_usd=10 for s1.
	r1 := New(db, &mockOTel{costs: map[string]float64{"s1": 10.0}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r1.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}
	var otel1 float64
	if err := db.QueryRowContext(ctx,
		`SELECT otel_cost_usd FROM session_reconciliation WHERE session_id='s1'`).Scan(&otel1); err != nil {
		t.Fatalf("read otel1: %v", err)
	}
	if math.Abs(otel1-10.0) > eps {
		t.Fatalf("after reconcile1 otel_cost_usd=%.4f, want 10.0", otel1)
	}

	// Second reconcile: Prometheus returns nothing for s1 (series aged out).
	// The previously recorded 10.0 must be preserved — not overwritten with 0.
	r2 := New(db, &mockOTel{costs: map[string]float64{}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	var otel2 float64
	if err := db.QueryRowContext(ctx,
		`SELECT otel_cost_usd FROM session_reconciliation WHERE session_id='s1'`).Scan(&otel2); err != nil {
		t.Fatalf("read otel2: %v", err)
	}
	if math.Abs(otel2-10.0) > eps {
		t.Fatalf("after reconcile2 (absent from Prometheus) otel_cost_usd=%.4f, want 10.0 (must not overwrite with 0)", otel2)
	}
}

// TestReconcile_LiveValueBeatsStale verifies the converse: when Prometheus
// returns a real (non-zero) value in a subsequent reconcile, it overwrites any
// previously recorded value — the GREATEST guard only protects against going
// from non-zero to zero, not from lower to higher.
func TestReconcile_LiveValueBeatsStale(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	seedLedger(t, db, ctx, "m1", "s1", "spine", base.Add(time.Minute), 5.0, 500)

	// First reconcile with 10.0.
	r1 := New(db, &mockOTel{costs: map[string]float64{"s1": 10.0}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r1.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}

	// Second reconcile with 15.0 (higher live reading).
	r2 := New(db, &mockOTel{costs: map[string]float64{"s1": 15.0}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	var otel2 float64
	if err := db.QueryRowContext(ctx,
		`SELECT otel_cost_usd FROM session_reconciliation WHERE session_id='s1'`).Scan(&otel2); err != nil {
		t.Fatalf("read otel2: %v", err)
	}
	if math.Abs(otel2-15.0) > eps {
		t.Fatalf("live value 15.0 did not update stored 10.0: otel_cost_usd=%.4f", otel2)
	}
}

// --- minimal mysql:// test harness (duplicated from store_test, not importable) ---

func mysqlReachable(dsn string) bool {
	rest := strings.TrimPrefix(dsn, "mysql://")
	if i := strings.Index(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	conn, err := net.DialTimeout("tcp", rest, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func mysqlEnsureSchema(dsn, schema string) error {
	db, err := mysqlConnect(dsn, "")
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func mysqlDropSchema(dsn, schema string) error {
	db, err := mysqlConnect(dsn, "")
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

// mysqlRebindSchema rewrites a mysql://...host[:port]/db?params URL to point at
// the supplied schema (or no database when schema is "").
func mysqlRebindSchema(dsn, schema string) (string, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return "", fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	hostport, pathAndQuery, _ := splitOn(hostpath, "/")
	_, query, _ := splitOn(pathAndQuery, "?")
	out := "mysql://" + creds + "@" + hostport + "/"
	if schema != "" {
		out += schema
	}
	if query != "" {
		out += "?" + query
	}
	return out, nil
}

func splitOn(s, sep string) (head, tail string, ok bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// mysqlConnect opens a raw management *sql.DB at the given schema (or server level
// when schema is ""), bypassing the migration path in mysql.New.
func mysqlConnect(dsn, schema string) (*sql.DB, error) {
	drvDSN, err := mysqlDriverDSN(dsn, schema)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return db, nil
}

// mysqlDriverDSN converts a mysql://user:pass@host:port/db?params URL into the
// go-sql-driver form, overriding the database with schema (empty = server level).
func mysqlDriverDSN(raw, schema string) (string, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		return "", fmt.Errorf("expected mysql:// DSN, got %q", raw)
	}
	rest := strings.TrimPrefix(raw, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return "", fmt.Errorf("mysql DSN missing '@': %q", raw)
	}
	user, pass, _ := splitOn(creds, ":")
	hostport, dbAndQuery, _ := splitOn(hostpath, "/")
	_, query, _ := splitOn(dbAndQuery, "?")
	// Override unconditionally: an empty schema means a server-level connection
	// (no database selected) so CREATE/DROP DATABASE can run before the per-test
	// schema exists. Keeping the original DSN's db name here targets a database
	// that doesn't exist yet → Error 1049. (Matches the store/mysql harness.)
	dbname := schema
	params := "parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27"
	if query != "" {
		params = query + "&" + params
	}
	drv := user
	if pass != "" {
		drv += ":" + pass
	}
	drv += "@tcp(" + hostport + ")/" + dbname + "?" + params
	return drv, nil
}
