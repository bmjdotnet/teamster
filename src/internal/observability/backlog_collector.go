package observability

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var backlogDesc = prometheus.NewDesc(
	"teamster_wms_unattributed_backlog",
	"Number of token_ledger messages with no usage_attribution row yet — the "+
		"allocator's anti-join depth. A small, steady value is healthy "+
		"(in-flight, self-heals on the next rollup pass); a growing value means "+
		"the rollup is stalled. Rollup-liveness monitor, pairs with "+
		"teamster_wms_attribution_rate.",
	nil,
	nil,
)

// BacklogCollector is a prometheus.Collector that reports how many ledger
// messages are awaiting attribution (the allocator's LEFT-JOIN backlog). It is a
// rollup-liveness signal, not a correctness one — it cannot false-alarm because
// a healthy pipeline keeps it small and a stalled one makes it grow. Cached 30s.
type BacklogCollector struct {
	db *sql.DB

	mu        sync.Mutex
	lastQuery time.Time
	cached    float64
	haveCache bool
}

// NewBacklogCollector creates a BacklogCollector backed by db with a 30s cache TTL.
func NewBacklogCollector(db *sql.DB) *BacklogCollector {
	return &BacklogCollector{db: db}
}

// Describe sends the descriptor to ch.
func (c *BacklogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- backlogDesc
}

// Collect emits the backlog depth, refreshing the cache when stale.
func (c *BacklogCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		backlogDesc,
		prometheus.GaugeValue,
		c.snapshot(),
	)
}

// snapshot returns the cached depth, refreshing it if older than 30s.
func (c *BacklogCollector) snapshot() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveCache && time.Since(c.lastQuery) < 30*time.Second {
		return c.cached
	}

	depth, ok := c.query()
	if ok {
		c.cached = depth
		c.lastQuery = time.Now()
		c.haveCache = true
	}
	return c.cached
}

// query counts ledger messages with no usage_attribution row — the same
// anti-join the allocator's Allocate pass drains. Returns ok=false on error so a
// transient DB failure keeps the last good value.
func (c *BacklogCollector) query() (depth float64, ok bool) {
	if c.db == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM token_ledger t
		LEFT JOIN usage_attribution ua ON ua.message_id = t.message_id
		WHERE ua.message_id IS NULL`).Scan(&depth)
	if err != nil {
		slog.Warn("BacklogCollector: query failed", "error", err)
		return 0, false
	}
	return depth, true
}
