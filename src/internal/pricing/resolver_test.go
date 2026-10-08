package pricing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

var (
	testTokens = Tokens{Input: 1000, Output: 500, CacheRead: 200, CacheWrite5m: 100, CacheWrite1h: 50}
	rTime0     = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

func close12(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func captureWarns(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func newSeededStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeSource is a scriptable RateSource.
type fakeSource struct {
	rates []store.ModelRate
	err   error
	calls int
}

func (f *fakeSource) ListRates(context.Context, store.RateFilter) ([]store.ModelRate, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rates, nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestResolver(src RateSource) (*Resolver, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	r := NewResolver(src)
	r.now = c.now
	return r, c
}

func runtimeFor(model string) string {
	if strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o") {
		return store.RateRuntimeCodex
	}
	return store.RateRuntimeClaudeCode
}

// The frozen v73 seed must price every embedded model exactly as the embedded
// tables do — the guard against a typo in the hand-authored per-Mtok seed.
func TestResolverSeedPricesLikeEmbeddedTables(t *testing.T) {
	ctx := context.Background()
	r := NewResolver(newSeededStore(t))
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	var models []string
	for key := range Known {
		models = append(models, key, key+"-20260101")
	}
	// Same-class fallback models, priced as estimates on both sides.
	models = append(models, "claude-opus-9-9", "claude-sonnet-9", "claude-haiku-9", "claude-fable-9")
	// Unpriced on both sides: must be $0 on both, never one-sided.
	models = append(models, "o3", "claude-unknown-model", "gpt-5.2-codex")

	captureWarns(t)
	for _, model := range models {
		runtime := runtimeFor(model)
		got := r.Price(ctx, runtime, model, rTime0, testTokens)
		want := ComputeCost(model, testTokens.Input, testTokens.Output, testTokens.CacheRead, testTokens.CacheWrite5m, testTokens.CacheWrite1h)
		if !close12(got.CostUSD, want) {
			t.Errorf("%s: store-backed %v != embedded %v (source %s)", model, got.CostUSD, want, got.Source)
		}
		if want == 0 && got.Source != SourceUnknown {
			t.Errorf("%s: $0 but source %s, want %s", model, got.Source, SourceUnknown)
		}
		if want != 0 && (got.Source != SourceStore || got.RateID == 0) {
			t.Errorf("%s: source %s rate_id %d, want a store-priced row", model, got.Source, got.RateID)
		}
	}
}

func TestResolverRateChangeWithoutRecompile(t *testing.T) {
	ctx := context.Background()
	s := newSeededStore(t)
	r, clock := newTestResolver(s)
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	const model = "claude-sonnet-4-6"
	before := r.Price(ctx, "claude_code", model, rTime0, testTokens)

	changeAt := rTime0.Add(48 * time.Hour)
	id, err := s.UpsertRate(ctx, store.ModelRate{
		Runtime: "claude_code", MatchKind: store.RateMatchPrefix, ModelKey: model, Variant: "base",
		InputPerMtok: 2, OutputPerMtok: 10, CacheReadPerMtok: 0.2, CacheWrite5mPerMtok: 2.5, CacheWrite1hPerMtok: 4,
		ValidFrom: changeAt, SourceURL: "https://example.test/pricing", FetchedAt: changeAt,
	})
	if err != nil {
		t.Fatalf("UpsertRate: %v", err)
	}
	clock.advance(refreshTimeout + defaultRefreshEvery) // snapshot is now due for refresh

	after := r.Price(ctx, "claude_code", model, changeAt.Add(time.Hour), testTokens)
	if after.RateID != id || after.CostUSD >= before.CostUSD {
		t.Errorf("after the change: %+v, want the new row %d priced below %v", after, id, before.CostUSD)
	}
	old := r.Price(ctx, "claude_code", model, changeAt.Add(-time.Hour), testTokens)
	if old.RateID != before.RateID || !close12(old.CostUSD, before.CostUSD) {
		t.Errorf("usage before the change repriced: %+v, want %+v (history untouched)", old, before)
	}
}

func TestResolverForcedRefreshFindsNewModel(t *testing.T) {
	ctx := context.Background()
	s := newSeededStore(t)
	r, _ := newTestResolver(s)
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	captureWarns(t)
	if got := r.Price(ctx, "codex", "gpt-7-new", rTime0, testTokens); got.Source != SourceUnknown {
		t.Fatalf("before the row exists: %+v, want unknown", got)
	}
	// An operator adds the row; the next miss's forced refresh may be
	// rate-limited, so advance past it.
	if _, err := s.UpsertRate(ctx, store.ModelRate{
		Runtime: "codex", MatchKind: store.RateMatchPrefix, ModelKey: "gpt-7-new", Variant: "base",
		InputPerMtok: 1, OutputPerMtok: 2, ValidFrom: rTime0.Add(-time.Hour),
		SourceURL: "https://example.test/pricing", FetchedAt: rTime0,
	}); err != nil {
		t.Fatalf("UpsertRate: %v", err)
	}
	r.now = (&fakeClock{t: r.now().Add(2 * defaultForcedEvery)}).now
	if got := r.Price(ctx, "codex", "gpt-7-new", rTime0, testTokens); got.Source != SourceStore || got.CostUSD == 0 {
		t.Errorf("after the row exists: %+v, want a store price via forced refresh", got)
	}
}

func TestResolverColdStartStoreDownThenRecovers(t *testing.T) {
	ctx := context.Background()
	buf := captureWarns(t)
	src := &fakeSource{err: errors.New("connection refused")}
	r, clock := newTestResolver(src)

	got := r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens)
	want := ComputeCost("claude-opus-4-8", testTokens.Input, testTokens.Output, testTokens.CacheRead, testTokens.CacheWrite5m, testTokens.CacheWrite1h)
	if got.Source != SourceFallback || got.RateID != 0 || !close12(got.CostUSD, want) {
		t.Fatalf("store down: %+v, want fallback priced at %v", got, want)
	}
	for _, needle := range []string{"pricing rate refresh failed", "embedded", "connection refused"} {
		if !strings.Contains(buf.String(), needle) {
			t.Errorf("WARN output missing %q:\n%s", needle, buf.String())
		}
	}
	if s := r.Stats(); s.Loaded || s.FallbackPriced != 1 || s.RefreshErrors != 1 {
		t.Errorf("stats = %+v, want unloaded, 1 fallback, 1 refresh error", s)
	}

	// Within the retry window the store is not hammered.
	r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens)
	if src.calls != 1 {
		t.Errorf("ListRates called %d times inside the retry window, want 1", src.calls)
	}

	// The store recovers; the next retry picks it up.
	src.err = nil
	src.rates = store.ModelPricingSeedV1()
	for i := range src.rates {
		src.rates[i].ID = int64(i + 1)
	}
	clock.advance(defaultRetryEvery)
	if got := r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens); got.Source != SourceStore || got.RateID == 0 {
		t.Errorf("after recovery: %+v, want a store price", got)
	}
}

