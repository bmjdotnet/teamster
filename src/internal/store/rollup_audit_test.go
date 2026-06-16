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

// TestRollupReallocateDedupAudit is the @auditor's adversarial verification of
// the dedup / double-count invariants the operator suspects. It stresses two
// paths the existing rollup tests do NOT cover:
//
//  1. Reallocate idempotency + scope: --reallocate clears ONLY method='unallocated'
//     rows, never a temporal_join row; running it repeatedly never changes the
//     conserved total, the row count, or produces a duplicate attribution row.
//  2. 2c recovery without double-count: after a message's agent identity is
//     rewritten (unknown -> real agent) AND a covering focus interval exists, a
//     --reallocate pass re-derives ONLY the previously-unallocated row to the
//     real entity — cost stays conserved, no row is double-counted.
//
// Skips when TEAMSTER_TEST_MYSQL_DSN is unset. Never touches the live DB.
func TestRollupReallocateDedupAudit(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}

	schema := fmt.Sprintf("teamster_test_realloc_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
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
	base := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	// @spine focuses outcome o1 for the whole window.
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		 VALUES ('focus','direct',?,?,?,?,?,?)`,
		"s1", "@spine", "outcome", "o1", base, nil)

	// m1: spine, covered by o1            -> temporal_join (outcome/o1)
	// m2: unknown agent                   -> unallocated
	// m3: unknown agent                   -> unallocated
	// m3 must stay genuinely unallocated through the 2c recovery scenario below
	// (it is the lone agentless dollar recovery never touches). It uses the
	// never-attributable "unknown" sentinel rather than the lead's "" so the P1a
	// lead-session fallback does not (correctly) attribute it to the session's
	// @spine outcome/o1 focus — mirrors rollup_test.go's legacy-1 seeding.
	insertLedger(t, db, ctx, "m1", "s1", "spine", base.Add(5*time.Minute), 10.0)
	insertLedger(t, db, ctx, "m2", "s1", "unknown", base.Add(6*time.Minute), 20.0)
	insertLedger(t, db, ctx, "m3", "s1", "unknown", base.Add(7*time.Minute), 5.0)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := rollup.New(db, nil, discard)

	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	const ledgerTotal = 35.0
	assertConserved(t, db, ctx, ledgerTotal)
	assertRowCount(t, db, ctx, "usage_attribution", 3)
	assertMethod(t, db, ctx, "m1", "temporal_join", "outcome", "o1")
	assertMethod(t, db, ctx, "m2", "unallocated", "", "")
	assertMethod(t, db, ctx, "m3", "unallocated", "", "")
	// Capture the computed_at of the attributed row so we can prove reallocate
	// never rewrites it.
	m1Before := computedAt(t, db, ctx, "m1")

	// ---- Invariant 1: repeated --reallocate is idempotent and scope-safe. ----
	for i := 0; i < 3; i++ {
		if err := r.Run(ctx, true); err != nil {
			t.Fatalf("reallocate run %d: %v", i, err)
		}
		assertConserved(t, db, ctx, ledgerTotal)
		assertRowCount(t, db, ctx, "usage_attribution", 3) // never duplicates
		assertMethod(t, db, ctx, "m1", "temporal_join", "outcome", "o1")
		assertMethod(t, db, ctx, "m2", "unallocated", "", "")
		assertMethod(t, db, ctx, "m3", "unallocated", "", "")
	}
	// The temporal_join row was never deleted/re-inserted: its computed_at is
	// unchanged across all three reallocate passes (Reallocate is scoped to
	// method='unallocated').
	if got := computedAt(t, db, ctx, "m1"); !got.Equal(m1Before) {
		t.Fatalf("temporal_join row m1 was rewritten by reallocate: computed_at %v -> %v (scope leak)", m1Before, got)
	}

	// ---- Invariant 2: 2c recovery — fix identity + interval, reallocate, no dup. ----
	// Simulate the 2c backfill: m2's agent identity is corrected from "unknown"
	// to "@spine", and @spine's o1 interval covers m2's timestamp. A reallocate
	// must move ONLY m2 (was unallocated) to outcome/o1; m3 (still agentless)
	// stays unallocated; m1 is untouched; cost stays exactly conserved.
	mustExec(t, db, ctx, `UPDATE token_ledger SET agent_name='spine' WHERE message_id='m2'`)

	if err := r.Run(ctx, true); err != nil {
		t.Fatalf("recovery reallocate: %v", err)
	}
	assertConserved(t, db, ctx, ledgerTotal)
	assertRowCount(t, db, ctx, "usage_attribution", 3) // still 3, no double-count
	assertMethod(t, db, ctx, "m1", "temporal_join", "outcome", "o1")
	assertMethod(t, db, ctx, "m2", "temporal_join", "outcome", "o1") // recovered
	assertMethod(t, db, ctx, "m3", "unallocated", "", "")

	// Outcome o1 now holds m1+m2 = 30.0; unallocated holds only m3 = 5.0.
	assertBucketCost(t, db, ctx, "outcome", "o1", 30.0)
	assertBucketCost(t, db, ctx, "", "", 5.0)

	// SUM(weight)=1 per message must still hold everywhere.
	var bad int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT message_id, SUM(weight) s FROM usage_attribution
		 GROUP BY message_id HAVING ABS(s-1.0) > 1e-6) x`).Scan(&bad); err != nil {
		t.Fatalf("weight invariant: %v", err)
	}
	if bad != 0 {
		t.Fatalf("weight invariant violated after recovery: %d messages", bad)
	}
}

