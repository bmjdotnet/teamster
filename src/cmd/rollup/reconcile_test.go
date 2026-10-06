package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/reconciler"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

type fakeRates []store.ModelRate

func (f fakeRates) ListRates(context.Context, store.RateFilter) ([]store.ModelRate, error) {
	return f, nil
}

func testRate(variant string, in, out, cr, cw5, cw1 float64) store.ModelRate {
	return store.ModelRate{
		ID: 1, Runtime: store.RateRuntimeClaudeCode, MatchKind: store.RateMatchPrefix, ModelKey: "claude-opus-4-8",
		Variant: variant, InputPerMtok: in, OutputPerMtok: out, CacheReadPerMtok: cr,
		CacheWrite5mPerMtok: cw5, CacheWrite1hPerMtok: cw1, ValidFrom: store.SeedRateValidFrom,
	}
}

func newTestRater(t *testing.T, rates fakeRates) *baseRaterAdapter {
	t.Helper()
	res := pricing.NewResolver(rates)
	if err := res.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &baseRaterAdapter{ctx: context.Background(), resolver: res}
}

func TestBaseRaterPricesBaseNotPremium(t *testing.T) {
	a := newTestRater(t, fakeRates{
		testRate(store.RateVariantBase, 5, 25, 0.5, 6.25, 10),
		testRate(store.RateVariant1M, 10, 37.5, 1, 12.5, 20),
	})
	tok := reconciler.TokenCounts{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheWrite: 2e6, CacheWrite5m: 1e6, CacheWrite1h: 1e6}
	const want = 5 + 25 + 0.5 + 6.25 + 10

	for _, model := range []string{"claude-opus-4-8", "claude-opus-4-8[1m]"} {
		got, ok := a.BaseRateCost(model, tok)
		if !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("BaseRateCost(%q) = %v, %v; want %v, true", model, got, ok, want)
		}
	}
}

func TestBaseRaterPricesUnsplitCacheWriteAt5mRate(t *testing.T) {
	a := newTestRater(t, fakeRates{testRate(store.RateVariantBase, 5, 25, 0.5, 6.25, 10)})

	// 3M cache-write tokens, only 1M+1M accounted by the TTL split: the 1M
	// remainder is a pre-split row and must cost the 5m rate.
	got, ok := a.BaseRateCost("claude-opus-4-8", reconciler.TokenCounts{CacheWrite: 3e6, CacheWrite5m: 1e6, CacheWrite1h: 1e6})
	const want = 2*6.25 + 10
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v, %v; want %v, true", got, ok, want)
	}

	// A split that already exceeds the total must not go negative.
	got, _ = a.BaseRateCost("claude-opus-4-8", reconciler.TokenCounts{CacheWrite: 1e6, CacheWrite5m: 1e6, CacheWrite1h: 1e6})
	if want := 6.25 + 10.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("over-split got %v, want %v", got, want)
	}
}

func TestBaseRaterUnknownModelIsNotOK(t *testing.T) {
	a := newTestRater(t, fakeRates{testRate(store.RateVariantBase, 5, 25, 0.5, 6.25, 10)})
	if got, ok := a.BaseRateCost("claude-mystery-1", reconciler.TokenCounts{Input: 1e6}); ok || got != 0 {
		t.Errorf("unknown model = %v, %v; want 0, false", got, ok)
	}
}

type fakeProm map[string]reconciler.VendorSession

func (f fakeProm) ReadSessions(_ context.Context, ids []string, _ time.Time, _ time.Duration) (map[string]reconciler.VendorSession, error) {
	out := map[string]reconciler.VendorSession{}
	for _, id := range ids {
		if v, ok := f[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

type failingProm struct{}

func (failingProm) ReadSessions(context.Context, []string, time.Time, time.Duration) (map[string]reconciler.VendorSession, error) {
	return nil, errors.New("prometheus down")
}

func seedVerifyStore(t *testing.T) (*sqlite.Store, fakeProm) {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Opus 4.8 at base rates: 1M input $5, 100k output $2.50, 10M cache read
	// $5, 1M cache write $6.25 = $18.75.
	const baseUSD = 18.75
	old := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	row := func(session string) store.TelemetryRow {
		return store.TelemetryRow{
			SessionID: session, MessageID: session + "-m1", Model: "claude-opus-4-8", Timestamp: old,
			InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 10_000_000,
			CacheWriteTokens: 1_000_000, CacheWrite5m: 1_000_000, CostUSD: baseUSD,
		}
	}
	if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{row("honest"), row("premium"), row("dark")}); err != nil {
		t.Fatal(err)
	}

	vendor := func(label string, usd float64) reconciler.VendorSession {
		return reconciler.VendorSession{
			Models: map[string]reconciler.VendorModel{label: {
				Tokens:  reconciler.TokenCounts{Input: 1_000_000, Output: 100_000, CacheRead: 10_000_000, CacheWrite: 1_000_000},
				CostUSD: usd,
			}},
			TokensStableSince: old,
		}
	}
	return s, fakeProm{
		"honest":  vendor("claude-opus-4-8", baseUSD),
		"premium": vendor("claude-opus-4-8[1m]", 30),
	}
}

func verdictsOf(t *testing.T, s *sqlite.Store) map[string]string {
	t.Helper()
	rows, err := s.ListVerifications(context.Background(), "claude_code", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.SessionID] = r.Verdict
	}
	return out
}

func TestVerifyCostsStoresVerdictsAndFiresPremiumTripwire(t *testing.T) {
	s, prom := seedVerifyStore(t)
	var logs bytes.Buffer
	verifyCosts(context.Background(), s, prom, false, slog.New(slog.NewTextHandler(&logs, nil)))

	got := verdictsOf(t, s)
	want := map[string]string{"honest": "within_tolerance", "premium": "premium_pricing", "dark": "unverifiable"}
	if len(got) != len(want) {
		t.Fatalf("verdicts = %v, want %v", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("%s verdict = %q, want %q (all: %v)", id, got[id], v, got)
		}
	}
	if !strings.Contains(logs.String(), "cost alert") || !strings.Contains(logs.String(), "session_id=premium") {
		t.Errorf("premium alert not logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "verification complete") {
		t.Errorf("summary not logged:\n%s", logs.String())
	}
}

func TestVerifyCostsDryRunNilAndFailureWriteNothing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	s, prom := seedVerifyStore(t)
	verifyCosts(context.Background(), s, prom, true, logger)
	if got := verdictsOf(t, s); len(got) != 0 {
		t.Errorf("dry-run wrote %v", got)
	}

	verifyCosts(context.Background(), s, nil, false, logger)
	if got := verdictsOf(t, s); len(got) != 0 {
		t.Errorf("nil reader wrote %v", got)
	}

	var logs bytes.Buffer
	verifyCosts(context.Background(), s, failingProm{}, false, slog.New(slog.NewTextHandler(&logs, nil)))
	if got := verdictsOf(t, s); len(got) != 0 {
		t.Errorf("failed pass wrote %v", got)
	}
	if !strings.Contains(logs.String(), "verification failed") || !strings.Contains(logs.String(), "prometheus down") {
		t.Errorf("failure not logged:\n%s", logs.String())
	}
}
