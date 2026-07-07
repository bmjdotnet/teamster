package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

var attributionRateDesc = prometheus.NewDesc(
	"teamster_wms_attribution_rate",
	"Fraction (0..1) of usage_attribution rows attributed to a real WMS entity "+
		"rather than the unallocated bucket. 0 until the allocator (rollup) has "+
		"populated usage_attribution.",
	nil,
	nil,
)

// AttributionCollector is a prometheus.Collector that reports the share of
// costed messages the allocator mapped to a real entity. Backed by
// usage_attribution.method ('unallocated' vs any join method). Cached for 30s.
type AttributionCollector struct {
	rep store.ReportingStore

	mu        sync.Mutex
	lastQuery time.Time
	cached    float64
	haveCache bool
}

// NewAttributionCollector creates an AttributionCollector backed by rep with
// a 30s cache TTL.
func NewAttributionCollector(rep store.ReportingStore) *AttributionCollector {
	return &AttributionCollector{rep: rep}
}

// Describe sends the descriptor to ch.
func (c *AttributionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- attributionRateDesc
}

// Collect emits the attribution rate, refreshing the cache when stale.
func (c *AttributionCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		attributionRateDesc,
		prometheus.GaugeValue,
		c.snapshot(),
	)
}

// snapshot returns the cached rate, refreshing it if older than 30s.
func (c *AttributionCollector) snapshot() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveCache && time.Since(c.lastQuery) < 30*time.Second {
		return c.cached
	}

	rate, ok := c.query()
	if ok {
		c.cached = rate
		c.lastQuery = time.Now()
		c.haveCache = true
	}
	return c.cached
}

// query computes attributed / total over usage_attribution. Returns ok=false
// on error so a transient DB failure keeps the last good value.
func (c *AttributionCollector) query() (rate float64, ok bool) {
	if c.rep == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	total, mapped, err := c.rep.AttributionRate(ctx)
	if err != nil {
		slog.Warn("AttributionCollector: query failed", "error", err)
		return 0, false
	}
	if total == 0 {
		return 0, true // empty table: 0% attributed, but a valid scrape
	}
	return float64(mapped) / float64(total), true
}
