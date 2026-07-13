package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/tui"
)

// compositionJSON mirrors the wire shape health-collector writes to a
// snapshot's composition_json field (cmd/health-collector/composition.go) —
// a local mirror, not a shared import, per the same D3 rationale as Agent/
// Snapshot in client.go. The four percentages sum to ~1.0.
type compositionJSON struct {
	TextPct     float64 `json:"text_pct"`
	ToolUsePct  float64 `json:"tool_use_pct"`
	ThinkingPct float64 `json:"thinking_pct"`
	ReadingPct  float64 `json:"reading_pct"`
}

// parseComposition unmarshals a snapshot's raw composition_json field.
// Returns nil for a nil/empty pointer or any parse failure — composition is
// optional decoration, never required for the rest of the UI to work.
func parseComposition(raw *string) *compositionJSON {
	if raw == nil || *raw == "" {
		return nil
	}
	var c compositionJSON
	if json.Unmarshal([]byte(*raw), &c) != nil {
		return nil
	}
	return &c
}

// pressureColor returns the style keyed ONLY off pressure_level (D6) — never
// off a client-recomputed threshold, since hysteresis lives server-side in
// internal/agenthealth/notify and a client-side >=0.75 rule would disagree
// with a server still reporting "ok".
func pressureColor(level string) lipgloss.Style {
	switch level {
	case "warning":
		return lipgloss.NewStyle().Foreground(tui.ColorWarn)
	case "critical":
		return lipgloss.NewStyle().Foreground(tui.ColorError).Bold(true)
	default:
		return lipgloss.NewStyle().Foreground(tui.ColorText)
	}
}

// Raw RGB triples mirroring select internal/tui.Color* hex constants —
// needed wherever a color must go through display.RGB directly (bypassing
// lipgloss's TerminalColor, which can't be independently math'd on) or
// through the agents grid's "inactive" row dimming (halveRGB/renderDim in
// agents.go), which needs a [3]int to halve rather than an opaque style.
// Kept in sync with internal/tui/styles.go by hand — there's no automatic
// hex->RGB conversion in this package.
var (
	successRGB = [3]int{63, 185, 80}   // tui.ColorSuccess #3fb950
	warnRGB    = [3]int{210, 153, 34}  // tui.ColorWarn #d29922
	errorRGB   = [3]int{248, 81, 73}   // tui.ColorError #f85149
	accentRGB  = [3]int{88, 166, 255}  // tui.ColorAccent #58a6ff
	purpleRGB  = [3]int{188, 140, 255} // tui.ColorPurple #bc8cff
	textRGB    = [3]int{201, 209, 217} // tui.ColorText #c9d1d9
	metricRGB  = [3]int{57, 197, 207}  // tui.ColorMetric #39c5cf
)

// pressureRGB is pressureColor's raw-RGB twin, for callers that need to
// halve the color (gridSegmentedBar's "inactive" row dimming) rather than
// hand it to a lipgloss.Style.
func pressureRGB(level string) [3]int {
	switch level {
	case "warning":
		return warnRGB
	case "critical":
		return errorRGB
	default:
		return textRGB
	}
}

// unreliableFill renders the dim "--" placeholder shown in place of a bar
// and percentage when fillPct can't be trusted (see fillBar/SegmentedBar).
func unreliableFill() string {
	return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("--")
}

// fillBar renders a 10-cell context-fill bar + percentage label, colored by
// pressure_level. fillPct is nominally a 0..1 fraction, but historical
// gauge rows computed against a since-corrected (smaller) context window
// can carry values well over 1.0. Clamping that display to 100% would still
// be misleading (it'd claim a specific, wrong number) — a dim "--" makes
// clear the data isn't trustworthy instead of asserting a number for it.
func fillBar(fillPct float64, level string) string {
	return fillBarCells(fillPct, level, 10)
}

// fillBarCells is fillBar's cell-count-parameterized core — the agents
// grid's CTX column renders a narrower 6-cell bar (see gridSegmentedBar)
// while the Detail panel keeps the full 10-cell fillBar/SegmentedBar.
func fillBarCells(fillPct float64, level string, cells int) string {
	if fillPct > 1.0 {
		return unreliableFill()
	}
	if fillPct < 0 {
		fillPct = 0
	}
	filled := int(math.Round(fillPct * float64(cells)))
	bar := strings.Repeat("▓", filled) + strings.Repeat("░", cells-filled)
	label := fmt.Sprintf("%3.0f%%", fillPct*100)
	style := pressureColor(level)
	return style.Render(bar) + " " + style.Render(label)
}

