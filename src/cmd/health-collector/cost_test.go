package main

import (
	"math"
	"testing"
)

const costEpsilon = 1e-9

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

	got1 := costForRows([]ledgerRow{withSmallTotal})
	got2 := costForRows([]ledgerRow{withHugeTotal})
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
	got := costForRows([]ledgerRow{row})
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
	got := costForRows(rows)
	want := 0.005 + 0.003
	if math.Abs(got-want) > costEpsilon {
		t.Errorf("costForRows = %v, want %v (sum of both rows, each at its own model's rate)", got, want)
	}
}

// TestCostForRows_EmptyRows covers the zero-new-rows tick (no new
// token_ledger data since the last poll) — must return 0, not panic.
func TestCostForRows_EmptyRows(t *testing.T) {
	if got := costForRows(nil); got != 0 {
		t.Errorf("costForRows(nil) = %v, want 0", got)
	}
}
