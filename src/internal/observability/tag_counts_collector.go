package observability

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

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

// tagCountRow is one grouped count of tag bindings.
type tagCountRow struct {
	entityType string
	tagKey     string
	tagValue   string
	category   string
	count      float64
}

// TagCountsCollector is a prometheus.Collector that groups entity_tags by
// (entity_type, tag_key, tag_value, category). Results are cached for 30s.
type TagCountsCollector struct {
	db *sql.DB

	mu        sync.Mutex
	lastQuery time.Time
	cached    []tagCountRow
	haveCache bool
}

// NewTagCountsCollector creates a TagCountsCollector backed by db with a 30s
// cache TTL.
func NewTagCountsCollector(db *sql.DB) *TagCountsCollector {
	return &TagCountsCollector{db: db}
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
			r.count,
			r.entityType, r.tagKey, r.tagValue, r.category,
		)
	}
}

// snapshot returns the cached rows, refreshing them if older than 30s.
func (c *TagCountsCollector) snapshot() []tagCountRow {
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
func (c *TagCountsCollector) query() (rows []tagCountRow, ok bool) {
	if c.db == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := c.db.QueryContext(ctx, `
		SELECT et.entity_type, t.tag_key, t.tag_value, t.category, COUNT(*)
		FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		GROUP BY et.entity_type, t.tag_key, t.tag_value, t.category`)
	if err != nil {
		slog.Warn("TagCountsCollector: query failed", "error", err)
		return nil, false
	}
	defer r.Close() //nolint:errcheck

	var out []tagCountRow
	for r.Next() {
		var row tagCountRow
		if err := r.Scan(&row.entityType, &row.tagKey, &row.tagValue, &row.category, &row.count); err != nil {
			slog.Warn("TagCountsCollector: scan failed", "error", err)
			return nil, false
		}
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		slog.Warn("TagCountsCollector: rows error", "error", err)
		return nil, false
	}
	return out, true
}
