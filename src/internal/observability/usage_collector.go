package observability

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// usageSnapshot holds one scraped snapshot of token_ledger aggregate metrics.
// modelTokens maps model name → total token count for today.
type usageSnapshot struct {
	dailyInputTokens  float64
	dailyOutputTokens float64
	dailyCacheWrite   float64
	dailyCacheRead    float64
	dailyTotalTokens  float64
	dailyCostUSD      float64
	modelTokens       map[string]float64

	blockTotalTokens float64
	blockCostUSD     float64
	blockEntries     float64
	blockStartUnix   float64
	blockEndUnix     float64
	blockIsActive    float64 // 1.0 or 0.0

	totalCostUSD float64
	totalTokens  float64

	scrapeSuccess   float64 // 1.0 or 0.0
	scrapeTimestamp float64
}

var (
	usageDailyInputTokens  = prometheus.NewDesc("claude_daily_input_tokens", "Daily input tokens (UTC day)", nil, nil)
	usageDailyOutputTokens = prometheus.NewDesc("claude_daily_output_tokens", "Daily output tokens (UTC day)", nil, nil)
	usageDailyCacheWrite   = prometheus.NewDesc("claude_daily_cache_creation_tokens", "Daily cache creation tokens (UTC day)", nil, nil)
	usageDailyCacheRead    = prometheus.NewDesc("claude_daily_cache_read_tokens", "Daily cache read tokens (UTC day)", nil, nil)
	usageDailyTotalTokens  = prometheus.NewDesc("claude_daily_total_tokens", "Daily total tokens (UTC day)", nil, nil)
	usageDailyCostUSD      = prometheus.NewDesc("claude_daily_cost_usd", "Daily cost in USD (UTC day)", nil, nil)
	usageDailyModelTokens  = prometheus.NewDesc("claude_daily_model_tokens", "Daily tokens by model", []string{"model"}, nil)

	usageBlockTotalTokens = prometheus.NewDesc("claude_block_total_tokens", "Tokens in current 5-hour billing block", nil, nil)
	usageBlockCostUSD     = prometheus.NewDesc("claude_block_cost_usd", "Cost in current 5-hour billing block", nil, nil)
	usageBlockEntries     = prometheus.NewDesc("claude_block_entries", "Row count in current 5-hour billing block", nil, nil)
	usageBlockStartUnix   = prometheus.NewDesc("claude_block_start_timestamp", "Unix timestamp of current block start", nil, nil)
	usageBlockEndUnix     = prometheus.NewDesc("claude_block_end_timestamp", "Unix timestamp of current block end", nil, nil)
	usageBlockIsActive    = prometheus.NewDesc("claude_block_is_active", "1 if current block end > now", nil, nil)

	usageTotalCostUSD = prometheus.NewDesc("claude_total_cost_usd", "All-time total cost in USD", nil, nil)
	usageTotalTokens  = prometheus.NewDesc("claude_total_tokens", "All-time total tokens", nil, nil)

	usageScrapeSuccess   = prometheus.NewDesc("claude_usage_scrape_success", "1 if last collect succeeded", nil, nil)
	usageScrapeTimestamp = prometheus.NewDesc("claude_usage_scrape_timestamp", "Unix timestamp of last successful collect", nil, nil)
)

// UsageCollector is a prometheus.Collector that queries token_ledger for
// daily, block, and all-time usage metrics. Results are cached for 30s.
type UsageCollector struct {
	db *sql.DB

	mu        sync.Mutex
	lastQuery time.Time
	cached    *usageSnapshot
}

// NewUsageCollector creates a UsageCollector backed by db with a 30s cache TTL.
func NewUsageCollector(db *sql.DB) *UsageCollector {
	return &UsageCollector{db: db}
}

// Describe sends all metric descriptors to ch.
func (c *UsageCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- usageDailyInputTokens
	ch <- usageDailyOutputTokens
	ch <- usageDailyCacheWrite
	ch <- usageDailyCacheRead
	ch <- usageDailyTotalTokens
	ch <- usageDailyCostUSD
	ch <- usageDailyModelTokens
	ch <- usageBlockTotalTokens
	ch <- usageBlockCostUSD
	ch <- usageBlockEntries
	ch <- usageBlockStartUnix
	ch <- usageBlockEndUnix
	ch <- usageBlockIsActive
	ch <- usageTotalCostUSD
	ch <- usageTotalTokens
	ch <- usageScrapeSuccess
	ch <- usageScrapeTimestamp
}

// Collect emits the latest snapshot, refreshing the cache when stale.
func (c *UsageCollector) Collect(ch chan<- prometheus.Metric) {
	snap := c.snapshot()

	g := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
	}

	g(usageDailyInputTokens, snap.dailyInputTokens)
	g(usageDailyOutputTokens, snap.dailyOutputTokens)
	g(usageDailyCacheWrite, snap.dailyCacheWrite)
	g(usageDailyCacheRead, snap.dailyCacheRead)
	g(usageDailyTotalTokens, snap.dailyTotalTokens)
	g(usageDailyCostUSD, snap.dailyCostUSD)
	for model, tokens := range snap.modelTokens {
		g(usageDailyModelTokens, tokens, model)
	}
	g(usageBlockTotalTokens, snap.blockTotalTokens)
	g(usageBlockCostUSD, snap.blockCostUSD)
	g(usageBlockEntries, snap.blockEntries)
	g(usageBlockStartUnix, snap.blockStartUnix)
	g(usageBlockEndUnix, snap.blockEndUnix)
	g(usageBlockIsActive, snap.blockIsActive)
	g(usageTotalCostUSD, snap.totalCostUSD)
	g(usageTotalTokens, snap.totalTokens)
	g(usageScrapeSuccess, snap.scrapeSuccess)
	g(usageScrapeTimestamp, snap.scrapeTimestamp)
}

