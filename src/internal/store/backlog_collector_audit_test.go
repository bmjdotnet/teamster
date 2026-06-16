package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/rollup"
	"github.com/bmjdotnet/teamster/internal/store/mysql"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestBacklogCollectorLiveValidation is the @auditor's live-validation of the
// teamster_wms_unattributed_backlog gauge (@observ). It proves the gauge:
//   - reports the TRUE anti-join depth (token_ledger LEFT JOIN usage_attribution
//     WHERE ua.message_id IS NULL) — i.e. the exact count rollup.Allocate drains,
//   - ticks DOWN by exactly N after a rollup pass allocates N messages,
//   - reaches 0 when the ledger is fully attributed.
//
// It drives the collector through its public Collect path (what Prometheus
// scrapes), constructing a FRESH collector per read so the 30s cache never
// serves a stale value across the rollup pass.
//
// Skips when TEAMSTER_TEST_MYSQL_DSN is unset. Never touches the live DB.
func TestBacklogCollectorLiveValidation(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}

	schema := fmt.Sprintf("teamster_test_backlog_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
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

	// Fresh schema: empty ledger ⇒ backlog must read 0.
	if got := readBacklog(t, db); got != 0 {
		t.Fatalf("empty ledger: backlog = %v, want 0", got)
	}

	// Seed 5 ledger messages, none attributed yet. @spine focuses outcome o1 for
	// the whole window so 4 of them are attributable; m5 is agentless (still
	// drains — it just lands unallocated).
	mustExec(t, db, ctx,
		`INSERT INTO wms_intervals (kind, session_id, agent_name, entity_type, entity_id, started_at, ended_at, identity_source)
		 VALUES ('focus',?,?,?,?,?,?,'direct')`,
		"s1", "@spine", "outcome", "o1", base, nil)
	for i, m := range []string{"m1", "m2", "m3", "m4"} {
		insertLedger(t, db, ctx, m, "s1", "spine", base.Add(time.Duration(i+1)*time.Minute), 10.0)
	}
	insertLedger(t, db, ctx, "m5", "s1", "", base.Add(5*time.Minute), 5.0)

	// All 5 are unattributed ⇒ backlog == 5 (the true LEFT-JOIN depth).
	if got := readBacklog(t, db); got != 5 {
		t.Fatalf("after seeding 5 unattributed: backlog = %v, want 5", got)
	}

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := rollup.New(db, nil, discard)

	// One rollup pass allocates all 5 (4 temporal_join + 1 unallocated) ⇒ the
	// backlog drains to exactly 0. This is the invariant: a pass that allocates
	// N drops the backlog by N, because the collector shares the allocator's
	// anti-join.
	allocated, err := r.Allocate(ctx)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if allocated != 5 {
		t.Fatalf("allocate returned %d, want 5", allocated)
	}
	if got := readBacklog(t, db); got != 0 {
		t.Fatalf("after full rollup pass: backlog = %v, want 0 (gauge did not drain with the anti-join)", got)
	}

	// Add 2 more unattributed messages ⇒ backlog rises to exactly 2 (only the
	// new ones; the prior 5 keep their attribution rows). Proves the gauge counts
	// the live shortfall, not a cumulative total.
	insertLedger(t, db, ctx, "m6", "s1", "spine", base.Add(6*time.Minute), 10.0)
	insertLedger(t, db, ctx, "m7", "s1", "spine", base.Add(7*time.Minute), 10.0)
	if got := readBacklog(t, db); got != 2 {
		t.Fatalf("after adding 2 new unattributed: backlog = %v, want 2", got)
	}

	// Drain again ⇒ back to 0.
	if _, err := r.Allocate(ctx); err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if got := readBacklog(t, db); got != 0 {
		t.Fatalf("after second pass: backlog = %v, want 0", got)
	}
}

// readBacklog drives a FRESH BacklogCollector through its public Collect path
// (the exact mechanism Prometheus uses) and returns the emitted gauge value.
// A fresh collector each call sidesteps the 30s cache so the value is current.
func readBacklog(t *testing.T, db *sql.DB) float64 {
	t.Helper()
	c := observability.NewBacklogCollector(db)
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	close(ch)
	m, ok := <-ch
	if !ok {
		t.Fatal("backlog collector emitted no metric")
	}
	var pb dto.Metric
	if err := m.Write(&pb); err != nil {
		t.Fatalf("metric write: %v", err)
	}
	if pb.Gauge == nil || pb.Gauge.Value == nil {
		t.Fatal("backlog metric is not a gauge with a value")
	}
	return *pb.Gauge.Value
}
