package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/rollup"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

// TestRollupAllocatorInvariants is a DB-backed verification of the attribution
// rollup against a throwaway schema (teamster_test_*). It seeds token_ledger +
// wms_intervals (kind='focus') by hand, runs the allocator + cost_rollup, and asserts:
//   - exactly one usage_attribution row per message (SUM(weight)=1),
//   - focused messages attribute to the most-specific entity,
//   - unfocused / agentless messages land in the unallocated bucket,
//   - cost_rollup is conserved against the ledger.
//
// Skips when TEAMSTER_TEST_MYSQL_DSN is unset, matching the store conformance
// suite. It never touches the live `teamster` database.
func TestRollupAllocatorInvariants(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}

	schema := fmt.Sprintf("teamster_test_rollup_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
	if err := mysqlEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN, err := mysqlRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	// mysql.New runs migrations, including v12 attribution-spine, on the fresh schema.
	st, err := mysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		_ = mysqlDropSchema(dsn, schema)
	})

	ctx := context.Background()
	db := st.DB()
	base := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	// Focus history: @spine focuses goal g1 at T+0 (closed at T+10m), then task
	// t1 at T+10m (still open).
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"sess1", "@spine", "goal", "g1", base, base.Add(10*time.Minute))
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"sess1", "@spine", "task", "t1", base.Add(10*time.Minute), nil)

	// Ledger rows — agent_name is the BARE form ("spine") the scraper-derived
	// token_ledger carries, while the focus intervals above use the
	// "@"-prefixed form ("@spine") the focus-writer emits. This is the EXACT
	// production mismatch that routed 100% of attribution to unallocated.
	// The allocator's focusAt must join @-prefix-insensitively, so these still
	// attribute. (Before the fix, m1/m2/m3 fall to unallocated and the entity
	// assertions below fail — this test is the regression guard.)
	//   m1: spine at T+5m  → covered by goal g1          → goal/g1
	//   m2: spine at T+15m → covered by task t1          → task/t1
	//   m3: spine at T+30m → task t1 still open          → task/t1
	//   legacy-1: "unknown" sentinel → unallocated (the genuinely-unattributable
	//             dollar that proves the unallocated bucket = 5.0). It uses the
	//             never-attributable "unknown" sentinel rather than the lead's ""
	//             so the P1a lead-session fallback does not (correctly) attribute
	//             it to this session's task/t1 focus — mirrors rollup_test.go.
	insertLedger(t, db, ctx, "m1", "sess1", "spine", base.Add(5*time.Minute), 10.0)
	insertLedger(t, db, ctx, "m2", "sess1", "spine", base.Add(15*time.Minute), 20.0)
	insertLedger(t, db, ctx, "m3", "sess1", "spine", base.Add(30*time.Minute), 30.0)
	insertLedger(t, db, ctx, "legacy-1", "sess1", "unknown", base.Add(40*time.Minute), 5.0)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := rollup.New(db, nil, discard) // nil OTel → reconciliation skipped
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("rollup run: %v", err)
	}

	// Invariant 1: exactly one usage_attribution row per message, SUM(weight)=1.
	var badMessages int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
			SELECT message_id, SUM(weight) s FROM usage_attribution GROUP BY message_id
			HAVING ABS(s - 1.0) > 1e-6
		) x`).Scan(&badMessages); err != nil {
		t.Fatalf("weight invariant query: %v", err)
	}
	if badMessages != 0 {
		t.Fatalf("weight invariant violated: %d messages with SUM(weight) != 1", badMessages)
	}

	// Invariant 2: correct entity assignment per message.
	assertAttribution(t, db, ctx, "m1", "goal", "g1", "temporal_join")
	assertAttribution(t, db, ctx, "m2", "task", "t1", "temporal_join")
	assertAttribution(t, db, ctx, "m3", "task", "t1", "temporal_join")
	assertAttribution(t, db, ctx, "legacy-1", "", "", "unallocated")

	// Invariant 3: conservation — cost_rollup total == ledger total == 65.0.
	var rollupTotal, ledgerTotal float64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup`).Scan(&rollupTotal); err != nil {
		t.Fatalf("rollup total: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0) FROM token_ledger`).Scan(&ledgerTotal); err != nil {
		t.Fatalf("ledger total: %v", err)
	}
	if math.Abs(rollupTotal-ledgerTotal) > 1e-4 {
		t.Fatalf("conservation violated: cost_rollup=%.6f ledger=%.6f", rollupTotal, ledgerTotal)
	}
	if math.Abs(ledgerTotal-65.0) > 1e-4 {
		t.Fatalf("expected ledger total 65.0, got %.6f", ledgerTotal)
	}

	// Invariant 4: the unallocated bucket holds exactly the legacy row's cost.
	var unalloc float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup WHERE entity_type='' AND entity_id=''`).Scan(&unalloc); err != nil {
		t.Fatalf("unallocated total: %v", err)
	}
	if math.Abs(unalloc-5.0) > 1e-4 {
		t.Fatalf("expected unallocated 5.0, got %.6f", unalloc)
	}

	// Idempotency: a second pass changes nothing (still 4 rows).
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("second rollup run: %v", err)
	}
	var ua int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_attribution`).Scan(&ua); err != nil {
		t.Fatalf("count ua: %v", err)
	}
	if ua != 4 {
		t.Fatalf("expected 4 usage_attribution rows after re-run, got %d", ua)
	}
}

