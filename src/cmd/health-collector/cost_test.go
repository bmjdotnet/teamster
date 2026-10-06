package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/store"
)

const costEpsilon = 1e-9

type fakeRates []store.ModelRate

func (f fakeRates) ListRates(context.Context, store.RateFilter) ([]store.ModelRate, error) {
	return f, nil
}

func newTestResolver(t *testing.T, rates []store.ModelRate) *pricing.Resolver {
	t.Helper()
	res := pricing.NewResolver(fakeRates(rates))
	if err := res.Refresh(context.Background()); err != nil {
		t.Fatalf("resolver refresh: %v", err)
	}
	return res
}

func seedResolver(t *testing.T) *pricing.Resolver {
	t.Helper()
	return newTestResolver(t, store.ModelPricingSeedV1())
}

// TestCostForRows_UsesComponentColumnsNotTotalInput is the regression for
// the per-agent cost fix: cost must be derived from the component token
// columns (input/output/cache_read/cache_write), never total_input — that
// column is occupancy-shaped (context-window fill), not cost-shaped. Two
// rows with identical component columns but wildly different TotalInput
// values must price identically.
func TestCostForRows_UsesComponentColumnsNotTotalInput(t *testing.T) {
	base := ledgerRow{
		Model:           "claude-opus-4-6",
		InputTokens:     1000,
		OutputTokens:    500,
		CacheReadTokens: 2000,
		CacheWrite5m:    100,
	}
	withSmallTotal := base
	withSmallTotal.TotalInput = 1
	withHugeTotal := base
	withHugeTotal.TotalInput = 999_999_999

	got1 := costForRows(context.Background(), seedResolver(t), []ledgerRow{withSmallTotal})
	got2 := costForRows(context.Background(), seedResolver(t), []ledgerRow{withHugeTotal})
	if got1 != got2 {
		t.Errorf("cost differs by TotalInput alone: %v vs %v, want equal (TotalInput must be ignored)", got1, got2)
	}

	// 1000*$5/Mtok + 500*$25/Mtok + 2000*$0.5/Mtok + 100*$6.25/Mtok(5m tier)
	want := 1000*0.000005 + 500*0.000025 + 2000*0.0000005 + 100*0.00000625
	if math.Abs(got1-want) > costEpsilon {
		t.Errorf("costForRows = %v, want %v", got1, want)
	}
}

// TestCostForRows_CacheWrite1hPricedSeparately is the WP1 regression at the
// ledgerRow/costForRows layer (see internal/pricing's own TestComputeCost
// CacheWrite1hTier for the ComputeCost-level coverage): the 1h cache-write
// bucket must price at its own (higher) rate, not fall back to the 5m rate
// or get dropped.
func TestCostForRows_CacheWrite1hPricedSeparately(t *testing.T) {
	row := ledgerRow{
		Model:        "claude-opus-4-6",
		CacheWrite5m: 100,
		CacheWrite1h: 100,
	}
	got := costForRows(context.Background(), seedResolver(t), []ledgerRow{row})
	// 100*$6.25/Mtok(5m) + 100*$10/Mtok(1h)
	want := 100*0.00000625 + 100*0.00001
	if math.Abs(got-want) > costEpsilon {
		t.Errorf("costForRows = %v, want %v (5m and 1h buckets priced independently)", got, want)
	}
}

// TestCostForRows_SumsAcrossRowsUsingEachRowsOwnModel covers the two other
// requirements: rows sum (not just the last one), and pricing uses each
// row's OWN model (the real API model ID token-scraper recorded), not a
// single session-wide model — a mid-session model change (rare but
// possible) must price each row at its own rate.
func TestCostForRows_SumsAcrossRowsUsingEachRowsOwnModel(t *testing.T) {
	rows := []ledgerRow{
		{Model: "claude-opus-4-6", InputTokens: 1000},   // 1000 * 0.000005 = 0.005
		{Model: "claude-sonnet-4-5", InputTokens: 1000}, // 1000 * 0.000003 = 0.003
	}
	got := costForRows(context.Background(), seedResolver(t), rows)
	want := 0.005 + 0.003
	if math.Abs(got-want) > costEpsilon {
		t.Errorf("costForRows = %v, want %v (sum of both rows, each at its own model's rate)", got, want)
	}
}

