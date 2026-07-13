package main

import (
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

func TestChooseContextWindow_NoExistingRow_UsesDefault(t *testing.T) {
	window, used, longCtx, fillPct, source := chooseContextWindow(gauge.GaugeRow{}, false, time.Now(), 50_000)
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic", source)
	}
	if window != defaultContextWindow {
		t.Fatalf("window = %d, want default %d", window, defaultContextWindow)
	}
	if used != 50_000 {
		t.Fatalf("used = %d, want 50000", used)
	}
	if longCtx {
		t.Fatal("longCtx should be false for the default window")
	}
	if fillPct != 50_000.0/float64(defaultContextWindow) {
		t.Fatalf("fillPct = %v, want %v", fillPct, 50_000.0/float64(defaultContextWindow))
	}
}

func TestChooseContextWindow_FreshStatusline_Preferred(t *testing.T) {
	reportedAt := time.Now().Add(-10 * time.Second)
	existing := gauge.GaugeRow{
		ContextWindowTokens: 1_000_000,
		ContextTokensUsed:   265_343,
		ContextFillPct:      0.265343,
		LongContextActive:   true,
		ContextSource:       gauge.ContextSourceStatusline,
		ContextReportedAt:   &reportedAt,
		UpdatedAt:           time.Now().Add(-10 * time.Second),
	}

	// heuristicUsed deliberately disagrees (999) to prove the fresh
	// statusline row wins, not just "happens to match".
	window, used, longCtx, fillPct, source := chooseContextWindow(existing, true, time.Now(), 999)
	if source != gauge.ContextSourceStatusline {
		t.Fatalf("source = %q, want statusline", source)
	}
	if window != 1_000_000 || used != 265_343 {
		t.Fatalf("window/used = %d/%d, want 1000000/265343 (from statusline row)", window, used)
	}
	if !longCtx {
		t.Fatal("expected longCtx=true carried from the statusline row")
	}
	if fillPct != 0.265343 {
		t.Fatalf("fillPct = %v, want 0.265343 (carried, not recomputed)", fillPct)
	}
}

func TestChooseContextWindow_StaleStatusline_FallsBackToDefault(t *testing.T) {
	existing := gauge.GaugeRow{
		ContextWindowTokens: 1_000_000,
		ContextTokensUsed:   500_000,
		ContextSource:       gauge.ContextSourceStatusline,
		UpdatedAt:           time.Now().Add(-2 * time.Minute), // older than statuslineStaleAfter (60s)
	}

	window, used, _, _, source := chooseContextWindow(existing, true, time.Now(), 10_000)
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic — stale statusline row must not win", source)
	}
	if window != defaultContextWindow || used != 10_000 {
		t.Fatalf("window/used = %d/%d, want default values %d/10000", window, used, defaultContextWindow)
	}
}

// TestChooseContextWindow_ExistingHeuristicRow_AlwaysRecomputesDefault is the
// regression for the "[1m]" model-name/host heuristic removal
// (contextWindowForModel/hostHasLongContext are gone — StatusLine is the
// only authoritative source now): a stale heuristic-sourced row's own
// window must never be carried forward, even if it happens to be the long
// window from a prior tick — every heuristic recompute lands flatly on
// defaultContextWindow, longCtx=false. chooseContextWindow is LEAD-only —
// a teammate never reaches this fallback; see teammateContextTracker in
// teammate_context.go for how its window is resolved instead.
func TestChooseContextWindow_ExistingHeuristicRow_AlwaysRecomputesDefault(t *testing.T) {
	existing := gauge.GaugeRow{
		ContextWindowTokens: 1_000_000, // stale value from a prior tick — must not be carried forward
		LongContextActive:   true,
		ContextSource:       gauge.ContextSourceHeuristic,
		UpdatedAt:           time.Now(),
	}

	window, used, longCtx, _, source := chooseContextWindow(existing, true, time.Now(), 10_000)
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic", source)
	}
	if window != defaultContextWindow || longCtx {
		t.Fatalf("window/longCtx = %d/%v, want default %d/false (no [1m] heuristic left to override it)", window, longCtx, defaultContextWindow)
	}
	if used != 10_000 {
		t.Fatalf("used = %d, want 10000 (freshly computed, not carried from the existing row)", used)
	}
}