// TestRollupAllocatorV3Attribution is the regression guard for the P2b core
// bug: the entity-specificity table was v1-only ({workitem,task,goal,project}),
// so the v3 attribution-spine types (outcome, workunit) that 2a writes into
// wms_intervals (kind='focus') ranked 0 in mostSpecific and were DROPPED — routing
// 100% of v3 messages to the unallocated bucket while the v1-only invariants
// test stayed green. This test seeds outcome/workunit intervals + ledger rows
// and asserts:
//   - a workunit-focused message attributes to that workunit (NOT unallocated),
//   - an outcome-focused message attributes to that outcome,
//   - when both an outcome and a workunit cover a timestamp, the workunit wins
//     (workunit is more specific, mirroring task > goal),
//   - the "unknown" agent guard routes straight to unallocated,
//   - SUM(weight)=1 per message and cost is conserved into cost_rollup,
//   - >0% of cost attributes to a real v3 entity (the headline proof: before
//     the fix this fraction is 0).
//
// Skips when TEAMSTER_TEST_MYSQL_DSN is unset.
func TestRollupAllocatorV3Attribution(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}

	schema := fmt.Sprintf("teamster_test_rollupv3_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
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

	ctx := context.Background()
	db := st.DB()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// v3 focus history for @spine in sess-v3:
	//   outcome o1 open T+0 .. (never closed) — the strategic umbrella.
	//   workunit w1 open T+10m .. T+20m — a bounded unit nested under o1.
	// During [T+10m, T+20m) BOTH o1 (outcome) and w1 (workunit) cover the
	// timestamp; the allocator must pick w1 (more specific).
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"sess-v3", "@spine", "outcome", "o1", base, nil)
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"sess-v3", "@spine", "workunit", "w1", base.Add(10*time.Minute), base.Add(20*time.Minute))

	// Ledger rows (bare agent name, @-prefix bridged by focusAt's TRIM):
	//   v1: spine at T+5m  → only outcome o1 covers       → outcome/o1
	//   v2: spine at T+15m → outcome o1 AND workunit w1   → workunit/w1 (more specific)
	//   v3: spine at T+25m → workunit closed, o1 still open → outcome/o1
	//   vu: agent "unknown" → unallocated (guard, never queries focusAt)
	insertLedger(t, db, ctx, "v1", "sess-v3", "spine", base.Add(5*time.Minute), 11.0)
	insertLedger(t, db, ctx, "v2", "sess-v3", "spine", base.Add(15*time.Minute), 22.0)
	insertLedger(t, db, ctx, "v3", "sess-v3", "spine", base.Add(25*time.Minute), 33.0)
	insertLedger(t, db, ctx, "vu", "sess-v3", "unknown", base.Add(30*time.Minute), 7.0)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := rollup.New(db, nil, discard)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("rollup run: %v", err)
	}

	// Core proof: v3 entity types attribute, with workunit beating outcome.
	assertAttribution(t, db, ctx, "v1", "outcome", "o1", "temporal_join")
	assertAttribution(t, db, ctx, "v2", "workunit", "w1", "temporal_join")
	assertAttribution(t, db, ctx, "v3", "outcome", "o1", "temporal_join")
	assertAttribution(t, db, ctx, "vu", "", "", "unallocated")

	// SUM(weight)=1 per message — no double-counting introduced by the v3 path.
	var badMessages int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
			SELECT message_id, SUM(weight) s FROM usage_attribution GROUP BY message_id
			HAVING ABS(s - 1.0) > 1e-6
		) x`).Scan(&badMessages); err != nil {
		t.Fatalf("weight invariant query: %v", err)
	}
	if badMessages != 0 {
		t.Fatalf("weight invariant violated: %d messages with SUM(weight) != 1", badMessages)
	}

	// Conservation: cost_rollup total == ledger total == 73.0.
	var rollupTotal, ledgerTotal float64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup`).Scan(&rollupTotal); err != nil {
		t.Fatalf("rollup total: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0) FROM token_ledger`).Scan(&ledgerTotal); err != nil {
		t.Fatalf("ledger total: %v", err)
	}
	if math.Abs(rollupTotal-ledgerTotal) > 1e-4 {
		t.Fatalf("conservation violated: cost_rollup=%.6f ledger=%.6f", rollupTotal, ledgerTotal)
	}
	if math.Abs(ledgerTotal-73.0) > 1e-4 {
		t.Fatalf("expected ledger total 73.0, got %.6f", ledgerTotal)
	}

	// Headline metric: the fraction of cost attributed to a real v3 entity must
	// be > 0. Before the specificity-map fix this is exactly 0 (everything
	// unallocated). After the fix it is 66/73 (v1+v2+v3 attributed, vu only
	// unallocated).
	var v3Attributed float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup
		 WHERE entity_type IN ('outcome','workunit')`).Scan(&v3Attributed); err != nil {
		t.Fatalf("v3 attributed total: %v", err)
	}
	if v3Attributed <= 0 {
		t.Fatalf("v3 attribution broken: 0 cost attributed to outcome/workunit (specificity map regressed)")
	}
	if math.Abs(v3Attributed-66.0) > 1e-4 {
		t.Fatalf("expected v3-attributed cost 66.0, got %.6f", v3Attributed)
	}

	// The unallocated bucket holds exactly the unknown-agent row's cost.
	var unalloc float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup WHERE entity_type='' AND entity_id=''`).Scan(&unalloc); err != nil {
		t.Fatalf("unallocated total: %v", err)
	}
	if math.Abs(unalloc-7.0) > 1e-4 {
		t.Fatalf("expected unallocated 7.0, got %.6f", unalloc)
	}

	// --reallocate path: clearing + re-deriving must be idempotent. The
	// unknown-agent row (vu) is the only unallocated one; after a reallocate
	// pass it returns to unallocated (no covering interval), every v3 row keeps
	// its attribution, totals are unchanged, and there are still 4 rows.
	if err := r.Run(ctx, true); err != nil {
		t.Fatalf("reallocate run: %v", err)
	}
	assertAttribution(t, db, ctx, "v1", "outcome", "o1", "temporal_join")
	assertAttribution(t, db, ctx, "v2", "workunit", "w1", "temporal_join")
	assertAttribution(t, db, ctx, "v3", "outcome", "o1", "temporal_join")
	assertAttribution(t, db, ctx, "vu", "", "", "unallocated")
	var ua int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_attribution`).Scan(&ua); err != nil {
		t.Fatalf("count ua: %v", err)
	}
	if ua != 4 {
		t.Fatalf("expected 4 usage_attribution rows after reallocate, got %d", ua)
	}

	// reallocate recovery: if vu's interval is created retroactively (the 2c
	// agent-identity recovery shape) and the agent is no longer "unknown",
	// a reallocate pass must re-derive vu out of the unallocated bucket. Here we
	// give @spine a workunit interval covering vu's timestamp and rewrite vu's
	// agent, then reallocate.
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"sess-v3", "@spine", "workunit", "w2", base.Add(28*time.Minute), nil)
	mustExec(t, db, ctx, `UPDATE token_ledger SET agent_name='spine' WHERE message_id='vu'`)
	if err := r.Run(ctx, true); err != nil {
		t.Fatalf("reallocate recovery run: %v", err)
	}
	assertAttribution(t, db, ctx, "vu", "workunit", "w2", "temporal_join")
	// Still exactly 4 attribution rows — recovery rewrote vu in place, no new rows.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_attribution`).Scan(&ua); err != nil {
		t.Fatalf("count ua after recovery: %v", err)
	}
	if ua != 4 {
		t.Fatalf("expected 4 usage_attribution rows after recovery, got %d", ua)
	}
}