// assertConserved checks cost_rollup total == token_ledger total == want — the
// conservation invariant that makes double-counting structurally detectable.
func assertConserved(t *testing.T, db *sql.DB, ctx context.Context, want float64) {
	t.Helper()
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
	if math.Abs(rollupTotal-want) > 1e-4 {
		t.Fatalf("conserved total=%.6f, want %.6f", rollupTotal, want)
	}
}

func assertRowCount(t *testing.T, db *sql.DB, ctx context.Context, table string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if n != want {
		t.Fatalf("%s row count = %d, want %d", table, n, want)
	}
}

func assertMethod(t *testing.T, db *sql.DB, ctx context.Context, messageID, wantMethod, wantType, wantID string) {
	t.Helper()
	var method, et, eid string
	var w float64
	if err := db.QueryRowContext(ctx,
		`SELECT method, entity_type, entity_id, weight FROM usage_attribution WHERE message_id=?`,
		messageID).Scan(&method, &et, &eid, &w); err != nil {
		t.Fatalf("query %s: %v", messageID, err)
	}
	if method != wantMethod || et != wantType || eid != wantID {
		t.Fatalf("%s: got (%q,%q,%q), want (%q,%q,%q)", messageID, method, et, eid, wantMethod, wantType, wantID)
	}
	if math.Abs(w-1.0) > 1e-6 {
		t.Fatalf("%s: weight %.6f, want 1.0", messageID, w)
	}
}

func assertBucketCost(t *testing.T, db *sql.DB, ctx context.Context, et, eid string, want float64) {
	t.Helper()
	var c float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup WHERE entity_type=? AND entity_id=?`,
		et, eid).Scan(&c); err != nil {
		t.Fatalf("bucket cost (%q,%q): %v", et, eid, err)
	}
	if math.Abs(c-want) > 1e-4 {
		t.Fatalf("bucket (%q,%q) cost=%.6f, want %.6f", et, eid, c, want)
	}
}

func computedAt(t *testing.T, db *sql.DB, ctx context.Context, messageID string) time.Time {
	t.Helper()
	var ts time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT computed_at FROM usage_attribution WHERE message_id=?`, messageID).Scan(&ts); err != nil {
		t.Fatalf("computed_at %s: %v", messageID, err)
	}
	return ts
}
