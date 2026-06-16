package observability

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var intervalPhaseCostDesc = prometheus.NewDesc(
	"teamster_wms_cost_by_phase_usd",
	"Conserved cost (USD) per (entity_type, phase), summed in MySQL from the "+
		"per-interval cost_usd assembled on wms_intervals (kind='state'). Each interval "+
		"holds exactly one phase, so a sum by (phase) over this gauge aggregates disjoint "+
		"rows — it cannot fan out the way a cost * on(entity) tag_value join can. "+
		"Intervals with no phase yet are reported as phase=\"unclassified\".",
	[]string{"entity_type", "phase"},
	nil,
)

// intervalPhaseCostRow is one grouped per-phase cost from wms_intervals (kind='state').
type intervalPhaseCostRow struct {
	entityType string
	phase      string
	costUSD    float64
}

// IntervalPhaseCostCollector is a prometheus.Collector that exposes conserved
// per-phase cost from wms_intervals (kind='state'), grouped by (entity_type, phase). The
// SQL does the grouping; Collect emits one const metric per row, so no PromQL
// aggregate downstream can multiply cost across phases. Results are cached for
// 30s.
type IntervalPhaseCostCollector struct {
	db *sql.DB

	mu        sync.Mutex
	lastQuery time.Time
	cached    []intervalPhaseCostRow
	haveCache bool
}

// NewIntervalPhaseCostCollector creates an IntervalPhaseCostCollector backed by
// db with a 30s cache TTL.
func NewIntervalPhaseCostCollector(db *sql.DB) *IntervalPhaseCostCollector {
	return &IntervalPhaseCostCollector{db: db}
}

// Describe sends the descriptor to ch.
func (c *IntervalPhaseCostCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- intervalPhaseCostDesc
}

// Collect emits one gauge series per (entity_type, phase) group, refreshing the
// cache when stale.
func (c *IntervalPhaseCostCollector) Collect(ch chan<- prometheus.Metric) {
	for _, r := range c.snapshot() {
		ch <- prometheus.MustNewConstMetric(
			intervalPhaseCostDesc,
			prometheus.GaugeValue,
			r.costUSD,
			r.entityType, r.phase,
		)
	}
}

// snapshot returns the cached rows, refreshing them if older than 30s.
func (c *IntervalPhaseCostCollector) snapshot() []intervalPhaseCostRow {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveCache && time.Since(c.lastQuery) < 30*time.Second {
		return c.cached
	}

	rows, ok := c.query()
	if ok {
		c.cached = rows
		c.lastQuery = time.Now()
		c.haveCache = true
	}
	return c.cached
}

// query sums the conserved per-interval cost_usd grouped by (entity_type,
// phase). NULL phase collapses to "unclassified" so a pending interval still
// reports its cost under a stable label. ok is false only on a query/scan error
// so the caller keeps the last good value; a legitimately empty result returns
// (nil, true) and is cached like any other.
func (c *IntervalPhaseCostCollector) query() (rows []intervalPhaseCostRow, ok bool) {
	if c.db == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := c.db.QueryContext(ctx, `
		SELECT entity_type, COALESCE(phase,'unclassified'), SUM(cost_usd)
		FROM wms_intervals
		WHERE kind = 'state' AND cost_usd IS NOT NULL
		GROUP BY entity_type, phase`)
	if err != nil {
		slog.Warn("IntervalPhaseCostCollector: query failed", "error", err)
		return nil, false
	}
	defer r.Close() //nolint:errcheck

	var out []intervalPhaseCostRow
	for r.Next() {
		var row intervalPhaseCostRow
		if err := r.Scan(&row.entityType, &row.phase, &row.costUSD); err != nil {
			slog.Warn("IntervalPhaseCostCollector: scan failed", "error", err)
			return nil, false
		}
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		slog.Warn("IntervalPhaseCostCollector: rows error", "error", err)
		return nil, false
	}
	return out, true
}