func mustExec(t *testing.T, db *sql.DB, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func insertLedger(t *testing.T, db *sql.DB, ctx context.Context, messageID, sessionID, agent string, ts time.Time, cost float64) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO token_ledger
			(session_id, message_id, agent_name, host, model, total_input, cost_usd, timestamp)
		 VALUES (?,?,?,?,?,?,?,?)`,
		sessionID, messageID, agent, "testhost", "claude-opus-4-8", 1000, cost, ts)
	if err != nil {
		t.Fatalf("insert ledger %s: %v", messageID, err)
	}
}

func assertAttribution(t *testing.T, db *sql.DB, ctx context.Context, messageID, wantType, wantID, wantMethod string) {
	t.Helper()
	var et, eid, method string
	var w float64
	err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id, weight, method FROM usage_attribution WHERE message_id=?`,
		messageID).Scan(&et, &eid, &w, &method)
	if err != nil {
		t.Fatalf("query attribution %s: %v", messageID, err)
	}
	if et != wantType || eid != wantID || method != wantMethod {
		t.Fatalf("message %s: got (%q,%q,%q), want (%q,%q,%q)", messageID, et, eid, method, wantType, wantID, wantMethod)
	}
	if math.Abs(w-1.0) > 1e-6 {
		t.Fatalf("message %s: weight %.6f, want 1.0", messageID, w)
	}
}
