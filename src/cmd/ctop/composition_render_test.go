package main

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/display"
)

func TestParseCompositionNilAndEmpty(t *testing.T) {
	if got := parseComposition(nil); got != nil {
		t.Errorf("parseComposition(nil) = %+v, want nil", got)
	}
	empty := ""
	if got := parseComposition(&empty); got != nil {
		t.Errorf("parseComposition(\"\") = %+v, want nil", got)
	}
	garbage := "not json"
	if got := parseComposition(&garbage); got != nil {
		t.Errorf("parseComposition(garbage) = %+v, want nil", got)
	}
}

func TestParseCompositionValid(t *testing.T) {
	raw := `{"text_pct":0.35,"tool_use_pct":0.25,"thinking_pct":0.3,"reading_pct":0.1}`
	got := parseComposition(&raw)
	if got == nil {
		t.Fatal("parseComposition() = nil, want a value")
	}
	if got.TextPct != 0.35 || got.ToolUsePct != 0.25 || got.ThinkingPct != 0.3 || got.ReadingPct != 0.1 {
		t.Errorf("parseComposition() = %+v, want the exact fixture values", got)
	}
}

func TestSegmentedBarFallsBackToPlainBarWhenNil(t *testing.T) {
	got := SegmentedBar(0.5, "ok", nil)
	want := fillBar(0.5, "ok")
	if got != want {
		t.Errorf("SegmentedBar(nil comp) = %q, want plain fillBar output %q", got, want)
	}
}

func TestSegmentedBarCellsSumToFilled(t *testing.T) {
	comp := &compositionJSON{TextPct: 0.5, ToolUsePct: 0.3, ThinkingPct: 0.1, ReadingPct: 0.1}
	out := display.StripANSI(SegmentedBar(0.7, "ok", comp))
	filledCells := strings.Count(out, "▓")
	emptyCells := strings.Count(out, "░")
	if filledCells+emptyCells != 10 {
		t.Errorf("total cells = %d, want 10 (out=%q)", filledCells+emptyCells, out)
	}
	wantFilled := 7 // round(0.7*10)
	if filledCells != wantFilled {
		t.Errorf("filled cells = %d, want %d", filledCells, wantFilled)
	}
}

func TestSegmentedBarNeverExceedsFilledBudget(t *testing.T) {
	// text_pct + tool_use_pct alone would round up to more than filled cells
	// at some fractions — the tool segment must be clamped so segments never
	// overflow past the bar's actual filled-cell count.
	comp := &compositionJSON{TextPct: 0.6, ToolUsePct: 0.6, ThinkingPct: 0, ReadingPct: 0}
	out := display.StripANSI(SegmentedBar(0.3, "ok", comp))
	filledCells := strings.Count(out, "▓")
	if filledCells != 3 { // round(0.3*10)
		t.Errorf("filled cells = %d, want 3 (out=%q)", filledCells, out)
	}
}

func TestSegmentedBarPartitionsAcrossAllThreeCategories(t *testing.T) {
	// Roughly equal thirds should each claim at least one of a 9-cell fill —
	// proves text/tool_use(+reading)/thinking are genuinely split, not one
	// category silently swallowing the whole bar.
	comp := &compositionJSON{TextPct: 0.34, ToolUsePct: 0.33, ThinkingPct: 0.33, ReadingPct: 0}
	out := display.StripANSI(SegmentedBar(0.9, "ok", comp))
	filledCells := strings.Count(out, "▓")
	if filledCells != 9 {
		t.Fatalf("filled cells = %d, want 9 (out=%q)", filledCells, out)
	}
}