// SegmentedBar renders the 10-cell context-fill bar with its FILLED cells
// colored by composition breakdown instead of a single pressure color, when
// composition data is available (falls back to the plain fillBar when comp
// is nil — the common case, since composition_json is only ever present on
// a fetched Snapshot, never on a plain agents-list row). The bar's overall
// fill/empty split still reflects fillPct exactly — context_fill_pct and
// composition are two independent measurements (how full vs. what filled
// it) — only the color of the already-filled cells changes.
//
// Segment colors: text = default (ColorText), tool_use = blue (ColorAccent;
// reading_pct is folded into this segment — both are "tool activity", the
// call and its result), thinking = yellow (ColorWarn). Unfilled cells stay
// dim regardless of composition.
func SegmentedBar(fillPct float64, pressureLevel string, comp *compositionJSON) string {
	return segmentedBarCells(fillPct, pressureLevel, comp, 10)
}

// gridSegmentedBar renders the agents grid's CTX column: the same
// composition-colored bar as SegmentedBar, narrowed to 6 cells (bar + space
// + 4-char "N%%" label = 11 visible cells total) to fit the column diet
// (§ctop AgentAF refinements) — the Detail panel keeps the full-width
// SegmentedBar. Unlike SegmentedBar/fillBar, whose label is keyed only on
// pressure_level (D6 — see TestFillBarColorKeyedOnlyOnPressureLevel), the
// grid's percentage label is colored by a client-side fillPct gradient
// (ctxPctColor) — a quick-glance temperature gauge distinct from the
// server's hysteresis-gated pressure badge, intentionally NOT shared with
// the Detail panel's D6-authoritative rendering.
// gridSegmentedBar takes a dim (rowDim, see agents.go) so the agents grid
// can render an "inactive" row's CTX bar with every segment's own hue
// halved rather than losing it — dimNone/dimFlat both render at full
// brightness here: dimFlat's flat-grey treatment is applied once, over the
// whole assembled row, by renderRow's own post-process (see rowDimLevel's
// doc comment), not per-cell.
func gridSegmentedBar(fillPct float64, pressureLevel string, comp *compositionJSON, dim rowDim) string {
	if fillPct > 1.0 {
		return unreliableFill()
	}
	const cells = 6
	clamped := fillPct
	if clamped < 0 {
		clamped = 0
	}
	filled := int(math.Round(clamped * float64(cells)))
	label := fmt.Sprintf("%3.0f%%", clamped*100)

	if dim == dimHalve {
		var bar string
		if comp == nil {
			bar = renderDim(pressureRGB(pressureLevel), strings.Repeat("▓", filled)+strings.Repeat("░", cells-filled), dimHalve)
		} else {
			bar = compositionBarHalved(clamped, comp, cells)
		}
		return bar + " " + renderDim(ctxPctRGB(clamped), label, dimHalve)
	}

	var bar string
	if comp == nil {
		bar = pressureColor(pressureLevel).Render(strings.Repeat("▓", filled) + strings.Repeat("░", cells-filled))
	} else {
		bar = compositionBar(clamped, comp, cells)
	}
	return bar + " " + lipgloss.NewStyle().Foreground(ctxPctColor(clamped)).Render(label)
}

// ctxPctColor grades a (already-clamped, 0..1) fillPct into the agents
// grid's CTX label gradient: green below 40%, yellow 40-65%, orange
// 65-80%, red 80%+. Deliberately client-side and independent of
// pressure_level/D6 (see gridSegmentedBar's doc comment) — a quick visual
// temperature read, not a substitute for the server's hysteresis-gated
// warning/critical badge.
func ctxPctColor(fillPct float64) lipgloss.TerminalColor {
	switch {
	case fillPct < 0.40:
		return tui.ColorSuccess
	case fillPct < 0.65:
		return lipgloss.Color("#e3b341")
	case fillPct < 0.80:
		return tui.ColorWarn
	default:
		return tui.ColorError
	}
}

