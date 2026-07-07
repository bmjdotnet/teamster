package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
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
	rep store.ReportingStore

	mu        sync.Mutex
	lastQuery time.Time
	cached    *usageSnapshot
}

// NewUsageCollector creates a UsageCollector backed by rep with a 30s cache TTL.
func NewUsageCollector(rep store.ReportingStore) *UsageCollector {
	return &UsageCollector{rep: rep}
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

// query runs the store aggregations backing token_ledger usage metrics.
func (c *UsageCollector) query() (snap usageSnapshot) {
	if c.rep == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now().UTC()

	// --- daily aggregates (UTC calendar day) + daily by model ---
	daily, err := c.rep.DailyTokenUsage(ctx)
	if err != nil {
		slog.Warn("UsageCollector: daily query failed", "error", err)
		return
	}
	snap.dailyInputTokens = daily.DailyInputTokens
	snap.dailyOutputTokens = daily.DailyOutputTokens
	snap.dailyCacheWrite = daily.DailyCacheWrite
	snap.dailyCacheRead = daily.DailyCacheRead
	snap.dailyTotalTokens = daily.DailyTotalTokens
	snap.dailyCostUSD = daily.DailyCostUSD
	snap.modelTokens = daily.ModelTokens

	// --- block algorithm (row-based gap detection, last 24h) ---
	// Each row anchors a block: first row in a sequence sets blockStart =
	// row.ts.Truncate(hour), blockEnd = blockStart + 5h. Rows outside that
	// window start a new block. Active block = last block whose end > now.
	ledgerRows, err := c.rep.TokenLedgerRows(ctx, now.Add(-24*time.Hour))
	if err != nil {
		slog.Warn("UsageCollector: block rows query failed", "error", err)
		return
	}

	type block struct {
		start   time.Time
		end     time.Time
		tokens  int64
		cost    float64
		entries int
	}
	var blocks []block
	var cur *block
	for _, r := range ledgerRows {
		tokens := r.Input + r.Output + r.CacheRead + r.CacheCreate
		if cur == nil {
			bs := r.Timestamp.Truncate(time.Hour)
			cur = &block{start: bs, end: bs.Add(5 * time.Hour)}
		} else if !r.Timestamp.Before(cur.end) {
			blocks = append(blocks, *cur)
			bs := r.Timestamp.Truncate(time.Hour)
			cur = &block{start: bs, end: bs.Add(5 * time.Hour)}
		}
		cur.tokens += tokens
		cur.cost += r.CostUSD
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
	totals, err := c.rep.AllTimeTokenTotals(ctx)
	if err != nil {
		slog.Warn("UsageCollector: totals query failed", "error", err)
		return
	}
	snap.totalCostUSD = totals.CostUSD
	snap.totalTokens = totals.Tokens

	snap.scrapeSuccess = 1
	snap.scrapeTimestamp = float64(now.Unix())
	return
}
