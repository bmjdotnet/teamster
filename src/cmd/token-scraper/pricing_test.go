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

// rateRow renders one /rates element. validTo "" means open-ended (null).
func rateRow(id int, model string, in, out, cr, cw5, cw1 float64, validFrom, validTo string) string {
	to := "null"
	if validTo != "" {
		to = `"` + validTo + `"`
	}
	return fmt.Sprintf(`{"id":%d,"runtime":"claude_code","match_kind":"prefix","model_key":%q,"variant":"base",`+
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

// usageLine is an assistant line whose cache writes are split across both TTL
// tiers: in=1000 out=500 cache_read=200 cache_write_5m=300 cache_write_1h=400.
func usageLine(model string) map[string]any {
	line := asstLine("u1", "msg_P", "req_P", model, 1000, 500, 200, 700, "text")
	usage := line["message"].(map[string]any)["usage"].(map[string]any)
	usage["cache_creation"] = map[string]any{
		"ephemeral_5m_input_tokens": 300,
		"ephemeral_1h_input_tokens": 400,
	}
	return line
}

func scraperWithHub(t *testing.T, hubURL string) (*scraper, *captureServer) {
	t.Helper()
	s, cap := newCaptureScraper(t)
	s.resolver = pricing.NewResolver(pricing.NewHTTPSource(hubURL))
	return s, cap
}

func emitOne(t *testing.T, s *scraper, cap *captureServer, line map[string]any) telemetryRow {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sess-1.jsonl")
	writeJSONL(t, path, []map[string]any{line})
	if err := s.processFile(context.Background(), path, "", ""); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("want exactly one emitted row, got %+v", cap.rows)
	}
	return cap.rows[0]
}

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

// The scraper must price from hookd's rate table, not the embedded one, with
// each cache-write bucket at its own rate.
func TestEmitPricesFromHookdRates(t *testing.T) {
	hub, hits := serveRates(t, http.StatusOK,
		rateRow(1, "claude-opus-4-8", 100, 200, 10, 30, 70, "1970-01-01T00:00:00Z", ""))
	s, cap := scraperWithHub(t, hub.URL)

	row := emitOne(t, s, cap, usageLine("claude-opus-4-8"))

	const want = (1000*100 + 500*200 + 200*10 + 300*30 + 400*70) / 1e6
	if !approx(row.CostUSD, want) {
		t.Errorf("cost_usd = %.9f, want %.9f (store rates; the embedded table would give 0.023475)", row.CostUSD, want)
	}
	if hits.Load() == 0 {
		t.Error("/rates was never fetched")
	}
}

// The usage row's own timestamp selects the rate in effect then, not "now".
func TestEmitPricesAtTheRowTimestamp(t *testing.T) {
	hub, _ := serveRates(t, http.StatusOK,
		rateRow(1, "claude-opus-4-8", 100, 0, 0, 0, 0, "1970-01-01T00:00:00Z", "2026-08-01T00:00:00Z"),
		rateRow(2, "claude-opus-4-8", 1, 0, 0, 0, 0, "2026-08-01T00:00:00Z", ""))
	s, cap := scraperWithHub(t, hub.URL)

	// asstLine stamps 2026-06-09, inside the first row's validity.
	row := emitOne(t, s, cap, usageLine("claude-opus-4-8"))

	if want := 1000 * 100 / 1e6; !approx(row.CostUSD, want) {
		t.Errorf("cost_usd = %.9f, want %.9f (the rate in effect on 2026-06-09, not the current one)", row.CostUSD, want)
	}
}

// With hookd's rate endpoint failing the row must still be priced (embedded
// tables) and posted, never dropped or costed at zero.
func TestEmitFallsBackToEmbeddedWhenRatesUnavailable(t *testing.T) {
	hub, hits := serveRates(t, http.StatusServiceUnavailable)
	s, cap := scraperWithHub(t, hub.URL)

	row := emitOne(t, s, cap, usageLine("claude-opus-4-8"))

	want := pricing.ComputeCost("claude-opus-4-8", 1000, 500, 200, 300, 400)
	if want == 0 || !approx(row.CostUSD, want) {
		t.Errorf("cost_usd = %.9f, want embedded %.9f", row.CostUSD, want)
	}
	if hits.Load() == 0 {
		t.Error("/rates was never attempted")
	}
}
