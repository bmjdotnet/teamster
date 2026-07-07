package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

var tagCountsDesc = prometheus.NewDesc(
	"teamster_wms_tag_counts",
	"Count of entity_tags bindings by entity type, tag key/value, and category "+
		"(context vs lifecycle). Joins entity_tags to the tags vocabulary; powers "+
		"the WMS Pulse tag-distribution panels.",
	[]string{"entity_type", "tag_key", "tag_value", "category"},
	nil,
)

// TagCountsCollector is a prometheus.Collector that groups entity_tags by
// (entity_type, tag_key, tag_value, category). Results are cached for 30s.
type TagCountsCollector struct {
	rep store.ReportingStore

	mu        sync.Mutex
	lastQuery time.Time
	cached    []store.TagCountRow
	haveCache bool
}

// NewTagCountsCollector creates a TagCountsCollector backed by rep with a 30s
// cache TTL.
func NewTagCountsCollector(rep store.ReportingStore) *TagCountsCollector {
	return &TagCountsCollector{rep: rep}
}

// Describe sends the descriptor to ch.
func (c *TagCountsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- tagCountsDesc
}

// Collect emits one gauge series per (entity_type, tag_key, tag_value,
// category) group, refreshing the cache when stale.
func (c *TagCountsCollector) Collect(ch chan<- prometheus.Metric) {
	for _, r := range c.snapshot() {
		ch <- prometheus.MustNewConstMetric(
			tagCountsDesc,
			prometheus.GaugeValue,
			float64(r.Count),
			r.EntityType, r.TagKey, r.TagValue, r.Category,
		)
	}
}

// snapshot returns the cached rows, refreshing them if older than 30s.
func (c *TagCountsCollector) snapshot() []store.TagCountRow {
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

// query groups entity_tags joined to the tags vocabulary. ok is false only on a
// query/scan error so the caller keeps the last good value; a legitimately empty
// entity_tags returns (nil, true) and is cached like any other result.
func (c *TagCountsCollector) query() (rows []store.TagCountRow, ok bool) {
	if c.rep == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := c.rep.TagBindingCounts(ctx)
	if err != nil {
		slog.Warn("TagCountsCollector: query failed", "error", err)
		return nil, false
	}
	return out, true
}