// ctxPctRGB is ctxPctColor's raw-RGB twin (same thresholds, kept in sync by
// hand) — for gridSegmentedBar's "inactive" halved-color label.
func ctxPctRGB(fillPct float64) [3]int {
	switch {
	case fillPct < 0.40:
		return successRGB
	case fillPct < 0.65:
		return [3]int{0xe3, 0xb3, 0x41} // #e3b341
	case fillPct < 0.80:
		return warnRGB
	default:
		return errorRGB
	}
}

func segmentedBarCells(fillPct float64, pressureLevel string, comp *compositionJSON, cells int) string {
	if fillPct > 1.0 {
		return unreliableFill()
	}
	if comp == nil {
		return fillBarCells(fillPct, pressureLevel, cells)
	}
	if fillPct < 0 {
		fillPct = 0
	}
	bar := compositionBar(fillPct, comp, cells)
	label := fmt.Sprintf("%3.0f%%", fillPct*100)
	return bar + " " + pressureColor(pressureLevel).Render(label)
}

// compositionBar renders just the composition-colored bar (no percentage
// label) for an already-clamped fillPct and cell count — the segment-split
// math shared by segmentedBarCells (SegmentedBar/Detail panel) and
// gridSegmentedBar (the agents grid), which differ only in cell count and
// how they color the label beside the bar.
// compositionCellCounts computes the text/tool/think/filled cell split
// shared by compositionBar (full color, used by the Detail panel via
// SegmentedBar) and compositionBarHalved (the agents grid's "inactive" row
// dimming) — kept as one function so the rounding/clamping rules can't
// silently drift between the two.
func compositionCellCounts(fillPct float64, comp *compositionJSON, cells int) (textCells, toolCells, thinkCells, filled int) {
	filled = int(math.Round(fillPct * float64(cells)))

	toolPct := comp.ToolUsePct + comp.ReadingPct
	textCells = int(math.Round(comp.TextPct * float64(filled)))
	if textCells > filled {
		textCells = filled
	}
	toolCells = int(math.Round(toolPct * float64(filled)))
	if textCells+toolCells > filled {
		toolCells = filled - textCells
	}
	thinkCells = filled - textCells - toolCells
	if thinkCells < 0 {
		thinkCells = 0
	}
	return textCells, toolCells, thinkCells, filled
}

func compositionBar(fillPct float64, comp *compositionJSON, cells int) string {
	textCells, toolCells, thinkCells, filled := compositionCellCounts(fillPct, comp, cells)

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(tui.ColorText).Render(strings.Repeat("▓", textCells)))
	b.WriteString(lipgloss.NewStyle().Foreground(tui.ColorAccent).Render(strings.Repeat("▓", toolCells)))
	b.WriteString(lipgloss.NewStyle().Foreground(tui.ColorWarn).Render(strings.Repeat("▓", thinkCells)))
	b.WriteString(lipgloss.NewStyle().Foreground(tui.ColorDim).Render(strings.Repeat("░", cells-filled)))
	return b.String()
}

// compositionBarHalved is compositionBar's "inactive" row twin: same
// segment split, each segment's own RGB channel-halved (via renderDim)
// instead of rendered at full brightness — hue survives, brightness drops.
func compositionBarHalved(fillPct float64, comp *compositionJSON, cells int) string {
	textCells, toolCells, thinkCells, filled := compositionCellCounts(fillPct, comp, cells)

	var b strings.Builder
	b.WriteString(renderDim(textRGB, strings.Repeat("▓", textCells), dimHalve))
	b.WriteString(renderDim(accentRGB, strings.Repeat("▓", toolCells), dimHalve))
	b.WriteString(renderDim(warnRGB, strings.Repeat("▓", thinkCells), dimHalve))
	b.WriteString(renderDim(dimGreyRGB, strings.Repeat("░", cells-filled), dimHalve))
	return b.String()
}