func TestResolverStaleSnapshotKeepsPricing(t *testing.T) {
	ctx := context.Background()
	captureWarns(t)
	src := &fakeSource{rates: store.ModelPricingSeedV1()}
	for i := range src.rates {
		src.rates[i].ID = int64(i + 1)
	}
	r, clock := newTestResolver(src)
	first := r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens)
	if first.Source != SourceStore {
		t.Fatalf("first price: %+v", first)
	}
	src.err = errors.New("store blinked")
	clock.advance(defaultRefreshEvery)
	again := r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens)
	if again != first {
		t.Errorf("with the store down a loaded resolver changed its price: %+v vs %+v", again, first)
	}
	if s := r.Stats(); s.RefreshErrors != 1 || !s.Loaded {
		t.Errorf("stats = %+v, want 1 refresh error and a retained snapshot", s)
	}
}

func TestResolverEmptyTableIsAFailureNotAZero(t *testing.T) {
	ctx := context.Background()
	captureWarns(t)
	src := &fakeSource{rates: nil}
	r, _ := newTestResolver(src)
	got := r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens)
	if got.Source != SourceFallback || got.CostUSD == 0 {
		t.Errorf("empty model_pricing: %+v, want embedded fallback pricing, not $0", got)
	}
}

func TestResolverUnknownModelIsLoudAndCounted(t *testing.T) {
	ctx := context.Background()
	buf := captureWarns(t)
	src := &fakeSource{rates: store.ModelPricingSeedV1()}
	for i := range src.rates {
		src.rates[i].ID = int64(i + 1)
	}
	r, clock := newTestResolver(src)

	const model = "claude-phantom-model-xyz9"
	for i := 0; i < 5; i++ {
		got := r.Price(ctx, "claude_code", model, rTime0, testTokens)
		if got.CostUSD != 0 || got.Source != SourceUnknown || got.RateID != 0 {
			t.Fatalf("call %d: %+v, want $0 / unknown", i, got)
		}
	}
	if n := strings.Count(buf.String(), "no pricing row for model"); n != 1 {
		t.Errorf("WARN emitted %d times for 5 calls inside one window, want exactly 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), model) {
		t.Errorf("WARN does not name the model id:\n%s", buf.String())
	}
	s := r.Stats()
	if s.UnknownPriced != 5 || s.UnknownModels[model] != 5 {
		t.Errorf("counters = %d total / %d for model, want 5 / 5 (every occurrence counted)", s.UnknownPriced, s.UnknownModels[model])
	}
	// Forced refreshes are rate-limited: initial load + one forced, not one per miss.
	if src.calls != 2 {
		t.Errorf("ListRates called %d times across 5 unknown prices, want 2", src.calls)
	}

	clock.advance(defaultWarnEvery)
	r.Price(ctx, "claude_code", model, rTime0, testTokens)
	if n := strings.Count(buf.String(), "no pricing row for model"); n != 2 || !strings.Contains(buf.String(), "suppressed=4") {
		t.Errorf("after the window: %d WARNs, want 2 and a suppressed=4 report:\n%s", n, buf.String())
	}
}

func TestResolverUnknownModelCardinalityIsBounded(t *testing.T) {
	ctx := context.Background()
	captureWarns(t)
	src := &fakeSource{rates: store.ModelPricingSeedV1()}
	r, _ := newTestResolver(src)
	for i := 0; i < maxUnknownModels+50; i++ {
		r.Price(ctx, "claude_code", "zz-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), rTime0, testTokens)
	}
	if n := len(r.Stats().UnknownModels); n > maxUnknownModels+1 {
		t.Errorf("tracking %d distinct unknown models, want at most %d+1", n, maxUnknownModels)
	}
}

func TestResolver1MLabelWarnsOnceAndPricesAtBase(t *testing.T) {
	ctx := context.Background()
	buf := captureWarns(t)
	r := NewResolver(newSeededStore(t))
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	plain := r.Price(ctx, "claude_code", "claude-opus-4-8", rTime0, testTokens)
	for i := 0; i < 3; i++ {
		got := r.Price(ctx, "claude_code", "claude-opus-4-8[1m]", rTime0, testTokens)
		if got.CostUSD != plain.CostUSD || got.RateID != plain.RateID {
			t.Fatalf("[1m] priced %+v, want the base price %+v", got, plain)
		}
	}
	if n := strings.Count(buf.String(), "[1m]-labelled model priced at the base rate"); n != 1 {
		t.Errorf("[1m] WARN emitted %d times, want 1:\n%s", n, buf.String())
	}
}

func TestResolverClassRowIsFlaggedAsEstimate(t *testing.T) {
	ctx := context.Background()
	buf := captureWarns(t)
	r := NewResolver(newSeededStore(t))
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := r.Price(ctx, "claude_code", "claude-opus-9-9", rTime0, testTokens); got.Source != SourceStore || got.CostUSD == 0 {
		t.Fatalf("class-priced: %+v", got)
	}
	if !strings.Contains(buf.String(), "class fallback row (estimate") || r.Stats().ClassEstimatePriced != 1 {
		t.Errorf("class estimate not flagged/counted:\n%s\n%+v", buf.String(), r.Stats())
	}
}

func BenchmarkResolverPrice(b *testing.B) {
	ctx := context.Background()
	src := &fakeSource{rates: store.ModelPricingSeedV1()}
	r := NewResolver(src)
	_ = r.Refresh(ctx)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Price(ctx, "claude_code", "claude-sonnet-4-5-20250929", rTime0, testTokens)
	}
}

// No store configured is not an outage: Price must not panic, must price from
// the embedded tables exactly as ComputeCost does, must stay quiet about a
// store, and must keep an unknown model loud.
func TestResolverNilSourceFallsBackToEmbeddedQuietly(t *testing.T) {
	ctx := context.Background()
	buf := captureWarns(t)
	r := NewResolver(nil)
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh with no source: %v", err)
	}

	for _, model := range []string{"claude-opus-4-8", "claude-fable-5-1", "gpt-6-luna", "claude-sonnet-4-5-20250929"} {
		got := r.Price(ctx, runtimeFor(model), model, rTime0, testTokens)
		want := ComputeCost(model, testTokens.Input, testTokens.Output, testTokens.CacheRead, testTokens.CacheWrite5m, testTokens.CacheWrite1h)
		if got.Source != SourceFallback || got.RateID != 0 || !close12(got.CostUSD, want) {
			t.Errorf("%s: %+v, want fallback priced at %v", model, got, want)
		}
	}
	if strings.Contains(buf.String(), "store") {
		t.Errorf("nil source must not WARN about a store outage:\n%s", buf.String())
	}

	if got := r.Price(ctx, "claude_code", "claude-phantom-xyz", rTime0, testTokens); got.Source != SourceUnknown || got.CostUSD != 0 {
		t.Errorf("unknown model with no store: %+v, want $0 / unknown", got)
	}
	if !strings.Contains(buf.String(), "no pricing row for model") || r.Stats().UnknownModels["claude-phantom-xyz"] != 1 {
		t.Errorf("unknown model must stay loud and counted:\n%s\n%+v", buf.String(), r.Stats())
	}
	if s := r.Stats(); s.Loaded || s.Refreshes != 0 || s.RefreshErrors != 0 {
		t.Errorf("stats = %+v, want unloaded with no refresh activity", s)
	}
}