// snapshot returns a cached snapshot, refreshing it if older than 30s.
func (c *UsageCollector) snapshot() usageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil && time.Since(c.lastQuery) < 30*time.Second {
		return *c.cached
	}

	snap := c.query()
	c.cached = &snap
	c.lastQuery = time.Now()
	return snap
}

// ledgerRow is a minimal row from token_ledger for block computation.
type ledgerRow struct {
	ts     time.Time
	tokens int64
	cost   float64
}

// query runs the SQL aggregations against token_ledger.
func (c *UsageCollector) query() (snap usageSnapshot) {
	if c.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now().UTC()

	// --- daily aggregates (UTC calendar day) ---
	row := c.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_write_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM token_ledger
		WHERE DATE(CONVERT_TZ(timestamp, 'UTC', 'UTC')) = CURDATE()`)
	if err := row.Scan(
		&snap.dailyInputTokens,
		&snap.dailyOutputTokens,
		&snap.dailyCacheWrite,
		&snap.dailyCacheRead,
		&snap.dailyTotalTokens,
		&snap.dailyCostUSD,
	); err != nil {
		slog.Warn("UsageCollector: daily query failed", "error", err)
		return
	}

	// --- daily by model ---
	modelRows, err := c.db.QueryContext(ctx, `
		SELECT model,
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0)
		FROM token_ledger
		WHERE DATE(CONVERT_TZ(timestamp, 'UTC', 'UTC')) = CURDATE()
		GROUP BY model`)
	if err != nil {
		slog.Warn("UsageCollector: daily model query failed", "error", err)
		return
	}
	defer modelRows.Close()
	snap.modelTokens = make(map[string]float64)
	for modelRows.Next() {
		var model string
		var tokens float64
		if err := modelRows.Scan(&model, &tokens); err != nil {
			slog.Warn("UsageCollector: model row scan failed", "error", err)
			return
		}
		snap.modelTokens[model] = tokens
	}
	if err := modelRows.Err(); err != nil {
		slog.Warn("UsageCollector: model rows error", "error", err)
		return
	}
	modelRows.Close()

	// --- block algorithm (row-based gap detection, last 24h) ---
	// Each row anchors a block: first row in a sequence sets blockStart =
	// row.ts.Truncate(hour), blockEnd = blockStart + 5h. Rows outside that
	// window start a new block. Active block = last block whose end > now.
	blockRowsQ, err := c.db.QueryContext(ctx, `
		SELECT timestamp,
			input_tokens + output_tokens + cache_read_tokens + cache_write_tokens,
			cost_usd
		FROM token_ledger
		WHERE timestamp >= ?
		ORDER BY timestamp ASC`,
		now.Add(-24*time.Hour).Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		slog.Warn("UsageCollector: block rows query failed", "error", err)
		return
	}
	defer blockRowsQ.Close()

	var rows []ledgerRow
	for blockRowsQ.Next() {
		var r ledgerRow
		var ts string
		var tokens int64
		var cost float64
		if err := blockRowsQ.Scan(&ts, &tokens, &cost); err != nil {
			slog.Warn("UsageCollector: block row scan failed", "error", err)
			return
		}
		t, err := time.Parse("2006-01-02 15:04:05.999999", ts)
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04:05", ts)
			if err != nil {
				continue
			}
		}
		r.ts = t.UTC()
		r.tokens = tokens
		r.cost = cost
		rows = append(rows, r)
	}
	if err := blockRowsQ.Err(); err != nil {
		slog.Warn("UsageCollector: block rows iteration error", "error", err)
		return
	}
	blockRowsQ.Close()

	// Group rows into blocks; each new block starts when a row falls outside
	// the previous block's window.
	type block struct {
		start   time.Time
		end     time.Time
		tokens  int64
		cost    float64
		entries int
	}
	var blocks []block
	var cur *block
	for _, r := range rows {
		if cur == nil {
			bs := r.ts.Truncate(time.Hour)
			cur = &block{start: bs, end: bs.Add(5 * time.Hour)}
		} else if !r.ts.Before(cur.end) {
			blocks = append(blocks, *cur)
			bs := r.ts.Truncate(time.Hour)
			cur = &block{start: bs, end: bs.Add(5 * time.Hour)}
		}
		cur.tokens += r.tokens
		cur.cost += r.cost
		cur.entries++
	}
	if cur != nil {
		blocks = append(blocks, *cur)
	}

	// Active block = last block where end > now.
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if b.end.After(now) {
			snap.blockTotalTokens = float64(b.tokens)
			snap.blockCostUSD = b.cost
			snap.blockEntries = float64(b.entries)
			snap.blockStartUnix = float64(b.start.Unix())
			snap.blockEndUnix = float64(b.end.Unix())
			snap.blockIsActive = 1
			break
		}
	}

	// --- all-time totals ---
	row = c.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(cost_usd), 0),
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0)
		FROM token_ledger`)
	if err := row.Scan(&snap.totalCostUSD, &snap.totalTokens); err != nil {
		slog.Warn("UsageCollector: totals query failed", "error", err)
		return
	}

	snap.scrapeSuccess = 1
	snap.scrapeTimestamp = float64(now.Unix())
	return
}