// pressureBadge renders the ok/WARN/CRIT three-color badge (D6, shared
// contract with the web dashboard's §3.4 parity table).
func pressureBadge(level string) string {
	switch level {
	case "warning":
		return lipgloss.NewStyle().Foreground(tui.ColorWarn).Bold(true).Render("WARN")
	case "critical":
		return lipgloss.NewStyle().Foreground(tui.ColorError).Bold(true).Render("CRIT")
	case "ok":
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("ok")
	default:
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("—")
	}
}

// livenessBadge renders the glyph+word liveness indicator — the prominent
// "alive" signal in the grid (D6). collector_status never appears here.
func livenessBadge(liveness string) string {
	switch liveness {
	case "live":
		return lipgloss.NewStyle().Foreground(tui.ColorSuccess).Render("● live")
	case "idle":
		return lipgloss.NewStyle().Foreground(tui.ColorText).Render("◌ idle")
	case "stale":
		return lipgloss.NewStyle().Foreground(tui.ColorWarn).Render("○ stale")
	case "closed":
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("✕ closed")
	case "unbound":
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("? unbnd")
	default:
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("—")
	}
}

// statusDots renders the 3-char ST column: a health dot, a space, then a
// runtime glyph. The health dot (see healthDot) is a HEALTH signal — is
// this agent well? — deliberately independent of whether it's doing
// anything right now (that's the ACTIVITY column's job, see
// activityStateFor in agents.go). The space between the two glyphs (added
// per the style guide's glyph-spacing polish — jammed-together dots were
// hard to read) makes the column 3 visible cells, not 2.
func statusDots(liveness, runtime, pressureLevel string, inactive, midTurn bool, now time.Time) string {
	return healthDot(liveness, pressureLevel, inactive, midTurn, now) + " " + runtimeDot(runtime)
}

// healthDotStyle is the health dot's color for an alive, active row, keyed
// ONLY off pressure_level (mirrors pressureColor's own D6 rationale —
// server-side hysteresis is authoritative, never a client-recomputed
// threshold). Split out from healthDot so tests can compare
// GetForeground() directly rather than grep rendered output: lipgloss
// disables styling outside a real TTY (as under `go test`), so two
// differently-styled renders can come out byte-identical even though the
// underlying styles differ — see TestFillBarColorKeyedOnlyOnPressureLevel's
// comment on the same quirk.
func healthDotStyle(pressureLevel string) lipgloss.Style {
	switch pressureLevel {
	case "critical":
		return lipgloss.NewStyle().Foreground(tui.ColorError)
	case "warning":
		return lipgloss.NewStyle().Foreground(tui.ColorWarn)
	default:
		return lipgloss.NewStyle().Foreground(tui.ColorSuccess)
	}
}

// healthDot renders the STATUS column's health glyph — the AgentAF filled/
// hollow pair (operator-specified): "◉" (FISHEYE) is the filled "healthy
// signal present" shape, "○" (WHITE CIRCLE) is hollow "no reliable signal"
// — paired against runtimeDot's own "○" so the ST column reads as two
// visually distinct shapes, not the same glyph recolored twice.
//   - closed: dim hollow "○" — the agent is gone, nothing left to report.
//   - alive (live/idle) and not inactive: pressure-colored "◉"
//     (healthDotStyle) — critical additionally FLASHES between "◉" and "○"
//     every second (same now.Second()%2==1 alternation activityGlyph uses)
//     when the agent is ALSO mid-turn, to draw the eye to a pressured agent
//     actively burning context, not just a pressured one sitting idle.
//   - inactive (alive but no activity for --inactive-after), or any other
//     liveness (stale/unbound/unknown — no reliable health signal to guess
//     at): dim hollow "○" — same shape as closed, distinguished only by
//     color/context, since there's equally no live signal to show filled.
func healthDot(liveness, pressureLevel string, inactive, midTurn bool, now time.Time) string {
	if liveness == "closed" {
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("○")
	}
	alive := liveness == "live" || liveness == "idle"
	if !alive || inactive {
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("○")
	}
	glyph := "◉"
	if pressureLevel == "critical" && midTurn && now.Second()%2 == 1 {
		glyph = "○"
	}
	return healthDotStyle(pressureLevel).Render(glyph)
}