// sonnet-5-5 and sonnet-5 carry identical rates, so only the row id can show
// that claude-sonnet-5-5 is served by its own row and not by the claude-sonnet-5
// prefix that would also match it.
func TestResolverSonnet55ResolvesViaItsOwnRow(t *testing.T) {
	ctx := context.Background()
	captureWarns(t)
	r := NewResolver(newSeededStore(t))
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	five := r.Price(ctx, "claude_code", "claude-sonnet-5", rTime0, testTokens)
	fiveFive := r.Price(ctx, "claude_code", "claude-sonnet-5-5", rTime0, testTokens)
	dated := r.Price(ctx, "claude_code", "claude-sonnet-5-5-20261101", rTime0, testTokens)
	if fiveFive.Source != SourceStore || five.RateID == 0 || fiveFive.RateID == five.RateID {
		t.Errorf("sonnet-5-5 rate id %d vs sonnet-5 %d: want distinct rows", fiveFive.RateID, five.RateID)
	}
	if dated.RateID != fiveFive.RateID {
		t.Errorf("dated sonnet-5-5 id resolved row %d, want sonnet-5-5's row %d (longest prefix)", dated.RateID, fiveFive.RateID)
	}
	if !close12(fiveFive.CostUSD, five.CostUSD) {
		t.Errorf("sonnet-5-5 $%v != sonnet-5 $%v: the published rates are equal today", fiveFive.CostUSD, five.CostUSD)
	}
}
