package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

var costDesc = prometheus.NewDesc(
	"teamster_wms_cost_usd",
	"Authoritative per-entity cost (USD) over the last 30 days, summed from the "+
		"conserved cost_rollup table (allocator output). Reads the deduped spine "+
		"directly, so it cannot fan out the way a bridge-gauge token join does. "+
		"The unallocated bucket is entity_type=\"\".",
	[]string{"entity_type", "entity_id", "model"},
	nil,
)

// CostCollector is a prometheus.Collector that exposes per-entity cost from the
// conserved cost_rollup table, grouped by (entity_type, entity_id, model) over a
// rolling 30-day window. agent_name is deliberately NOT a label (high
// cardinality, and cost is entity-attributed not agent-attributed). Results are
// cached for 30s.
type CostCollector struct {
	rep store.ReportingStore

	mu        sync.Mutex
	lastQuery time.Time
	cached    []store.CostRow
	haveCache bool
}

// NewCostCollector creates a CostCollector backed by rep with a 30s cache TTL.
func NewCostCollector(rep store.ReportingStore) *CostCollector {
	return &CostCollector{rep: rep}
}

// Describe sends the descriptor to ch.
func (c *CostCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- costDesc
}

// Collect emits one gauge series per (entity_type, entity_id, model) group,
// refreshing the cache when stale.
func (c *CostCollector) Collect(ch chan<- prometheus.Metric) {
	for _, r := range c.snapshot() {
		ch <- prometheus.MustNewConstMetric(
			costDesc,
			prometheus.GaugeValue,
			r.CostUSD,
			r.EntityType, r.EntityID, r.Model,
		)
	}
}

// snapshot returns the cached rows, refreshing them if older than 30s.
func (c *CostCollector) snapshot() []store.CostRow {
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

// query sums cost_rollup per entity over the last 30 days. The bucket_day window
// bounds live series to recently-active entities — v3's terminal 'done' (no hard
// delete) means cost_rollup retains old entities indefinitely. ok is false only
// on a query/scan error so the caller keeps the last good value; a legitimately
// empty cost_rollup returns (nil, true) and is cached like any other result.
func (c *CostCollector) query() (rows []store.CostRow, ok bool) {
	if c.rep == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := c.rep.CostByEntityLast30Days(ctx)
	if err != nil {
		slog.Warn("CostCollector: query failed", "error", err)
		return nil, false
	}
	return out, true
}
