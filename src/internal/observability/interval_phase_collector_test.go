package observability_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store/mysql"

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
// DB. Uses the mysql:// DSN form — the tcp(...) form silently SKIPs.
func TestIntervalPhaseCostCollector(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}

	schema := fmt.Sprintf("teamster_test_phasecost_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&phaseCostSchemaCounter, 1))
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
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Fresh schema after migrations: the v9 backfill seeds intervals from the
	// (empty) entity tables, so there are no rows with a non-NULL cost_usd.
	// An unassembled table ⇒ no series.
	if rows := readPhaseCost(t, db); len(rows) != 0 {
		t.Fatalf("empty/unassembled table: got %d series, want 0", len(rows))
	}

	// Seed assembled intervals. cost_usd non-NULL = "assembled". Two workunit
	// build intervals (must SUM to one series), one workunit test interval, one
	// outcome build interval (distinct entity_type ⇒ distinct series), one
	// workunit with a NULL phase (⇒ phase="unclassified"), and one workunit with
	// a NULL cost_usd (NOT yet assembled ⇒ must be excluded entirely).
	insertInterval(t, db, ctx, "workunit", "u1", base, "build", 1.500000)
	insertInterval(t, db, ctx, "workunit", "u2", base, "build", 2.250000)
	insertInterval(t, db, ctx, "workunit", "u3", base, "test", 0.750000)
	insertInterval(t, db, ctx, "outcome", "o1", base, "build", 4.000000)
	insertInterval(t, db, ctx, "workunit", "u4", base, "", 0.500000)   // NULL phase
	insertIntervalNoCost(t, db, ctx, "workunit", "u5", base, "review") // NULL cost ⇒ excluded

	want := map[string]float64{
		"workunit|build":        3.750000, // 1.5 + 2.25 summed in SQL
		"workunit|test":         0.750000,
		"outcome|build":         4.000000,
		"workunit|unclassified": 0.500000, // NULL phase collapses here
	}

	got := readPhaseCost(t, db)
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
	c := observability.NewIntervalPhaseCostCollector(db)
	first := collectPhaseCost(t, c)
	insertInterval(t, db, ctx, "workunit", "u6", base, "design", 9.000000)
	second := collectPhaseCost(t, c)
	if len(second) != len(first) {
		t.Fatalf("cache leaked: second read saw %d series, want cached %d", len(second), len(first))
	}
	if _, ok := second["workunit|design"]; ok {
		t.Fatalf("cache did not hold: new 'design' group appeared within TTL: %v", second)
	}
	// A FRESH collector sees the new group (no stale cache) — proves the cache,
	// not the query, suppressed it.
	fresh := readPhaseCost(t, db)
	if v, ok := fresh["workunit|design"]; !ok || !floatEq(v, 9.0) {
		t.Fatalf("fresh collector missing new 'design' group: %v", fresh)
	}
}

// readPhaseCost drives a FRESH IntervalPhaseCostCollector through its public
// Collect path and returns a map of "entity_type|phase" → value. A fresh
// collector each call sidesteps the 30s cache so the value is current.
func readPhaseCost(t *testing.T, db *sql.DB) map[string]float64 {
	t.Helper()
	return collectPhaseCost(t, observability.NewIntervalPhaseCostCollector(db))
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
func insertInterval(t *testing.T, db *sql.DB, ctx context.Context, etype, eid string, started time.Time, phase string, cost float64) {
	t.Helper()
	var phaseArg any
	if phase != "" {
		phaseArg = phase
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, phase, cost_usd)
		 VALUES ('state',?,?,?,?,?,?,?)`,
		etype, eid, "active", started, started.Add(time.Minute), phaseArg, cost)
	if err != nil {
		t.Fatalf("insert interval %s/%s: %v", etype, eid, err)
	}
}

// insertIntervalNoCost seeds a kind='state' interval whose cost_usd is NULL (not
// yet assembled by the rollup) — the collector must exclude it.
func insertIntervalNoCost(t *testing.T, db *sql.DB, ctx context.Context, etype, eid string, started time.Time, phase string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, phase)
		 VALUES ('state',?,?,?,?,?,?)`,
		etype, eid, "active", started, started.Add(time.Minute), phase)
	if err != nil {
		t.Fatalf("insert no-cost interval %s/%s: %v", etype, eid, err)
	}
}

func floatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

var phaseCostSchemaCounter int64

// --- self-contained mysql:// test-DSN helpers (mirrors internal/store helpers;
// duplicated here so this package's test stays in its own directory) ---

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
	serverDSN, err := mysqlRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := openServer(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func mysqlDropSchema(dsn, schema string) error {
	serverDSN, err := mysqlRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := openServer(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

// mysqlRebindSchema rewrites a mysql://...host[:port]/db?params DSN to point at
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

// openServer opens a server-level (no migrations) connection for DDL on a
// mysql:// DSN, converting it to the go-sql-driver form by hand so this test
// does not depend on store-internal helpers.
func openServer(dsn string) (*sql.DB, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return nil, fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	hostport, pathAndQuery, _ := splitOn(hostpath, "/")
	dbname, _, _ := splitOn(pathAndQuery, "?")
	drv := creds + "@tcp(" + hostport + ")/" + dbname
	return sql.Open("mysql", drv)
}
