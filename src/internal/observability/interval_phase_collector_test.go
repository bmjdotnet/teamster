package observability_test

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestIntervalPhaseCostCollector is T-B2.5: the @observ live-validation of the
// teamster_wms_cost_by_phase_usd gauge. It proves the collector:
//   - emits exactly one series per (entity_type, phase) group the SQL produces,
//   - reports the conserved SUM(cost_usd) for each group (the MySQL GROUP BY
//     does the work; Collect is thin),
//   - maps a NULL phase to phase="unclassified",
//   - skips intervals with NULL cost_usd (not yet assembled),
//   - emits no series for an empty/unassembled table,
//   - serves a stable value from its 30s cache within the TTL.
//
// It drives the collector through its public Collect path (what Prometheus
// scrapes). Skips when TEAMSTER_TEST_MYSQL_DSN is unset. Never touches the live
// DB.
func TestIntervalPhaseCostCollector(t *testing.T) {
	st := storetest.Open(t, "teamster_test_phasecost")
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Fresh schema after migrations: the v9 backfill seeds intervals from the
	// (empty) entity tables, so there are no rows with a non-NULL cost_usd.
	// An unassembled table ⇒ no series.
	if rows := readPhaseCost(t, st); len(rows) != 0 {
		t.Fatalf("empty/unassembled table: got %d series, want 0", len(rows))
	}

	// Seed assembled intervals. cost_usd non-NULL = "assembled". Two workunit
	// build intervals (must SUM to one series), one workunit test interval, one
	// outcome build interval (distinct entity_type ⇒ distinct series), one
	// workunit with a NULL phase (⇒ phase="unclassified"), and one workunit with
	// a NULL cost_usd (NOT yet assembled ⇒ must be excluded entirely).
	insertInterval(t, st, ctx, "workunit", "u1", base, "build", 1.500000)
	insertInterval(t, st, ctx, "workunit", "u2", base, "build", 2.250000)
	insertInterval(t, st, ctx, "workunit", "u3", base, "test", 0.750000)
	insertInterval(t, st, ctx, "outcome", "o1", base, "build", 4.000000)
	insertInterval(t, st, ctx, "workunit", "u4", base, "", 0.500000)   // NULL phase
	insertIntervalNoCost(t, st, ctx, "workunit", "u5", base, "review") // NULL cost ⇒ excluded

	want := map[string]float64{
		"workunit|build":        3.750000, // 1.5 + 2.25 summed in SQL
		"workunit|test":         0.750000,
		"outcome|build":         4.000000,
		"workunit|unclassified": 0.500000, // NULL phase collapses here
	}

	got := readPhaseCost(t, st)
	if len(got) != len(want) {
		t.Fatalf("series count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for key, w := range want {
		v, ok := got[key]
		if !ok {
			t.Fatalf("missing series %q (got %v)", key, got)
		}
		if !floatEq(v, w) {
			t.Fatalf("series %q: got %v, want %v", key, v, w)
		}
	}
	// The NULL-cost interval (u5/review) must NOT appear under any phase.
	if _, ok := got["workunit|review"]; ok {
		t.Fatalf("NULL cost_usd interval leaked into output: %v", got)
	}

	// Cache behavior: a SECOND read from the SAME collector within the 30s TTL
	// must return the cached snapshot even after we mutate the table underneath
	// it. We add a new group and confirm the cached read does NOT see it.
	c := observability.NewIntervalPhaseCostCollector(st)
	first := collectPhaseCost(t, c)
	insertInterval(t, st, ctx, "workunit", "u6", base, "design", 9.000000)
	second := collectPhaseCost(t, c)
	if len(second) != len(first) {
		t.Fatalf("cache leaked: second read saw %d series, want cached %d", len(second), len(first))
	}
	if _, ok := second["workunit|design"]; ok {
		t.Fatalf("cache did not hold: new 'design' group appeared within TTL: %v", second)
	}
	// A FRESH collector sees the new group (no stale cache) — proves the cache,
	// not the query, suppressed it.
	fresh := readPhaseCost(t, st)
	if v, ok := fresh["workunit|design"]; !ok || !floatEq(v, 9.0) {
		t.Fatalf("fresh collector missing new 'design' group: %v", fresh)
	}
}

// readPhaseCost drives a FRESH IntervalPhaseCostCollector through its public
// Collect path and returns a map of "entity_type|phase" → value. A fresh
// collector each call sidesteps the 30s cache so the value is current.
func readPhaseCost(t *testing.T, rep store.ReportingStore) map[string]float64 {
	t.Helper()
	return collectPhaseCost(t, observability.NewIntervalPhaseCostCollector(rep))
}

// collectPhaseCost reads one specific collector instance (used to exercise the
// cache across reads on the same instance).
func collectPhaseCost(t *testing.T, c *observability.IntervalPhaseCostCollector) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)

	out := make(map[string]float64)
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("metric write: %v", err)
		}
		if pb.Gauge == nil || pb.Gauge.Value == nil {
			t.Fatal("phase-cost metric is not a gauge with a value")
		}
		var etype, phase string
		for _, lp := range pb.Label {
			switch lp.GetName() {
			case "entity_type":
				etype = lp.GetValue()
			case "phase":
				phase = lp.GetValue()
			}
		}
		out[etype+"|"+phase] = pb.Gauge.GetValue()
	}
	return out
}

// insertInterval seeds an assembled (non-NULL cost_usd) kind='state' wms_intervals row.
// An empty phase argument is written as SQL NULL (the "not yet classified" case).
func insertInterval(t *testing.T, db store.Store, ctx context.Context, etype, eid string, started time.Time, phase string, cost float64) {
	t.Helper()
	var phaseArg any
	if phase != "" {
		phaseArg = phase
	}
	storetest.Exec(t, ctx, db,
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, phase, cost_usd)
		 VALUES ('state',?,?,?,?,?,?,?)`,
		etype, eid, "active", started, started.Add(time.Minute), phaseArg, cost)
}

// insertIntervalNoCost seeds a kind='state' interval whose cost_usd is NULL (not
// yet assembled by the rollup) — the collector must exclude it.
func insertIntervalNoCost(t *testing.T, db store.Store, ctx context.Context, etype, eid string, started time.Time, phase string) {
	t.Helper()
	storetest.Exec(t, ctx, db,
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, phase)
		 VALUES ('state',?,?,?,?,?,?)`,
		etype, eid, "active", started, started.Add(time.Minute), phase)
}

func floatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
