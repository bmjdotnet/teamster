package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func serveRates(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func serveBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return serveRates(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

// wireBody encodes rates the way hookd's /rates does: decimals as 6dp strings.
func wireBody(t *testing.T, rates []store.ModelRate) string {
	t.Helper()
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	out := wireRates{Rates: make([]wireRate, 0, len(rates))}
	for _, r := range rates {
		out.Rates = append(out.Rates, wireRate{
			ID: r.ID, Runtime: r.Runtime, MatchKind: r.MatchKind, ModelKey: r.ModelKey, Variant: r.Variant,
			InputPerMtok: f(r.InputPerMtok), OutputPerMtok: f(r.OutputPerMtok), CacheReadPerMtok: f(r.CacheReadPerMtok),
			CacheWrite5mPerMtok: f(r.CacheWrite5mPerMtok), CacheWrite1hPerMtok: f(r.CacheWrite1hPerMtok),
			ValidFrom: r.ValidFrom.UTC(), ValidTo: r.ValidTo, SourceURL: r.SourceURL, FetchedAt: r.FetchedAt.UTC(), Notes: r.Notes,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The literal key names below are the contract with internal/server/rates.go;
// they are written out (not derived from wireRate) so a renamed tag on either
// side fails here.
const sampleRatesBody = `{
  "rates": [
    {"id": 1, "runtime": "claude_code", "match_kind": "prefix", "model_key": "claude-opus-4-6", "variant": "base",
     "input_per_mtok": "5.000000", "output_per_mtok": "25.000000", "cache_read_per_mtok": "0.500000",
     "cache_write_5m_per_mtok": "6.250000", "cache_write_1h_per_mtok": "10.000000",
     "valid_from": "1970-01-01T00:00:00Z", "valid_to": null,
     "source_url": "https://example.test/pricing", "fetched_at": "2026-10-03T00:00:00Z", "notes": "seed"},
    {"id": 2, "runtime": "codex", "match_kind": "exact", "model_key": "gpt-x", "variant": "1m",
     "input_per_mtok": "1.250000", "output_per_mtok": "10.000000", "cache_read_per_mtok": "0.125000",
     "cache_write_5m_per_mtok": "0.000000", "cache_write_1h_per_mtok": "0.000000",
     "valid_from": "2026-01-01T00:00:00Z", "valid_to": "2026-09-01T00:00:00Z",
     "source_url": "", "fetched_at": "2026-10-03T12:00:00Z", "notes": ""}
  ],
  "count": 2,
  "fetched_at": "2026-10-03T12:00:00Z"
}`

func TestHTTPSourceParsesWireFormat(t *testing.T) {
	ts := serveBody(t, sampleRatesBody)
	got, err := NewHTTPSource(ts.URL).ListRates(context.Background(), store.RateFilter{})
	if err != nil {
		t.Fatalf("ListRates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rates, want 2", len(got))
	}

	a := got[0]
	if a.ID != 1 || a.Runtime != "claude_code" || a.MatchKind != "prefix" || a.ModelKey != "claude-opus-4-6" || a.Variant != "base" {
		t.Errorf("row 0 identity = %+v", a)
	}
	if a.InputPerMtok != 5 || a.OutputPerMtok != 25 || a.CacheReadPerMtok != 0.5 || a.CacheWrite5mPerMtok != 6.25 || a.CacheWrite1hPerMtok != 10 {
		t.Errorf("row 0 rates = %v/%v/%v/%v/%v", a.InputPerMtok, a.OutputPerMtok, a.CacheReadPerMtok, a.CacheWrite5mPerMtok, a.CacheWrite1hPerMtok)
	}
	if !a.ValidFrom.Equal(store.SeedRateValidFrom) || a.ValidTo != nil {
		t.Errorf("row 0 validity = %v .. %v, want epoch .. open", a.ValidFrom, a.ValidTo)
	}
	if a.SourceURL != "https://example.test/pricing" || a.Notes != "seed" || !a.FetchedAt.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("row 0 provenance = %q %q %v", a.SourceURL, a.Notes, a.FetchedAt)
	}

	b := got[1]
	if b.ID != 2 || b.Runtime != "codex" || b.Variant != "1m" || b.CacheReadPerMtok != 0.125 {
		t.Errorf("row 1 = %+v", b)
	}
	if b.ValidTo == nil || !b.ValidTo.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("row 1 valid_to = %v, want 2026-09-01", b.ValidTo)
	}
}

func TestHTTPSourceRequestShape(t *testing.T) {
	var path, query, method string
	ts := serveRates(t, func(w http.ResponseWriter, r *http.Request) {
		path, query, method = r.URL.Path, r.URL.RawQuery, r.Method
		_, _ = w.Write([]byte(`{"rates":[],"count":0}`))
	})

	for _, tc := range []struct {
		name, base, runtime, wantQuery string
	}{
		{"no filter", ts.URL, "", ""},
		{"runtime filter", ts.URL, "codex", "runtime=codex"},
		{"trailing slash on base", ts.URL + "/", "claude_code", "runtime=claude_code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHTTPSource(tc.base).ListRates(context.Background(), store.RateFilter{Runtime: tc.runtime}); err != nil {
				t.Fatalf("ListRates: %v", err)
			}
			if method != http.MethodGet || path != "/rates" || query != tc.wantQuery {
				t.Errorf("request = %s %s?%s, want GET /rates?%s", method, path, query, tc.wantQuery)
			}
		})
	}
}

func TestHTTPSourceEmptyTableIsEmptySlice(t *testing.T) {
	ts := serveBody(t, `{"rates":[],"count":0}`)
	got, err := NewHTTPSource(ts.URL).ListRates(context.Background(), store.RateFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty, nil (the Resolver decides an empty table is a failure)", got, err)
	}
}

func TestHTTPSourceNon200IsAnError(t *testing.T) {
	ts := serveRates(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"rate store unavailable"}`))
	})
	_, err := NewHTTPSource(ts.URL).ListRates(context.Background(), store.RateFilter{})
	if err == nil {
		t.Fatal("want an error for a 503")
	}
	for _, needle := range []string{"503", "rate store unavailable"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("error %q missing %q", err, needle)
		}
	}
}

func TestHTTPSourceBadRowFailsTheWholeFetch(t *testing.T) {
	row := func(input string) string {
		return `{"rates":[{"id":9,"runtime":"claude_code","match_kind":"exact","model_key":"bad-model","variant":"base",` +
			`"input_per_mtok":"` + input + `","output_per_mtok":"1.000000","cache_read_per_mtok":"1.000000",` +
			`"cache_write_5m_per_mtok":"1.000000","cache_write_1h_per_mtok":"1.000000",` +
			`"valid_from":"1970-01-01T00:00:00Z","valid_to":null,"source_url":"","fetched_at":"2026-10-03T00:00:00Z","notes":""}]}`
	}
	for _, bad := range []string{"", "abc", "NaN", "Inf", "-1.000000"} {
		t.Run("input="+bad, func(t *testing.T) {
			ts := serveBody(t, row(bad))
			got, err := NewHTTPSource(ts.URL).ListRates(context.Background(), store.RateFilter{})
			if err == nil {
				t.Fatalf("accepted input_per_mtok %q: %+v", bad, got)
			}
			if !strings.Contains(err.Error(), "bad-model") || !strings.Contains(err.Error(), "input_per_mtok") {
				t.Errorf("error %q should name the row's model and the field", err)
			}
		})
	}
}

func TestHTTPSourceRejectsNonJSONAndOversizedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"html":      "<html>dashboard</html>",
		"truncated": `{"rates":[{"id":1`,
		"oversized": `{"rates":[],"pad":"` + strings.Repeat("a", maxRatesBody) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ts := serveBody(t, body)
			if got, err := NewHTTPSource(ts.URL).ListRates(context.Background(), store.RateFilter{}); err == nil {
				t.Fatalf("accepted body, got %+v", got)
			}
		})
	}
}

func TestHTTPSourceAppliesAtFilterClientSide(t *testing.T) {
	old := store.ModelRate{
		ID: 1, Runtime: "claude_code", MatchKind: "exact", ModelKey: "m", Variant: "base",
		InputPerMtok: 3, ValidFrom: store.SeedRateValidFrom,
	}
	closedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	old.ValidTo = &closedAt
	cur := old
	cur.ID, cur.InputPerMtok, cur.ValidFrom, cur.ValidTo = 2, 2, closedAt, nil

	ts := serveBody(t, wireBody(t, []store.ModelRate{old, cur}))
	src := NewHTTPSource(ts.URL)

	for _, tc := range []struct {
		at     time.Time
		wantID int64
	}{
		{time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 2},
	} {
		at := tc.at
		got, err := src.ListRates(context.Background(), store.RateFilter{At: &at})
		if err != nil || len(got) != 1 || got[0].ID != tc.wantID {
			t.Errorf("At=%v: got %+v, %v; want only row %d", at, got, err, tc.wantID)
		}
	}
	if all, err := src.ListRates(context.Background(), store.RateFilter{}); err != nil || len(all) != 2 {
		t.Errorf("no At: got %d rows, %v; want full history (2)", len(all), err)
	}
}

func TestHTTPSourceUnreachableAndCancelled(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		ts := httptest.NewServer(http.NotFoundHandler())
		url := ts.URL
		ts.Close()
		if _, err := NewHTTPSource(url).ListRates(context.Background(), store.RateFilter{}); err == nil {
			t.Fatal("want an error with the hub down")
		}
	})
	t.Run("context deadline", func(t *testing.T) {
		ts := serveRates(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := NewHTTPSource(ts.URL).ListRates(ctx, store.RateFilter{}); err == nil {
			t.Fatal("want an error when the context expires")
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("returned after %v; the context deadline was not honored", d)
		}
	})
}

// The point of the HTTP path: a Resolver over hookd's rate table must price
// exactly like one over the store the table came from.
func TestHTTPSourceResolverPricesLikeTheStore(t *testing.T) {
	ctx := context.Background()
	ms := newSeededStore(t)
	all, err := ms.ListRates(ctx, store.RateFilter{})
	if err != nil || len(all) == 0 {
		t.Fatalf("seeded ListRates: %d rows, %v", len(all), err)
	}
	ts := serveBody(t, wireBody(t, all))

	direct, viaHTTP := NewResolver(ms), NewResolver(NewHTTPSource(ts.URL))
	if err := viaHTTP.Refresh(ctx); err != nil {
		t.Fatalf("Refresh over HTTP: %v", err)
	}
	captureWarns(t)
	for _, r := range all {
		for _, model := range []string{r.ModelKey, r.ModelKey + "-20260101"} {
			want := direct.Price(ctx, r.Runtime, model, rTime0, testTokens)
			got := viaHTTP.Price(ctx, r.Runtime, model, rTime0, testTokens)
			if got.Source != want.Source || got.RateID != want.RateID || !close12(got.CostUSD, want.CostUSD) {
				t.Errorf("%s/%s: via HTTP %+v, direct %+v", r.Runtime, model, got, want)
			}
		}
	}
}

func TestHTTPSourceResolverFallsBackWhileHubIsDownThenRecovers(t *testing.T) {
	ctx := context.Background()
	buf := captureWarns(t)
	ms := newSeededStore(t)
	all, err := ms.ListRates(ctx, store.RateFilter{})
	if err != nil {
		t.Fatal(err)
	}

	healthy := false
	ts := serveRates(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			http.Error(w, `{"error":"rate store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(wireBody(t, all)))
	})
	r, clock := newTestResolver(NewHTTPSource(ts.URL))

	want := ComputeCost("claude-opus-4-8", testTokens.Input, testTokens.Output, testTokens.CacheRead, testTokens.CacheWrite5m, testTokens.CacheWrite1h)
	got := r.Price(ctx, store.RateRuntimeClaudeCode, "claude-opus-4-8", rTime0, testTokens)
	if got.Source != SourceFallback || !close12(got.CostUSD, want) {
		t.Fatalf("hub down: %+v, want fallback priced at %v", got, want)
	}
	if !strings.Contains(buf.String(), "pricing rate refresh failed") || !strings.Contains(buf.String(), "503") {
		t.Errorf("expected a WARN naming the refresh failure and status:\n%s", buf.String())
	}

	healthy = true
	clock.advance(defaultRetryEvery)
	if got := r.Price(ctx, store.RateRuntimeClaudeCode, "claude-opus-4-8", rTime0, testTokens); got.Source != SourceStore || got.RateID == 0 {
		t.Errorf("after recovery: %+v, want a store price", got)
	}
}
