package main

import (
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

func TestChooseContextWindow_NoExistingRow_UnknownModel_UsesDefault(t *testing.T) {
	window, used, longCtx, fillPct, source := chooseContextWindow(gauge.GaugeRow{}, false, time.Now(), 50_000, "")
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic", source)
	}
	if window != defaultContextWindow {
		t.Fatalf("window = %d, want default %d", window, defaultContextWindow)
	}
	if used != 50_000 {
		t.Fatalf("used = %d, want 50000", used)
	}
	if !longCtx {
		t.Fatal("longCtx should be true — default is 1M")
	}
	if fillPct != 50_000.0/float64(defaultContextWindow) {
		t.Fatalf("fillPct = %v, want %v", fillPct, 50_000.0/float64(defaultContextWindow))
	}
}

func TestChooseContextWindow_NoExistingRow_KnownModel_UsesModelClass(t *testing.T) {
	window, used, longCtx, fillPct, source := chooseContextWindow(gauge.GaugeRow{}, false, time.Now(), 100_000, "claude-opus-4-6")
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic", source)
	}
	if window != 1_000_000 {
		t.Fatalf("window = %d, want 1000000 (from model class 'opus')", window)
	}
	if used != 100_000 {
		t.Fatalf("used = %d, want 100000", used)
	}
	if !longCtx {
		t.Fatal("longCtx should be true for a 1M model-class window")
	}
	if fillPct != 100_000.0/1_000_000.0 {
		t.Fatalf("fillPct = %v, want %v", fillPct, 100_000.0/1_000_000.0)
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
	window, used, longCtx, fillPct, source := chooseContextWindow(existing, true, time.Now(), 999, "claude-sonnet-5")
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

func TestChooseContextWindow_StaleStatusline_KnownModel_UsesModelClass(t *testing.T) {
	existing := gauge.GaugeRow{
		ContextWindowTokens: 1_000_000,
		ContextTokensUsed:   500_000,
		ContextSource:       gauge.ContextSourceStatusline,
		UpdatedAt:           time.Now().Add(-2 * time.Minute),
	}

	window, used, longCtx, _, source := chooseContextWindow(existing, true, time.Now(), 10_000, "claude-sonnet-5")
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic — stale statusline row must not win", source)
	}
	if window != 1_000_000 {
		t.Fatalf("window = %d, want 1000000 (from model class 'sonnet')", window)
	}
	if !longCtx {
		t.Fatal("longCtx should be true for 1M model-class window")
	}
	if used != 10_000 {
		t.Fatalf("used = %d, want 10000", used)
	}
}

func TestChooseContextWindow_StaleStatusline_UnknownModel_UsesDefault(t *testing.T) {
	existing := gauge.GaugeRow{
		ContextWindowTokens: 1_000_000,
		ContextTokensUsed:   500_000,
		ContextSource:       gauge.ContextSourceStatusline,
		UpdatedAt:           time.Now().Add(-2 * time.Minute),
	}

	window, used, longCtx, _, source := chooseContextWindow(existing, true, time.Now(), 10_000, "")
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic — stale statusline row must not win", source)
	}
	if window != defaultContextWindow || used != 10_000 {
		t.Fatalf("window/used = %d/%d, want default values %d/10000", window, used, defaultContextWindow)
	}
	if !longCtx {
		t.Fatal("longCtx should be true — default is 1M")
	}
}

func TestChooseContextWindow_ExistingHeuristicRow_KnownModel_UsesModelClass(t *testing.T) {
	existing := gauge.GaugeRow{
		ContextWindowTokens: 200_000,
		LongContextActive:   false,
		ContextSource:       gauge.ContextSourceHeuristic,
		UpdatedAt:           time.Now(),
	}

	window, used, longCtx, _, source := chooseContextWindow(existing, true, time.Now(), 10_000, "claude-fable-5")
	if source != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic", source)
	}
	if window != 1_000_000 {
		t.Fatalf("window = %d, want 1000000 (from model class 'fable')", window)
	}
	if !longCtx {
		t.Fatal("longCtx should be true for 1M model-class window")
	}
	if used != 10_000 {
		t.Fatalf("used = %d, want 10000", used)
	}
}

func TestChooseCodexContext_OldReport_Kept(t *testing.T) {
	reported := time.Now().Add(-3 * time.Hour)
	existing := gauge.GaugeRow{
		Runtime:             "codex",
		ContextSource:       gauge.ContextSourceCodexAppserver,
		ContextReportedAt:   &reported,
		ContextWindowTokens: 258_400,
		ContextTokensUsed:   64_600,
		ContextFillPct:      0.25,
		LongContextActive:   true,
	}
	window, used, longCtx, fillPct, source := chooseCodexContext(existing, true)
	if window != 258_400 || used != 64_600 || fillPct != 0.25 || !longCtx || source != gauge.ContextSourceCodexAppserver {
		t.Fatalf("got %d/%d/%v/%v/%q, want old codex report kept", window, used, longCtx, fillPct, source)
	}
}

func TestChooseCodexContext_NoReport_NotClaudeDefault(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing gauge.GaugeRow
		found    bool
	}{
		{"no row", gauge.GaugeRow{}, false},
		{"heuristic row", gauge.GaugeRow{ContextSource: gauge.ContextSourceHeuristic, ContextWindowTokens: 1_000_000}, true},
		{"claude statusline row", gauge.GaugeRow{ContextSource: gauge.ContextSourceStatusline, ContextWindowTokens: 1_000_000}, true},
	} {
		window, used, longCtx, fillPct, source := chooseCodexContext(tc.existing, tc.found)
		if window != 0 || used != 0 || longCtx || fillPct != 0 || source != gauge.ContextSourceHeuristic {
			t.Fatalf("%s: got %d/%d/%v/%v/%q, want 0/0/false/0/heuristic", tc.name, window, used, longCtx, fillPct, source)
		}
	}
}

func TestChooseContextWindow_StaleStatusline_NotAffectedByCodexSource(t *testing.T) {
	reported := time.Now().Add(-3 * time.Hour)
	existing := gauge.GaugeRow{
		ContextSource:       gauge.ContextSourceStatusline,
		ContextReportedAt:   &reported,
		ContextWindowTokens: 200_000,
		ContextTokensUsed:   10_000,
	}
	window, _, _, _, source := chooseContextWindow(existing, true, time.Now(), 50_000, "")
	if source != gauge.ContextSourceHeuristic || window != defaultContextWindow {
		t.Fatalf("got %d/%q, want heuristic default for stale Claude statusline", window, source)
	}
}
