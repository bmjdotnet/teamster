package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bmjdotnet/teamster/internal/pricing"
)

// rateRow renders one codex /rates element. validTo "" means open-ended (null).
func rateRow(id int, model string, in, out, cr, cw5, cw1 float64, validFrom, validTo string) string {
	to := "null"
	if validTo != "" {
		to = `"` + validTo + `"`
	}
	return fmt.Sprintf(`{"id":%d,"runtime":"codex","match_kind":"prefix","model_key":%q,"variant":"base",`+
		`"input_per_mtok":"%.6f","output_per_mtok":"%.6f","cache_read_per_mtok":"%.6f",`+
		`"cache_write_5m_per_mtok":"%.6f","cache_write_1h_per_mtok":"%.6f",`+
		`"valid_from":%q,"valid_to":%s,"source_url":"https://example.test","fetched_at":"2026-10-03T00:00:00Z","notes":""}`,
		id, model, in, out, cr, cw5, cw1, validFrom, to)
}

// serveRates stands in for hookd's GET /rates and counts the requests it gets.
func serveRates(t *testing.T, status int, rows ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status != http.StatusOK {
			http.Error(w, `{"error":"rate store unavailable"}`, status)
			return
		}
		fmt.Fprintf(w, `{"rates":[%s],"count":%d}`, strings.Join(rows, ","), len(rows))
	}))
	t.Cleanup(ts.Close)
	return ts, hits
}

func scraperWithHub(t *testing.T, hubURL string) (*scraper, *captureServer) {
	t.Helper()
	s, cap, _ := newTestScraper(t)
	s.resolver = pricing.NewResolver(pricing.NewHTTPSource(hubURL))
	return s, cap
}

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

func emitDirect(t *testing.T, s *scraper, cap *captureServer, timestamp string, u tokenUsage) telemetryRow {
	t.Helper()
	cursor := &cursorEntry{ThreadID: "thread-1", SessionID: "sess-1", Model: "gpt-6-luna"}
	if err := s.emitLedgerRow(context.Background(), timestamp, u, cursor); err != nil {
		t.Fatalf("emitLedgerRow: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("want exactly one emitted row, got %+v", cap.rows)
	}
	return cap.rows[0]
}

const lunaRates = "1970-01-01T00:00:00Z"

// A whole rollout priced through hookd's rate table, not the embedded one
// (gpt-6-luna embedded is $0.10 / $0.01 cached / $0.50 per Mtok).
func TestProcessFilePricesFromHookdRates(t *testing.T) {
	hub, hits := serveRates(t, http.StatusOK, rateRow(1, "gpt-6-luna", 2, 7, 0.5, 3, 9, lunaRates, ""))
	s, cap := scraperWithHub(t, hub.URL)
	path, err := filepath.Abs("testdata/codex-0.159.3-token-usage-record.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	wants := []float64{
		(9381*2 + 11008*0.5 + 166*7) / 1e6,
		(3507*2 + 20224*0.5 + 73*7) / 1e6,
		(3931*2 + 23296*0.5 + 79*7) / 1e6,
	}
	if len(cap.rows) != len(wants) {
		t.Fatalf("got %d rows, want %d", len(cap.rows), len(wants))
	}
	for i, row := range cap.rows {
		if !approx(row.CostUSD, wants[i]) {
			t.Errorf("row %d: cost_usd = %.9f, want %.9f", i, row.CostUSD, wants[i])
		}
	}
	if hits.Load() == 0 {
		t.Error("/rates was never fetched")
	}
}

// Codex has no 1h cache tier: cache-write tokens bill in the 5m slot only.
func TestEmitLedgerRowCacheWriteBillsAtTheFiveMinuteRate(t *testing.T) {
	hub, _ := serveRates(t, http.StatusOK, rateRow(1, "gpt-6-luna", 2, 7, 0.5, 3, 9, lunaRates, ""))
	s, cap := scraperWithHub(t, hub.URL)

	row := emitDirect(t, s, cap, "2026-10-03T00:00:00Z", tokenUsage{
		InputTokens: 1000, CachedInputTokens: 200, CacheWriteInputTokens: 100, OutputTokens: 50,
	})

	// uncached input = 1000 - 200 - 100 = 700
	const want = (700*2 + 200*0.5 + 100*3 + 50*7) / 1e6
	if !approx(row.CostUSD, want) {
		t.Errorf("cost_usd = %.9f, want %.9f (a 1h-rate cache write would give %.9f)",
			row.CostUSD, want, (700*2+200*0.5+100*9+50*7)/1e6)
	}
	if row.InputTokens != 700 || row.CacheWriteTokens != 100 {
		t.Errorf("row tokens = input %d cache_write %d, want 700/100", row.InputTokens, row.CacheWriteTokens)
	}
}

// The event's own timestamp selects the rate in effect then.
func TestEmitLedgerRowPricesAtTheEventTimestamp(t *testing.T) {
	hub, _ := serveRates(t, http.StatusOK,
		rateRow(1, "gpt-6-luna", 100, 0, 0, 0, 0, lunaRates, "2026-08-01T00:00:00Z"),
		rateRow(2, "gpt-6-luna", 1, 0, 0, 0, 0, "2026-08-01T00:00:00Z", ""))
	usage := tokenUsage{InputTokens: 1000}

	t.Run("event before the change", func(t *testing.T) {
		s, cap := scraperWithHub(t, hub.URL)
		row := emitDirect(t, s, cap, "2026-06-09T19:03:27.386Z", usage)
		if want := 1000 * 100 / 1e6; !approx(row.CostUSD, want) {
			t.Errorf("cost_usd = %.9f, want %.9f (old rate)", row.CostUSD, want)
		}
	})
	t.Run("event after the change", func(t *testing.T) {
		s, cap := scraperWithHub(t, hub.URL)
		row := emitDirect(t, s, cap, "2026-09-09T19:03:27Z", usage)
		if want := 1000 * 1 / 1e6; !approx(row.CostUSD, want) {
			t.Errorf("cost_usd = %.9f, want %.9f (new rate)", row.CostUSD, want)
		}
	})
	t.Run("unparseable timestamp prices as of now", func(t *testing.T) {
		s, cap := scraperWithHub(t, hub.URL)
		row := emitDirect(t, s, cap, "not-a-time", usage)
		if want := 1000 * 1 / 1e6; !approx(row.CostUSD, want) {
			t.Errorf("cost_usd = %.9f, want %.9f (current rate)", row.CostUSD, want)
		}
	})
}

// With hookd's rate endpoint failing the row must still be priced from the
// embedded tables and posted, never dropped or costed at zero.
func TestEmitLedgerRowFallsBackToEmbeddedWhenRatesUnavailable(t *testing.T) {
	hub, hits := serveRates(t, http.StatusServiceUnavailable)
	s, cap := scraperWithHub(t, hub.URL)

	row := emitDirect(t, s, cap, "2026-10-03T00:00:00Z", tokenUsage{
		InputTokens: 1000, CachedInputTokens: 200, CacheWriteInputTokens: 100, OutputTokens: 50,
	})

	want := pricing.ComputeCost("gpt-6-luna", 700, 50, 200, 100, 0)
	if want == 0 || !approx(row.CostUSD, want) {
		t.Errorf("cost_usd = %.9f, want embedded %.9f", row.CostUSD, want)
	}
	if hits.Load() == 0 {
		t.Error("/rates was never attempted")
	}
}