// TestCostForRows_EmptyRows covers the zero-new-rows tick (no new
// token_ledger data since the last poll) — must return 0, not panic.
func TestCostForRows_EmptyRows(t *testing.T) {
	if got := costForRows(context.Background(), seedResolver(t), nil); got != 0 {
		t.Errorf("costForRows(context.Background(), seedResolver(t), nil) = %v, want 0", got)
	}
}

// TestCostForRows_PricesAtRowTimestamp pins the temporal-rate wiring: each row
// is priced at the rate in effect at its own timestamp, so a rate change
// affects only rows after it and history is not repriced.
func TestCostForRows_PricesAtRowTimestamp(t *testing.T) {
	change := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rate := func(id int64, in float64, from time.Time, to *time.Time) store.ModelRate {
		return store.ModelRate{
			ID: id, Runtime: store.RateRuntimeClaudeCode, MatchKind: store.RateMatchPrefix, ModelKey: "claude-opus-4-6",
			Variant: store.RateVariantBase, InputPerMtok: in, ValidFrom: from, ValidTo: to,
		}
	}
	res := newTestResolver(t, []store.ModelRate{
		rate(1, 5, store.SeedRateValidFrom, &change),
		rate(2, 10, change, nil),
	})

	before := ledgerRow{Model: "claude-opus-4-6", InputTokens: 1_000_000, Timestamp: change.Add(-time.Hour)}
	after := ledgerRow{Model: "claude-opus-4-6", InputTokens: 1_000_000, Timestamp: change.Add(time.Hour)}

	if got := costForRows(context.Background(), res, []ledgerRow{before}); math.Abs(got-5) > costEpsilon {
		t.Errorf("row before the rate change = %v, want 5", got)
	}
	if got := costForRows(context.Background(), res, []ledgerRow{after}); math.Abs(got-10) > costEpsilon {
		t.Errorf("row after the rate change = %v, want 10", got)
	}
}

// TestCostForRows_UnknownModelCostsZeroAndIsCounted pins the loud-failure
// path: a model with no rate row prices at $0 and shows up in the resolver's
// unknown-model counter instead of silently inheriting some other rate.
func TestCostForRows_UnknownModelCostsZeroAndIsCounted(t *testing.T) {
	res := seedResolver(t)
	got := costForRows(context.Background(), res, []ledgerRow{{Model: "mystery-model-1", InputTokens: 1_000_000}})
	if got != 0 {
		t.Errorf("unknown model cost = %v, want 0", got)
	}
	if n := res.Stats().UnknownModels["mystery-model-1"]; n != 1 {
		t.Errorf("unknown-model count = %d, want 1", n)
	}
}

// TestCostForRows_PricesEachRowUnderItsOwnRuntime is the regression for the
// Codex $0 bug: a Codex model has rate rows only under runtime=codex, so a row
// must price under its own ledger runtime, not a fixed one.
func TestCostForRows_PricesEachRowUnderItsOwnRuntime(t *testing.T) {
	rows := []ledgerRow{
		{Runtime: store.RateRuntimeClaudeCode, Model: "claude-opus-4-6", InputTokens: 1_000_000}, // $5/Mtok
		{Runtime: store.RateRuntimeCodex, Model: "gpt-6.1-sol", InputTokens: 1_000_000},          // $2/Mtok
	}
	got := costForRows(context.Background(), seedResolver(t), rows)
	if want := 5.0 + 2.0; math.Abs(got-want) > costEpsilon {
		t.Errorf("costForRows = %v, want %v (each row under its own runtime)", got, want)
	}
}
