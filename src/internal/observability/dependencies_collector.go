package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

var dependenciesDesc = prometheus.NewDesc(
	"teamster_wms_dependencies",
	"Current WMS dependency edges by direction over the v3 entity_dependencies "+
		"table: direction=\"blocker\" counts distinct entities that block "+
		"something; direction=\"blocked\" counts distinct entities stuck behind "+
		"a blocker.",
	[]string{"direction"},
	nil,
)

// DependenciesCollector is a prometheus.Collector that reports how many entities
// are acting as blockers vs how many are blocked, from entity_dependencies.
// Results are cached for 30s.
type DependenciesCollector struct {
	rep store.ReportingStore

	mu        sync.Mutex
	lastQuery time.Time
	blockers  float64
	blocked   float64
	haveCache bool
}

// NewDependenciesCollector creates a DependenciesCollector backed by rep with
// a 30s cache TTL.
func NewDependenciesCollector(rep store.ReportingStore) *DependenciesCollector {
	return &DependenciesCollector{rep: rep}
}

// Describe sends the descriptor to ch.
func (c *DependenciesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- dependenciesDesc
}

// Collect emits the blocker and blocked gauges, refreshing the cache when stale.
func (c *DependenciesCollector) Collect(ch chan<- prometheus.Metric) {
	blockers, blocked := c.snapshot()
	ch <- prometheus.MustNewConstMetric(dependenciesDesc, prometheus.GaugeValue, blockers, "blocker")
	ch <- prometheus.MustNewConstMetric(dependenciesDesc, prometheus.GaugeValue, blocked, "blocked")
}

// snapshot returns the cached counts, refreshing them if older than 30s.
func (c *DependenciesCollector) snapshot() (blockers, blocked float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveCache && time.Since(c.lastQuery) < 30*time.Second {
		return c.blockers, c.blocked
	}

	b, bd, ok := c.query()
	if ok {
		c.blockers = b
		c.blocked = bd
		c.lastQuery = time.Now()
		c.haveCache = true
	}
	return c.blockers, c.blocked
}

// query counts distinct blocker and blocked entities in entity_dependencies.
// An entity is keyed by (type, id) so the same id under different types counts
// once each. Returns ok=false on error so a transient DB failure keeps the last
// good value.
func (c *DependenciesCollector) query() (blockers, blocked float64, ok bool) {
	if c.rep == nil {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b, bd, err := c.rep.DependencyCounts(ctx)
	if err != nil {
		slog.Warn("DependenciesCollector: query failed", "error", err)
		return 0, 0, false
	}
	return float64(b), float64(bd), true
}