// runtimeDot renders the STATUS column's second glyph: "○" (WHITE CIRCLE,
// hollow) for the common claude_code case — paired against healthDot's
// filled "◉" so the two ST glyphs read as visually distinct shapes, the
// runtime signal deliberately lighter-weight than the health one. codex
// keeps its own "◆" (a third runtime needs its own shape, not this pair).
// claudeOrangeRGB approximates Anthropic's brand coral/orange for the
// claude_code runtime glyph — not sourced from an exact brand asset (none
// available at render time), a reasonable placeholder pending a design pass.
var claudeOrangeRGB = lipgloss.Color("#da7756")

func runtimeDot(runtime string) string {
	switch runtime {
	case "claude_code":
		// "✦" (U+2726 BLACK FOUR POINTED STAR) — a small sparkle glyph
		// evoking Claude's own mark; no exact UTF match confirmed against
		// the AgentAF reference screenshot, so treated as a placeholder
		// (operator: "for now... could work") rather than a verified icon.
		return lipgloss.NewStyle().Foreground(claudeOrangeRGB).Render("✦")
	case "codex":
		return lipgloss.NewStyle().Foreground(tui.ColorPurple).Render("◆")
	default:
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("·")
	}
}

// humanizeTokens formats a token count with k/M suffixes, matching
// internal/render's formatTokens convention.
func humanizeTokens(n int64) string {
	f := float64(n)
	switch {
	case f >= 1_000_000:
		return fmt.Sprintf("%.1fM", f/1_000_000)
	case f >= 1000:
		return fmt.Sprintf("%.0fk", f/1000)
	default:
		return fmt.Sprintf("%.0f", f)
	}
}

// humanizeCount formats a plain integer count with k/M suffixes (no pair).
func humanizeCount(n int) string {
	return humanizeTokens(int64(n))
}

// fmtCost renders a session_cost_usd value as whole dollars — "$0", "$12",
// "$455" — rounded, no cents (per the style guide's column-diet polish: the
// COST column budgets for "$"+4 digits, and cents added width without
// adding much signal at a glance).
func fmtCost(usd float64) string {
	return fmt.Sprintf("$%d", int(usd+0.5))
}

// fmtToolCount renders tool_calls_total for the grid's T column — a plain
// humanized count, no color (it's a volume metric, not a health signal).
func fmtToolCount(n int64) string {
	return humanizeTokens(n)
}

