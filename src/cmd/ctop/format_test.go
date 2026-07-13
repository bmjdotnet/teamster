package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/tui"
)

func TestFillBarClampAndCells(t *testing.T) {
	tests := []struct {
		name   string
		pct    float64
		filled int
	}{
		{"zero", 0, 0},
		{"half", 0.5, 5},
		{"full", 1.0, 10},
		{"negative clamps", -0.2, 0},
		{"rounds up", 0.86, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := display.StripANSI(fillBar(tt.pct, "ok"))
			gotFilled := strings.Count(out, "▓")
			if gotFilled != tt.filled {
				t.Errorf("fillBar(%v) filled cells = %d, want %d (out=%q)", tt.pct, gotFilled, tt.filled, out)
			}
			if strings.Count(out, "▓")+strings.Count(out, "░") != 10 {
				t.Errorf("fillBar(%v) total cells != 10: %q", tt.pct, out)
			}
		})
	}
}

func TestFillBarLabelPercent(t *testing.T) {
	out := display.StripANSI(fillBar(0.523, "ok"))
	if !strings.Contains(out, "52%") {
		t.Errorf("fillBar(0.523) = %q, want it to contain 52%%", out)
	}
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "all"},
		{-time.Minute, "all"},
		{time.Hour, "1h"},
		{6 * time.Hour, "6h"},
		{24 * time.Hour, "24h"},
		{15 * time.Minute, "15m"},
		{90 * time.Minute, "90m"},
	}
	for _, tt := range tests {
		if got := formatAge(tt.d); got != tt.want {
			t.Errorf("formatAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFillBarShowsDashesForUnreliableOverOneFill(t *testing.T) {
	// Historical gauge rows computed against a since-corrected (smaller)
	// context window can carry context_fill_pct well over 1.0 (up to ~4.12
	// observed). Clamping the display to 100% would still assert a specific,
	// wrong number — "--" signals the data isn't trustworthy instead.
	for _, pct := range []float64{1.0001, 1.4, 3.04, 4.12} {
		out := display.StripANSI(fillBar(pct, "ok"))
		if out != "--" {
			t.Errorf("fillBar(%v) = %q, want \"--\" (no bar, no percentage)", pct, out)
		}
	}
}

func TestSegmentedBarShowsDashesForUnreliableOverOneFill(t *testing.T) {
	comp := &compositionJSON{TextPct: 0.5, ToolUsePct: 0.3, ThinkingPct: 0.2}
	out := display.StripANSI(SegmentedBar(3.04, "ok", comp))
	if out != "--" {
		t.Errorf("SegmentedBar(3.04, comp) = %q, want \"--\"", out)
	}
}

func TestGridSegmentedBarCellsAndUnreliableFill(t *testing.T) {
	out := display.StripANSI(gridSegmentedBar(0.5, "ok", nil, dimNone))
	filled := strings.Count(out, "▓")
	if filled != 3 { // round(0.5*6)
		t.Errorf("gridSegmentedBar(0.5) filled = %d, want 3 (out=%q)", filled, out)
	}
	if filled+strings.Count(out, "░") != 6 {
		t.Errorf("gridSegmentedBar(0.5) total cells != 6: %q", out)
	}
	if out := display.StripANSI(gridSegmentedBar(1.4, "ok", nil, dimNone)); out != "--" {
		t.Errorf("gridSegmentedBar(1.4) = %q, want \"--\" (unreliable fill)", out)
	}
}

func TestGridSegmentedBarWithCompositionSumsToFilled(t *testing.T) {
	comp := &compositionJSON{TextPct: 0.5, ToolUsePct: 0.3, ThinkingPct: 0.1, ReadingPct: 0.1}
	out := display.StripANSI(gridSegmentedBar(0.5, "ok", comp, dimNone))
	filled := strings.Count(out, "▓")
	if filled != 3 { // round(0.5*6)
		t.Errorf("gridSegmentedBar(0.5, comp) filled = %d, want 3 (out=%q)", filled, out)
	}
}

// TestGridSegmentedBarHalveKeepsCellCountAndAppliesHalvedColor is the core
// regression for gridSegmentedBar's "inactive" row dimming: the visible
// text (cell counts, label) must be unchanged, but the bar and label colors
// must come through halved, not full-brightness.
func TestGridSegmentedBarHalveKeepsCellCountAndAppliesHalvedColor(t *testing.T) {
	plain := display.StripANSI(gridSegmentedBar(0.5, "ok", nil, dimNone))
	halved := display.StripANSI(gridSegmentedBar(0.5, "ok", nil, dimHalve))
	if plain != halved {
		t.Errorf("gridSegmentedBar(dimHalve) changed the visible text: plain=%q halved=%q", plain, halved)
	}

	got := gridSegmentedBar(0.5, "ok", nil, dimHalve)
	hb := halveRGB(pressureRGB("ok"))
	wantBarEsc := display.RGB(hb[0], hb[1], hb[2])
	if !strings.Contains(got, wantBarEsc) {
		t.Errorf("gridSegmentedBar(dimHalve) = %q, want the halved bar color escape %q", got, wantBarEsc)
	}

	hl := halveRGB(ctxPctRGB(0.5))
	wantLabelEsc := display.RGB(hl[0], hl[1], hl[2])
	if !strings.Contains(got, wantLabelEsc) {
		t.Errorf("gridSegmentedBar(dimHalve) = %q, want the halved label color escape %q", got, wantLabelEsc)
	}
}

// TestGridSegmentedBarHalveWithCompositionPreservesSegmentHues checks the
// composition-colored path (compositionBarHalved): each segment keeps its
// own hue, halved, rather than collapsing to one flat color.
func TestGridSegmentedBarHalveWithCompositionPreservesSegmentHues(t *testing.T) {
	comp := &compositionJSON{TextPct: 0.5, ToolUsePct: 0.3, ThinkingPct: 0.2}
	got := gridSegmentedBar(0.5, "ok", comp, dimHalve)
	ht := halveRGB(accentRGB)
	wantEsc := display.RGB(ht[0], ht[1], ht[2])
	if !strings.Contains(got, wantEsc) {
		t.Errorf("gridSegmentedBar(dimHalve, comp) = %q, want halved tool-segment color %q", got, wantEsc)
	}
}

// ctxPctColor is deliberately client-side and independent of pressure_level
// (see gridSegmentedBar's doc comment) — a quick-glance fillPct gradient,
// not a substitute for the server's hysteresis-gated pressure badge.
func TestCtxPctColorGradient(t *testing.T) {
	tests := []struct {
		fillPct float64
		want    lipgloss.Color
	}{
		{0, tui.ColorSuccess},
		{0.39, tui.ColorSuccess},
		{0.40, lipgloss.Color("#e3b341")},
		{0.64, lipgloss.Color("#e3b341")},
		{0.65, tui.ColorWarn},
		{0.79, tui.ColorWarn},
		{0.80, tui.ColorError},
		{1.0, tui.ColorError},
	}
	for _, tt := range tests {
		if got := ctxPctColor(tt.fillPct); got != tt.want {
			t.Errorf("ctxPctColor(%v) = %v, want %v", tt.fillPct, got, tt.want)
		}
	}
}

func TestFillBarColorKeyedOnlyOnPressureLevel(t *testing.T) {
	// A fill of 0.76 (above the 0.75 warning threshold) must still render as
	// "ok" colored if pressure_level says ok — the server's hysteresis, not a
	// client-side recompute, is authoritative (design D6). Compare the
	// resolved style's foreground directly rather than rendered ANSI text:
	// lipgloss disables styling when output isn't a TTY (as under `go test`),
	// so rendered strings for different styles can come out byte-identical
	// even though the underlying styles differ.
	ok := pressureColor("ok").GetForeground()
	warn := pressureColor("warning").GetForeground()
	crit := pressureColor("critical").GetForeground()
	if ok == warn || ok == crit || warn == crit {
		t.Errorf("expected distinct foreground colors per pressure level, got ok=%v warn=%v crit=%v", ok, warn, crit)
	}
}

func TestPressureBadgeText(t *testing.T) {
	tests := map[string]string{
		"ok":       "ok",
		"warning":  "WARN",
		"critical": "CRIT",
	}
	for level, want := range tests {
		got := display.StripANSI(pressureBadge(level))
		if got != want {
			t.Errorf("pressureBadge(%q) = %q, want %q", level, got, want)
		}
	}
}

func TestLivenessBadgeText(t *testing.T) {
	tests := map[string]string{
		"live":    "● live",
		"idle":    "◌ idle",
		"stale":   "○ stale",
		"closed":  "✕ closed",
		"unbound": "? unbnd",
		"":        "—",
	}
	for liveness, want := range tests {
		got := display.StripANSI(livenessBadge(liveness))
		if got != want {
			t.Errorf("livenessBadge(%q) = %q, want %q", liveness, got, want)
		}
	}
}

func TestHumanizeTokens(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{1200, "1k"},
		{38000, "38k"},
		{1_000_000, "1.0M"},
		{1_200_000, "1.2M"},
	}
	for _, tt := range tests {
		if got := humanizeTokens(tt.n); got != tt.want {
			t.Errorf("humanizeTokens(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFmtCost(t *testing.T) {
	tests := []struct {
		usd  float64
		want string
	}{
		{0, "$0"},
		{1.23, "$1"},
		{0.12, "$0"},
		{123.45, "$123"},
		{12.3456, "$12"},
		{0.5, "$1"}, // rounds, doesn't floor
		{455.9, "$456"},
		{9999.4, "$9999"},
	}
	for _, tt := range tests {
		if got := fmtCost(tt.usd); got != tt.want {
			t.Errorf("fmtCost(%v) = %q, want %q", tt.usd, got, tt.want)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero time is dash", time.Time{}, "—"},
		{"seconds", now.Add(-3 * time.Second), "3s"},
		{"minutes", now.Add(-11 * time.Minute), "11m"},
		{"hours", now.Add(-2 * time.Hour), "2h"},
		{"days", now.Add(-3 * 24 * time.Hour), "3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relativeTime(tt.t); got != tt.want {
				t.Errorf("relativeTime(%v) = %q, want %q", tt.t, got, tt.want)
			}
		})
	}
}

func TestParseRFC3339(t *testing.T) {
	if !parseRFC3339("").IsZero() {
		t.Error("parseRFC3339(\"\") should be zero time")
	}
	if !parseRFC3339("not-a-time").IsZero() {
		t.Error("parseRFC3339(garbage) should be zero time")
	}
	ts, err := time.Parse(time.RFC3339, "2026-07-11T15:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	got := parseRFC3339("2026-07-11T15:04:05Z")
	if !got.Equal(ts) {
		t.Errorf("parseRFC3339 = %v, want %v", got, ts)
	}
}

func TestModelShortPreservesLongContextSuffix(t *testing.T) {
	tests := map[string]string{
		"claude-opus-4-6":    "opus-4-6",
		"claude-fable-5[1m]": "fable-5[1m]",
		"claude-sonnet-4-5":  "sonnet-4-5",
		"gpt-5.1":            "gpt-5.1", // no claude- prefix, untouched
	}
	for in, want := range tests {
		if got := modelShort(in); got != want {
			t.Errorf("modelShort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelAbbrev(t *testing.T) {
	tests := map[string]string{
		"claude-opus-4-6":       "o4.6",
		"claude-sonnet-5":       "s5",
		"claude-fable-5":        "f5",
		"claude-haiku-4-5":      "h4.5",
		"claude-opus-4-6[1m]":   "o4.6[1m]",
		"claude-sonnet-5[1m]":   "s5[1m]",
		"gpt-5.1":               "gpt-5.1", // unrecognized family, untouched
		"claude-unknown-family": "claude-unknown-family",
	}
	for in, want := range tests {
		if got := modelAbbrev(in); got != want {
			t.Errorf("modelAbbrev(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPadRightRightAlignsWithinWidth(t *testing.T) {
	got := padRight("42", 6)
	if got != "    42" {
		t.Errorf("padRight(%q, 6) = %q, want %q", "42", got, "    42")
	}
}

func TestPadRightTruncatesLongStrings(t *testing.T) {
	got := padRight("1234567", 5)
	if lipglossVisibleLen(got) != 5 {
		t.Errorf("padRight(\"1234567\", 5) visible len = %d, want 5 (out=%q)", lipglossVisibleLen(got), got)
	}
}

func TestPadRightNoTruncPadsShortStrings(t *testing.T) {
	if got := padRightNoTrunc("$5", 5); got != "   $5" {
		t.Errorf("padRightNoTrunc(\"$5\", 5) = %q, want %q", got, "   $5")
	}
}

func TestPadRightNoTruncNeverTruncatesOverlongStrings(t *testing.T) {
	// COST must overflow rather than lose digits — unlike padRight, which
	// truncates.
	got := padRightNoTrunc("$123456", 5)
	if got != "$123456" {
		t.Errorf("padRightNoTrunc(\"$123456\", 5) = %q, want it unchanged (no truncation)", got)
	}
}

func TestSessionAlias(t *testing.T) {
	if got := sessionAlias("wms-build", "225102e768b6-rest", false, ""); got != "#wms-build" {
		t.Errorf("sessionAlias(team known) = %q, want %q", got, "#wms-build")
	}
	if got := sessionAlias("", "225102e768b6-rest", false, ""); got != "solo·225102e7" {
		t.Errorf("sessionAlias(no team, solo) = %q, want %q", got, "solo·225102e7")
	}
	if got := sessionAlias("", "225102e768b6-rest", true, ""); got != "?·225102e7" {
		t.Errorf("sessionAlias(no team, multi-agent, no focus) = %q, want %q", got, "?·225102e7")
	}
	if got := sessionAlias("wms-build", "225102e768b6-rest", true, "agent-health-diet"); got != "#wms-build" {
		t.Errorf("sessionAlias(team known, focus set) = %q, want team to win over focus, got %q", got, got)
	}
	if got := sessionAlias("", "225102e768b6-rest", true, "agent-health-diet"); got != "agent-healt…" {
		t.Errorf("sessionAlias(no team, multi-agent, focus set) = %q, want truncated focus %q", got, "agent-healt…")
	}
	if got := sessionAlias("", "225102e768b6-rest", false, "agent-health-diet"); got != "solo·225102e7" {
		t.Errorf("sessionAlias(no team, solo, focus set) = %q, want focus ignored for solo sessions, got %q", got, got)
	}
}

// TestStatusDotsSpacesTheTwoGlyphs is the regression for the ST column's
// glyph-spacing polish: jammed-together glyphs were hard to read, so the
// health dot and runtime glyph get a space between them — making the
// column 3 visible cells, not 2. Closed now renders a hollow "○" (not the
// old "✕") — see healthDot's doc comment.
func TestStatusDotsSpacesTheTwoGlyphs(t *testing.T) {
	now := time.Now()
	got := display.StripANSI(statusDots("closed", "claude_code", "ok", false, false, now))
	if got != "○ ✦" {
		t.Errorf("statusDots(closed, claude_code) = %q, want %q", got, "○ ✦")
	}
	if w := lipglossVisibleLen(statusDots("live", "codex", "ok", false, false, now)); w != 3 {
		t.Errorf("statusDots visible width = %d, want 3", w)
	}
}

// TestHealthDotStyleKeyedOnlyOnPressureLevel mirrors
// TestFillBarColorKeyedOnlyOnPressureLevel's technique: compare the
// resolved style's foreground directly, since lipgloss disables styling
// outside a real TTY (as under `go test`), so rendered strings for
// different styles can come out byte-identical even though the underlying
// styles differ.
func TestHealthDotStyleKeyedOnlyOnPressureLevel(t *testing.T) {
	ok := healthDotStyle("ok").GetForeground()
	warn := healthDotStyle("warning").GetForeground()
	crit := healthDotStyle("critical").GetForeground()
	if ok == warn || ok == crit || warn == crit {
		t.Errorf("expected distinct foreground colors per pressure level, got ok=%v warn=%v crit=%v", ok, warn, crit)
	}
	if got := healthDotStyle("critical").GetForeground(); got != tui.ColorError {
		t.Errorf("healthDotStyle(critical) foreground = %v, want tui.ColorError", got)
	}
	if got := healthDotStyle("warning").GetForeground(); got != tui.ColorWarn {
		t.Errorf("healthDotStyle(warning) foreground = %v, want tui.ColorWarn", got)
	}
	if got := healthDotStyle("ok").GetForeground(); got != tui.ColorSuccess {
		t.Errorf("healthDotStyle(ok) foreground = %v, want tui.ColorSuccess", got)
	}
}

// TestHealthDotGlyphShapes covers item 1's full state table: glyph SHAPE is
// TTY-independent (unlike color, see TestHealthDotStyleKeyedOnlyOnPressureLevel),
// so these compare display.StripANSI'd output directly.
func TestHealthDotGlyphShapes(t *testing.T) {
	even := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	odd := time.Date(2024, 1, 1, 0, 0, 11, 0, time.UTC)

	tests := []struct {
		name               string
		liveness, pressure string
		inactive, midTurn  bool
		now                time.Time
		want               string
	}{
		{"ok, alive, active", "live", "ok", false, false, even, "◉"},
		{"warning, alive, active", "live", "warning", false, false, even, "◉"},
		{"critical, alive, active, not midturn", "idle", "critical", false, false, even, "◉"},
		{"critical, midturn, even second", "live", "critical", false, true, even, "◉"},
		{"critical, midturn, odd second", "live", "critical", false, true, odd, "○"},
		{"inactive overrides pressure and midturn", "idle", "critical", true, true, even, "○"},
		{"closed", "closed", "critical", false, true, even, "○"},
		{"stale liveness falls back to dim hollow", "stale", "ok", false, false, even, "○"},
		{"unbound liveness falls back to dim hollow", "unbound", "ok", false, false, even, "○"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := display.StripANSI(healthDot(tt.liveness, tt.pressure, tt.inactive, tt.midTurn, tt.now))
			if got != tt.want {
				t.Errorf("healthDot(%q, %q, inactive=%v, midTurn=%v) = %q, want %q", tt.liveness, tt.pressure, tt.inactive, tt.midTurn, got, tt.want)
			}
		})
	}
}

func TestRuntimeShort(t *testing.T) {
	tests := map[string]string{
		"claude_code": "cc",
		"codex":       "cdx",
		"other":       "othe",
		"ab":          "ab",
	}
	for in, want := range tests {
		if got := runtimeShort(in); got != want {
			t.Errorf("runtimeShort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPadTruncPadsShortStrings(t *testing.T) {
	got := padTrunc("abc", 6)
	if got != "abc   " {
		t.Errorf("padTrunc(\"abc\", 6) = %q, want %q", got, "abc   ")
	}
}

func TestPadTruncTruncatesLongStrings(t *testing.T) {
	got := padTrunc("abcdefgh", 5)
	if lipglossVisibleLen(got) != 5 {
		t.Errorf("padTrunc(\"abcdefgh\", 5) visible len = %d, want 5 (out=%q)", lipglossVisibleLen(got), got)
	}
}

func TestTruncatePlain(t *testing.T) {
	if got := truncatePlain("hello", 10); got != "hello" {
		t.Errorf("truncatePlain short string changed: %q", got)
	}
	got := truncatePlain("hello world", 8)
	if len([]rune(got)) != 8 {
		t.Errorf("truncatePlain(\"hello world\", 8) = %q, want length 8", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncatePlain should end with ellipsis, got %q", got)
	}
}

// lipglossVisibleLen strips ANSI via display.StripANSI and counts runes —
// a local helper so this test file doesn't need to import lipgloss just for
// width measurement.
func lipglossVisibleLen(s string) int {
	return len([]rune(display.StripANSI(s)))
}
