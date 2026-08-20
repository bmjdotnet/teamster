package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

var decompositionDesc = prometheus.NewDesc(
	"teamster_wms_outcome_decomposition",
	"Current WMS outcome_edges DAG adoption by kind: kind=\"root_outcomes\" "+
		"counts outcomes with no parent edge; kind=\"parented_outcomes\" "+
		"counts distinct outcomes with at least one parent edge; "+
		"kind=\"edges\" counts total outcome_edges rows.",
	[]string{"kind"},
	nil,
)

// DecompositionCollector is a prometheus.Collector that reports outcome DAG
// decomposition adoption from outcome_edges. Results are cached for 30s.
type DecompositionCollector struct {
	rep store.ReportingStore

	mu        sync.Mutex
	lastQuery time.Time
	root      float64
	parented  float64
	edges     float64
	haveCache bool
}

// NewDecompositionCollector creates a DecompositionCollector backed by rep
// with a 30s cache TTL.
func NewDecompositionCollector(rep store.ReportingStore) *DecompositionCollector {
	return &DecompositionCollector{rep: rep}
}

// Describe sends the descriptor to ch.
func (c *DecompositionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- decompositionDesc
}

// Collect emits the root/parented/edges gauges, refreshing the cache when stale.
func (c *DecompositionCollector) Collect(ch chan<- prometheus.Metric) {
	root, parented, edges := c.snapshot()
	ch <- prometheus.MustNewConstMetric(decompositionDesc, prometheus.GaugeValue, root, "root_outcomes")
	ch <- prometheus.MustNewConstMetric(decompositionDesc, prometheus.GaugeValue, parented, "parented_outcomes")
	ch <- prometheus.MustNewConstMetric(decompositionDesc, prometheus.GaugeValue, edges, "edges")
}

// snapshot returns the cached counts, refreshing them if older than 30s.
func (c *DecompositionCollector) snapshot() (root, parented, edges float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveCache && time.Since(c.lastQuery) < 30*time.Second {
		return c.root, c.parented, c.edges
	}

	r, p, e, ok := c.query()
	if ok {
		c.root = r
		c.parented = p
		c.edges = e
		c.lastQuery = time.Now()
		c.haveCache = true
	}
	return c.root, c.parented, c.edges
}

// query counts root/parented outcomes and total edges via
// OutcomeDecompositionCounts. Returns ok=false on error so a transient DB
// failure keeps the last good value.
func (c *DecompositionCollector) query() (root, parented, edges float64, ok bool) {
	if c.rep == nil {
		return 0, 0, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, p, e, err := c.rep.OutcomeDecompositionCounts(ctx)
	if err != nil {
		slog.Warn("DecompositionCollector: query failed", "error", err)
		return 0, 0, 0, false
	}
	return float64(r), float64(p), float64(e), true
}