// relativeTime renders a duration-since-t as a compact label: "3s", "11m",
// "2h", "3d". Returns "—" for a zero time.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// formatAge renders the agents-grid age filter for the title bar: "all" for
// unfiltered (d<=0), else a clean "1h"/"6h"/"90m" label for whole-hour or
// whole-minute durations (the only values --max-age or the "r" cycle
// actually produce), falling back to time.Duration's own String() for
// anything unusual.
func formatAge(d time.Duration) string {
	if d <= 0 {
		return "all"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// parseRFC3339 parses the nullable last_activity_ts wire field, returning the
// zero time on empty/unparsable input.
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// modelShort strips the leading "claude-" prefix but keeps any "[1m]"
// long-context suffix, which is signal (§2.3).
func modelShort(model string) string {
	return strings.TrimPrefix(model, "claude-")
}

// modelAbbrev renders the agents grid's narrow MODEL column: a single
// family letter + version, e.g. "claude-opus-4-6" -> "o4.6",
// "claude-sonnet-5" -> "s5", "claude-fable-5" -> "f5", "claude-haiku-4-5" ->
// "h4.5" — any "[1m]" long-context suffix (see modelShort) is preserved.
// Falls back to the raw model string for a family it doesn't recognize.
func modelAbbrev(model string) string {
	base := model
	suffix := ""
	if idx := strings.Index(base, "[1m]"); idx >= 0 {
		suffix = base[idx:]
		base = base[:idx]
	}
	base = strings.TrimPrefix(base, "claude-")

	var letter, rest string
	switch {
	case strings.HasPrefix(base, "opus-"):
		letter, rest = "o", strings.TrimPrefix(base, "opus-")
	case strings.HasPrefix(base, "sonnet-"):
		letter, rest = "s", strings.TrimPrefix(base, "sonnet-")
	case strings.HasPrefix(base, "haiku-"):
		letter, rest = "h", strings.TrimPrefix(base, "haiku-")
	case strings.HasPrefix(base, "fable-"):
		letter, rest = "f", strings.TrimPrefix(base, "fable-")
	default:
		return model
	}
	return letter + strings.ReplaceAll(rest, "-", ".") + suffix
}

// runtimeShort renders the RT column abbreviation.
func runtimeShort(runtime string) string {
	switch runtime {
	case "claude_code":
		return "cc"
	case "codex":
		return "cdx"
	default:
		if len(runtime) > 4 {
			return runtime[:4]
		}
		return runtime
	}
}

// padTrunc left-aligns s to width visible cells, truncating with an ellipsis
// via display.TruncateLine semantics if it's longer.
func padTrunc(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) > width {
		// display.TruncateLine is ANSI-aware (interleaves escape sequences
		// rather than slicing through them) — required here since callers
		// pass already-colored strings (badges, fill bars, entity-colored
		// names). truncatePlain would corrupt an embedded escape sequence.
		return display.TruncateLine(s, width)
	}
	return s + strings.Repeat(" ", width-lipgloss.Width(s))
}

// padRight right-aligns s within width visible cells — the numeric-column
// counterpart to padTrunc's left-align, used for IN/OUT/COST/T so the ones
// place lines up across rows. Truncates (ANSI-aware) if s is longer than
// width.
func padRight(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := lipgloss.Width(s)
	if w > width {
		return display.TruncateLine(s, width)
	}
	return strings.Repeat(" ", width-w) + s
}

// padRightNoTrunc right-aligns s like padRight, but never truncates an
// overlong s — used for the COST cell, where a genuinely large dollar
// figure should overflow the column rather than lose digits (e.g. "$99…"
// silently misreporting a session's actual cost).
func padRightNoTrunc(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

// renderTitleBand renders a panel's 1-line header band: left content, then
// right-aligned content (may be ""), full-width background (titleBandBgRGB,
// brighter titleBandFocusedBgRGB when focused) — the shared primitive
// behind every panel's title band now that bordered boxes are gone (see
// agents.go's titleBand, which adds the composition legend as the right
// side). Reuses applyRowTint's reinject-after-RESET trick so left/right's
// own embedded style resets don't wipe the band's background out partway
// through the line. colorize gates only the background (same convention as
// View()'s own row tint) — foreground colors are always emitted and rely on
// the top-level model.View()'s blanket ANSI strip under --no-color.
func renderTitleBand(left, right string, width int, focused, colorize bool) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	if !colorize {
		return padTrunc(line, width)
	}
	rgb := titleBandBgRGB
	if focused {
		rgb = titleBandFocusedBgRGB
	}
	bg := display.BGRGB(uint8(rgb[0]), uint8(rgb[1]), uint8(rgb[2]))
	return applyRowTint(line, bg, width)
}

// plainTitleBand is renderTitleBand with no right-aligned content — used by
// the Detail and Activity panels, which (unlike the agents grid) have
// nothing to right-align in their title band.
func plainTitleBand(title string, width int, focused, colorize bool) string {
	left := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorAccent).Render(title)
	return renderTitleBand(left, "", width, focused, colorize)
}

// sessionAlias returns the human-friendly display name for a session:
// "#team-name" when team_name is set; else, for a multi-agent session with a
// non-empty focus, a truncated version of that focus (the health API's
// team_name is empty for plenty of live sessions today, but CurrentFocus —
// the WMS focus string — is usually set and still meaningful identity);
// else "solo·<prefix8>" for single-agent sessions or "?·<prefix8>" for an
// unnamed, focus-less multi-agent session — used wherever a session/team's
// identity needs a compact label (the collapsed header row, in particular),
// now that the TEAM column is gone and the background tint is the only
// other identity signal.
func sessionAlias(teamName, sessionID string, isMultiAgent bool, focus string) string {
	if teamName != "" {
		return "#" + teamName
	}
	if isMultiAgent && focus != "" {
		return truncatePlain(sanitizeActivity(focus), 12)
	}
	prefix := sessionPrefix8(sessionID)
	if isMultiAgent {
		return "?·" + prefix
	}
	return "solo·" + prefix
}

// truncatePlain shortens a plain (non-ANSI) string to at most n visible
// runes, appending "…" when truncated.
func truncatePlain(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}
