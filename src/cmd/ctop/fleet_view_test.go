package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
)

func TestCtxGradientAnchorsExact(t *testing.T) {
	for _, a := range ctxGradientAnchors {
		if got := ctxGradientRGB(a.pos); got != a.rgb {
			t.Errorf("ctxGradientRGB(%v) = %v, want anchor %v", a.pos, got, a.rgb)
		}
	}
	if got := ctxGradientRGB(-0.5); got != ctxGradientAnchors[0].rgb {
		t.Errorf("below-range fill = %v, want first anchor", got)
	}
	if got := ctxGradientRGB(1.5); got != errorRGB {
		t.Errorf("above-range fill = %v, want red", got)
	}
}

func TestCtxGradientShiftsGreenToRed(t *testing.T) {
	low := ctxGradientRGB(0.05)
	high := ctxGradientRGB(0.95)
	if !(low[1] > low[0]) {
		t.Errorf("low fill %v should read green (G > R)", low)
	}
	if !(high[0] > high[1]) {
		t.Errorf("high fill %v should read red (R > G)", high)
	}
}

func TestFleetCtxBarFillAndUnreliable(t *testing.T) {
	if got := display.StripANSI(fleetCtxBar(1.5, dimNone)); !strings.Contains(got, "--") {
		t.Errorf("fill > 1.0 should render the dim -- placeholder, got %q", got)
	}
	full := display.StripANSI(fleetCtxBar(1.0, dimNone))
	if strings.Count(full, "█") != fleetCtxBarCells {
		t.Errorf("full bar = %q, want %d filled cells", full, fleetCtxBarCells)
	}
	empty := display.StripANSI(fleetCtxBar(0, dimNone))
	if strings.Count(empty, "░") != fleetCtxBarCells {
		t.Errorf("empty bar = %q, want %d empty cells", empty, fleetCtxBarCells)
	}
	half := display.StripANSI(fleetCtxBar(0.5, dimNone))
	if strings.Count(half, "█") != fleetCtxBarCells/2 {
		t.Errorf("half bar = %q, want %d filled cells", half, fleetCtxBarCells/2)
	}
}

func TestFleetModelAbbrev(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-6":     "opus-4-6",
		"claude-opus-4-6[1m]": "opus-4-6",
		"claude-sonnet-5":     "sonnet-5",
		"claude-haiku-4-5":    "haiku-4-5", // fits in full at fleetModelW (9) — was cutting to "haiku-4-" at the old width 8
		"claude-fable-5":      "fable-5",
		"":                    "--",
		"gpt-5.1-codex":       "gpt-5.1-c", // no "claude-" prefix to strip; hard-truncated at 9, no version field split
	}
	for in, want := range cases {
		if got := fleetModelAbbrev(in); got != want {
			t.Errorf("fleetModelAbbrev(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFleetModelAbbrevNeverSplitsVersionNumber pins the truncation
// backoff itself (operator report: "haiku-4-5" cutting to "haiku-4-" —
// fleetModelW's widening to 9 already fixes every current model name, but
// this guards any future longer one): a hard cut that would land on a
// bare trailing "-" or split a multi-digit number in half backs off to
// the last "-" boundary instead of showing a half-finished version field.
func TestFleetModelAbbrevNeverSplitsVersionNumber(t *testing.T) {
	if got := fleetModelAbbrev("xxx-123-456"); got != "xxx-123" {
		t.Errorf("fleetModelAbbrev(%q) = %q, want %q (digit-split backoff)", "xxx-123-456", got, "xxx-123")
	}
	if got := fleetModelAbbrev("xxxxx-12-34"); got != "xxxxx-12" {
		t.Errorf("fleetModelAbbrev(%q) = %q, want %q (trailing-dash backoff)", "xxxxx-12-34", got, "xxxxx-12")
	}
}

func TestFleetCursorReconcile(t *testing.T) {
	var a fleetModel
	rows := []fleetRow{
		{agent: Agent{SessionID: "s1", AgentName: "echo"}},
		{agent: Agent{SessionID: "s1", AgentName: "tonka"}},
	}
	a.setCursor(rows, 1)
	// Rows reorder: cursor should follow tonka to index 0.
	reordered := []fleetRow{rows[1], rows[0]}
	if got := a.reconcile(reordered); got != 0 {
		t.Errorf("reconcile after reorder = %d, want 0", got)
	}
	// Cursor row vanishes entirely: falls back to the stored index, clamped.
	if got := a.reconcile([]fleetRow{rows[0]}); got != 0 {
		t.Errorf("reconcile after removal = %d, want 0 (clamped)", got)
	}
}

// TestHandleKeyDispatchesToFleetView is the regression for ctop's single-view
// simplification: handleKey no longer switches between views (there's only
// ever fleetView) — it forwards any key it doesn't itself claim (quit, help)
// straight to fleetView's own Update.
func TestHandleKeyDispatchesToFleetView(t *testing.T) {
	m := model{}
	tm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	got := tm.(model)
	if !got.fleet.flat {
		t.Fatal("'t' should reach fleetView.Update and toggle fleet.flat")
	}
	v := fleetView{m: &got}
	if v.Title() != "fleet" {
		t.Fatalf("Title() = %q, want fleet", v.Title())
	}
}

func TestFleetViewRendersRows(t *testing.T) {
	m := model{width: 120, height: 20, colorize: true}
	agoTs := strPtr(time.Now().Add(-2 * time.Minute).Format(time.RFC3339))
	m.agents.setRows([]Agent{
		{SessionID: "s1", AgentName: "", TeamName: "medic", Liveness: "live", Model: "claude-opus-4-6", ContextFillPct: 0.42, SessionCostUSD: 12.3, TokensInTotal: 39000, TokensOutTotal: 40000, CurrentFocus: "ship view 4", LastActivityTs: agoTs},
		{SessionID: "s1", AgentName: "echo", Liveness: "live", Model: "claude-sonnet-5", ContextFillPct: 0.11, SessionCostUSD: 0, TokensInTotal: 13000, TokensOutTotal: 17000, LastActivityTs: agoTs},
	})
	v := fleetView{m: &m}
	out := display.StripANSI(v.View(120, 18))

	for _, want := range []string{"@echo", "sonnet-5", "opus-4-6", "TOTAL", "AGENT", "CTX", "AGE", "ACTIVITY", "Agents:", "2 agents", "42%", "11%", "2m"} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet view output missing %q\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 18 {
		t.Errorf("fleet view rendered %d lines, want 18", len(lines))
	}
	for i, l := range lines {
		if w := len([]rune(l)); w != 120 {
			t.Errorf("line %d width = %d, want 120: %q", i, w, l)
		}
	}
}

// TestFleetViewColumnOrderAndEconomicsAlignment is the regression for the
// operator-specified reorder: dots lead, AGE sits before ACTIVITY text, PRJ
// is gone, and — the tricky part — the CTX/economics block on ACTIVITY's
// right must land at the SAME column across rows despite ACTIVITY being a
// ragged variable-width field in the middle of the row now (mirrors the
// AGENT view's own AGE-column right-anchor fix; see rightAnchorTrailing).
func TestFleetViewColumnOrderAndEconomicsAlignment(t *testing.T) {
	m := model{width: 160, height: 20, colorize: true}
	ts := strPtr(time.Now().Add(-2 * time.Minute).Format(time.RFC3339))
	m.agents.setRows([]Agent{
		{SessionID: "s1", AgentName: "short", Liveness: "live", Model: "claude-sonnet-5", ContextFillPct: 0.42, LastActivityDisplay: "idle", LastActivityTs: ts},
		{SessionID: "s1", AgentName: "long", Liveness: "live", Model: "claude-sonnet-5", ContextFillPct: 0.11, LastActivityDisplay: "a much longer activity description here", LastActivityTs: ts},
	})
	v := fleetView{m: &m}
	out := display.StripANSI(v.View(160, 18))
	lines := strings.Split(out, "\n")
	if len(lines) < 4 {
		t.Fatalf("fleet view output too short: %q", out)
	}

	header := lines[1]
	if strings.Contains(header, "PRJ") {
		t.Errorf("header %q, want no PRJ column (dropped)", header)
	}
	ageIdx := strings.Index(header, "AGE")
	activityIdx := strings.Index(header, "ACTIVITY")
	if ageIdx == -1 || activityIdx == -1 {
		t.Fatalf("header %q missing AGE or ACTIVITY", header)
	}
	if ageIdx >= activityIdx {
		t.Errorf("header %q, want AGE before ACTIVITY", header)
	}

	// lines[2] is the session's v2 team-header row; agents start at 3.
	rowShort, rowLong := lines[3], lines[4]
	if !strings.Contains(rowShort, "idle") {
		t.Fatalf("rowShort %q missing activity text", rowShort)
	}
	if !strings.Contains(rowLong, "a much longer activity description here") {
		t.Fatalf("rowLong %q missing activity text", rowLong)
	}
	if strings.Contains(rowShort, "--") || strings.Contains(rowLong, "--") {
		t.Errorf("rows contain %q, want no PRJ placeholder (dropped)", "--")
	}

	// runeIndex, not strings.Index: rowShort's cursor marker "▸" is a
	// multi-byte UTF-8 rune (rowLong's cursor-less leading cell is a plain
	// 1-byte space), so a byte offset comparison between the two rows would
	// be off by the marker's extra bytes even when the two rows are
	// genuinely aligned on a visible-column basis.
	ctxIdxShort := runeIndex(rowShort, "42%")
	ctxIdxLong := runeIndex(rowLong, "11%")
	if ctxIdxShort == -1 || ctxIdxLong == -1 {
		t.Fatalf("missing CTX value: rowShort=%q rowLong=%q", rowShort, rowLong)
	}
	if ctxIdxShort != ctxIdxLong {
		t.Errorf("CTX column not aligned: short-activity row has it at column %d, long-activity row at column %d (want equal — ACTIVITY is a fixed-width budgeted column, see fleetLayoutFor)", ctxIdxShort, ctxIdxLong)
	}
}

// TestFleetLayoutForBudgetIsExact is the regression for the column-width
// management rework (replacing rightAnchorTrailing): every width from
// fleetLayoutFor must sum EXACTLY back to the panel width — totalFixed +
// activityW == width — for every column to land at a real, predictable
// screen position rather than an anchored-from-the-right approximation.
func TestFleetLayoutForBudgetIsExact(t *testing.T) {
	for _, w := range []int{220, 160, 100, 90, 85, 81, 80, 79, 70, 60} {
		cs := fleetColumnsForWidth(w)
		_, layout := fleetLayoutFor(w, 8, cs)
		if got := layout.totalFixed + layout.activityW; got != w {
			t.Errorf("fleetLayoutFor(%d): totalFixed(%d)+activityW(%d) = %d, want %d", w, layout.totalFixed, layout.activityW, got, w)
		}
	}
}

// TestFleetLayoutForSqueezesBarBeforeActivityGoesTooNarrow covers the
// squeeze priority: fleetColumnsForWidth alone would keep the CTX bar at
// width 80 (its own w>=80 threshold), but that leaves only 15 cells for
// ACTIVITY — below fleetMinActivityW — so fleetLayoutFor must drop the bar
// itself to free up room, rather than rendering an almost-unreadable
// activity column just because the bar's own threshold was technically met.
func TestFleetLayoutForSqueezesBarBeforeActivityGoesTooNarrow(t *testing.T) {
	cs := fleetColumnsForWidth(80)
	if !cs.bar {
		t.Fatal("fleetColumnsForWidth(80).bar = false, want true (precondition: bar's own threshold is met here)")
	}
	got, layout := fleetLayoutFor(80, 8, cs)
	if got.bar {
		t.Error("fleetLayoutFor(80) kept the bar, want it dropped (activityW would be below fleetMinActivityW otherwise)")
	}
	if layout.activityW < fleetMinActivityW {
		t.Errorf("activityW = %d, want >= %d after the bar-drop squeeze", layout.activityW, fleetMinActivityW)
	}
}

// TestFleetLayoutForClampsAtZeroForPathologicallyNarrowWidth ensures a
// terminal too narrow for even the fixed columns degrades to activityW=0
// rather than going negative (which would panic strings.Repeat elsewhere).
func TestFleetLayoutForClampsAtZeroForPathologicallyNarrowWidth(t *testing.T) {
	cs := fleetColumnsForWidth(10)
	_, layout := fleetLayoutFor(10, 8, cs)
	if layout.activityW != 0 {
		t.Errorf("activityW = %d, want 0 (clamped, fixed columns alone already exceed width 10)", layout.activityW)
	}
}

// TestFleetRenderAgentRowTruncatesOverlongActivityWithEllipsis covers the
// truncate-to-budget behavior itself (not just the layout math): an
// ACTIVITY string longer than its budget must be cut down to exactly that
// width, ellipsis included (padTrunc/display.TruncateLine's convention),
// not silently overflow into the economics block.
func TestFleetRenderAgentRowTruncatesOverlongActivityWithEllipsis(t *testing.T) {
	m := model{width: 60, colorize: true}
	row := Agent{SessionID: "s1", AgentName: "a", Liveness: "live", Model: "claude-sonnet-5", LastActivityDisplay: strings.Repeat("x", 200)}
	v := fleetView{m: &m}
	cs := fleetColumnsForWidth(60)
	cs, layout := fleetLayoutFor(60, 8, cs)
	out := display.StripANSI(v.renderAgentRow(fleetRow{agent: row}, false, cs, layout, 60))
	if w := len([]rune(out)); w != 60 {
		t.Errorf("row width = %d, want exactly 60", w)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("row = %q, want an ellipsis marking the truncated activity text", out)
	}
}

// runeIndex returns sub's index within s in RUNES, not bytes (unlike
// strings.Index) — needed when comparing horizontal column positions across
// lines that may contain multi-byte glyphs (dots, bars) at different counts.
func runeIndex(s, sub string) int {
	byteIdx := strings.Index(s, sub)
	if byteIdx == -1 {
		return -1
	}
	return len([]rune(s[:byteIdx]))
}

// TestFleetHealthDotIsAlwaysFilled covers fleetHealthDot's glyph shape: it
// always renders "◉" — coloring (critical/warning/ok) is delegated to
// format.go's already-tested healthDotStyle (TestHealthDotStyleKeyedOnlyOnPressureLevel),
// so this only needs to confirm the shape, not re-verify the color mapping.
func TestFleetHealthDotIsAlwaysFilled(t *testing.T) {
	for _, level := range []string{"critical", "warning", "ok", ""} {
		got := display.StripANSI(fleetHealthDot(level))
		if got != "◉" {
			t.Errorf("fleetHealthDot(%q) = %q, want the filled glyph ◉", level, got)
		}
	}
}

// fleetHealthDotGlyph extracts JUST the health-dot character from a
// rendered (and StripANSI'd) Fleet-view row — column 4 (0-indexed: marker,
// activity glyph, activity glyph's own baked-in trailing space, health
// dot) — since the preceding ACTIVITY dot can independently render "◉" or
// "○" too (its own liveness/state-keyed logic), a whole-line
// strings.Contains check can't isolate the health dot specifically.
func fleetHealthDotGlyph(strippedRow string) string {
	// marker(1) + activityDot(fleetActivityDotW) + sep(1) + MODEL(fleetModelW) +
	// sep(1) — see fleetAssemble's column order (MODEL precedes the
	// health dot, operator-requested reorder).
	idx := 1 + fleetActivityDotW + 1 + fleetModelW + 1
	r := []rune(strippedRow)
	if len(r) <= idx {
		return ""
	}
	return string(r[idx])
}

// TestFleetRenderAgentRowHealthDotHollowOnlyWithNoGaugeData is the
// regression for the operator-reported liveness data bug: the Fleet
// view's health dot must key on pressure_level/collector_status, not
// liveness — an agent with a genuine gauge row (pressure_level or
// collector_status set) always gets the filled "◉", even with
// liveness=="live" would (irrelevantly) trigger the ACTIVITY dot's own
// "◉", which is exactly why this test isolates the health-dot character
// specifically (see fleetHealthDotGlyph) rather than searching the whole
// line. Only a row with BOTH pressure_level and collector_status empty (no
// gauge row ever collected) gets the dim hollow "○" health dot.
func TestFleetRenderAgentRowHealthDotHollowOnlyWithNoGaugeData(t *testing.T) {
	m := model{width: 160, colorize: true}
	v := fleetView{m: &m}
	cs := fleetColumnsForWidth(160)
	cs, layout := fleetLayoutFor(160, 8, cs)

	noData := Agent{SessionID: "s1", AgentName: "a", Liveness: "live"}
	out := display.StripANSI(v.renderAgentRow(fleetRow{agent: noData}, false, cs, layout, 160))
	if got := fleetHealthDotGlyph(out); got != "○" {
		t.Errorf("health dot for no-gauge-data row = %q, want hollow ○: row=%q", got, out)
	}

	// liveness=="closed" (the buggy signal per the operator's report) but
	// pressure_level IS set — must still render filled, not hollow.
	buggyLivenessButHasData := Agent{SessionID: "s1", AgentName: "b", Liveness: "closed", PressureLevel: "ok"}
	out2 := display.StripANSI(v.renderAgentRow(fleetRow{agent: buggyLivenessButHasData}, false, cs, layout, 160))
	if got := fleetHealthDotGlyph(out2); got != "◉" {
		t.Errorf("health dot for closed-liveness-but-has-data row = %q, want filled ◉ (pressure_level, not liveness, decides): row=%q", got, out2)
	}

	// collector_status alone (no explicit pressure_level) also counts as
	// "has data".
	collectorOnlyData := Agent{SessionID: "s1", AgentName: "c", Liveness: "live", CollectorStatus: "fresh"}
	out3 := display.StripANSI(v.renderAgentRow(fleetRow{agent: collectorOnlyData}, false, cs, layout, 160))
	if got := fleetHealthDotGlyph(out3); got != "◉" {
		t.Errorf("health dot for collector_status-only row = %q, want filled ◉: row=%q", got, out3)
	}
}

// TestFleetRenderAgentRowHasNoRuntimeDot is the regression for dropping the
// third (runtime) dot from the Fleet view — only two dots now: activity
// state + health. codex's distinct "◆" runtime glyph (still used elsewhere,
// e.g. the AGENT view's ST column) must never appear in a Fleet-view row.
func TestFleetRenderAgentRowHasNoRuntimeDot(t *testing.T) {
	m := model{width: 160, colorize: true}
	v := fleetView{m: &m}
	cs := fleetColumnsForWidth(160)
	cs, layout := fleetLayoutFor(160, 8, cs)
	row := Agent{SessionID: "s1", AgentName: "a", Liveness: "live", Runtime: "codex", PressureLevel: "ok"}
	out := display.StripANSI(v.renderAgentRow(fleetRow{agent: row}, false, cs, layout, 160))
	if strings.Contains(out, "◆") {
		t.Errorf("renderAgentRow(codex) = %q, want no runtime dot (◆) — Fleet view drops the third dot entirely", out)
	}
}

// TestFleetCostCellBoldForLeadOnly covers the lead-vs-teammate cost weight
// distinction directly. Uses display.RGB/display.BOLD (raw ANSI, always
// emitted) rather than lipgloss.NewStyle()'s GetBold()-style comparison —
// unlike lipgloss, which silently no-ops styling outside a real TTY (as
// under go test — see several other tests' comments on the same quirk),
// fleetCostCell's raw escapes are always present, so a direct substring
// check is reliable here.
func TestFleetCostCellBoldForLeadOnly(t *testing.T) {
	leadCell := fleetCostCell(12.34, 0, true, dimNone)
	teammateCell := fleetCostCell(12.34, 12.34, false, dimNone)
	if !strings.Contains(leadCell, display.BOLD) {
		t.Errorf("lead cost cell = %q, want it to contain the bold escape %q", leadCell, display.BOLD)
	}
	if strings.Contains(teammateCell, display.BOLD) {
		t.Errorf("teammate cost cell = %q, want no bold escape", teammateCell)
	}
	if got, want := display.StripANSI(leadCell), display.StripANSI(teammateCell); got != want {
		t.Errorf("stripped content differs: lead=%q teammate=%q, want identical visible text", got, want)
	}
}

// TestFleetRenderAgentRowCostNeverBold: under v2 the bold cost weight
// belongs to the TEAM HEADER's authoritative total (per-agent cost since
// 039f718) — every member row, the lead included, renders normal weight
// (operator decision 2026-07-13). The header side of this rule is covered
// by TestFleetTeamHeaderTotalsBoldAndAligned.
func TestFleetRenderAgentRowCostNeverBold(t *testing.T) {
	m := model{width: 160, colorize: true}
	v := fleetView{m: &m}
	cs := fleetColumnsForWidth(160)
	cs, layout := fleetLayoutFor(160, 8, cs)

	for _, r := range []Agent{
		{SessionID: "s1", AgentName: "", Liveness: "live", SessionCostUSD: 12.34},
		{SessionID: "s1", AgentName: "echo", Liveness: "live", SessionCostUSD: 12.34},
	} {
		out := v.renderAgentRow(fleetRow{agent: r}, false, cs, layout, 160)
		if strings.Contains(out, display.BOLD) {
			t.Errorf("member row (name=%q) = %q, want no bold escape anywhere", r.AgentName, out)
		}
	}
}

// --- spawn-tree hierarchy (fleetTreeRows) ---

// treeAgent builds a hierarchy-test fixture row.
func treeAgent(name, rosterID, parentRef, relationship string) Agent {
	a := Agent{SessionID: "s1", AgentName: name, Relationship: relationship, Liveness: "live"}
	if rosterID != "" {
		a.RosterID = strPtr(rosterID)
	}
	if parentRef != "" {
		a.ParentRef = strPtr(parentRef)
	}
	return a
}

// names flattens a fleetRow list to "prefix+name" strings for compact
// order+connector assertions.
func names(rows []fleetRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.prefix + r.agent.AgentName + r.suffix
	}
	return out
}

// TestFleetTreeRowsTeammatesOfLeadNest is the current no-regression
// guarantee (operator correction 2026-07-14, superseding the prior
// "stays flat" pin): under the current hookd heuristic (every non-lead is
// "teammate" with parent_ref → the session lead), those teammates now nest
// beneath the lead — dash-less connectors ("├"/"└") since they're durable
// members, not ephemeral spawns — in tree mode, and pick up the "↳lead"
// suffix in flat mode.
func TestFleetTreeRowsTeammatesOfLeadNest(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("ux-design", "U", "L", "teammate"),
	}
	got := names(fleetTreeRows(rows, false))
	want := []string{"", "├collector", "└ux-design"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("flat=false row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	gotFlat := names(fleetTreeRows(rows, true))
	wantFlat := []string{"", "collector ↳lead", "ux-design ↳lead"}
	for i := range wantFlat {
		if gotFlat[i] != wantFlat[i] {
			t.Errorf("flat=true row %d = %q, want %q (full: %v)", i, gotFlat[i], wantFlat[i], gotFlat)
		}
	}
}

// TestFleetTreeRowsNestsSpawns covers the core tree: teammates nest under
// the lead with dash-less connectors ├/└, their own subagent spawns nest
// deeper still with dashed connectors ├─/└─ and a │ guide-line tracking
// whether the owning teammate has more siblings below.
func TestFleetTreeRowsNestsSpawns(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("Explore", "E", "C", "subagent"),
		treeAgent("grep-bot", "G", "E", "subagent"),
		treeAgent("doc-scan", "D", "C", "subagent"),
		treeAgent("ux-design", "U", "L", "teammate"),
	}
	got := names(fleetTreeRows(rows, false))
	want := []string{"", "├collector", "│ ├─Explore", "│ │ └─grep-bot", "│ └─doc-scan", "└ux-design"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFleetTreeRowsSubagentUnderLeadNests: a lead's own direct Agent-tool
// spawn is still a child, not a teammate — relationship=="subagent" nests
// even when the visible parent is the lead.
func TestFleetTreeRowsSubagentUnderLeadNests(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("Explore", "E", "L", "subagent"),
	}
	got := names(fleetTreeRows(rows, false))
	if got[1] != "└─Explore" {
		t.Errorf("lead's subagent = %q, want nested └─Explore (full: %v)", got[1], got)
	}
}

// TestFleetTreeRowsOrphanPromoted: a subagent whose parent_ref doesn't
// resolve to a visible row is promoted to top level with the ↳ marker,
// after the lead's own (now-nesting) subtree — never dropped.
func TestFleetTreeRowsOrphanPromoted(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("grep-bot", "G", "GONE", "subagent"),
		treeAgent("ux-design", "U", "L", "teammate"),
	}
	got := names(fleetTreeRows(rows, false))
	want := []string{"", "└ux-design", "↳grep-bot"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// Flat mode: unresolvable parent keeps the ↳ prefix too (no name to
	// annotate with).
	gotFlat := names(fleetTreeRows(rows, true))
	if gotFlat[1] != "↳grep-bot" {
		t.Errorf("flat orphan = %q, want ↳grep-bot", gotFlat[1])
	}
}

// TestFleetTreeRowsCycleStillRendersAllRows: mutually-referencing
// parent_refs (bad data) must not drop rows or hang — the cycle is broken
// by promoting its first member as an orphan and nesting the rest under it.
func TestFleetTreeRowsCycleStillRendersAllRows(t *testing.T) {
	rows := []Agent{
		treeAgent("a", "A", "B", "subagent"),
		treeAgent("b", "B", "A", "subagent"),
	}
	got := names(fleetTreeRows(rows, false))
	want := []string{"↳a", "└─b"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("cycle rows = %v, want %v", got, want)
	}
}

// TestFleetTreeRowsDepthCapped: a spawn chain deeper than
// fleetMaxTreeDepth renders at the cap — the prefix stops growing — so
// names can't be pushed out of the AGENT column.
func TestFleetTreeRowsDepthCapped(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("d1", "A", "L", "subagent"),
		treeAgent("d2", "B", "A", "subagent"),
		treeAgent("d3", "C", "B", "subagent"),
		treeAgent("d4", "D", "C", "subagent"),
		treeAgent("d5", "E", "D", "subagent"),
	}
	got := fleetTreeRows(rows, false)
	capW := lipgloss.Width(got[3].prefix) // d3 — at the cap (depth 3)
	for _, i := range []int{4, 5} {       // d4, d5 — beyond the cap
		if w := lipgloss.Width(got[i].prefix); w != capW {
			t.Errorf("depth-%d prefix %q width = %d, want capped at %d", i, got[i].prefix, w, capW)
		}
	}
}

// TestFleetTreeRowsFlatModeAnnotatesLineage: flat mode keeps input order
// and swaps indentation for a ↳parent suffix on nested-eligible rows.
func TestFleetTreeRowsFlatModeAnnotatesLineage(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("Explore", "E", "C", "subagent"),
		treeAgent("probe", "P", "L", "subagent"),
	}
	got := names(fleetTreeRows(rows, true))
	want := []string{"", "collector ↳lead", "Explore ↳collector", "probe ↳lead"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("flat row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFleetAgentColWidthAccountsForPrefixAndCapsAt18: the width calc must
// include the connector prefix (else nested names truncate) and still cap.
func TestFleetAgentColWidthAccountsForPrefixAndCapsAt18(t *testing.T) {
	rows := []fleetRow{
		{agent: Agent{AgentName: "echo"}, prefix: "│ └─"}, // 4 + 5 ("@echo") = 9
	}
	if got := fleetAgentColWidth(rows); got != 9 {
		t.Errorf("fleetAgentColWidth = %d, want 9 (prefix 4 + @echo 5)", got)
	}
	wide := []fleetRow{{agent: Agent{AgentName: "a-very-long-agent-name"}, prefix: "│ └─"}}
	if got := fleetAgentColWidth(wide); got != 18 {
		t.Errorf("fleetAgentColWidth(wide) = %d, want cap 18", got)
	}
}

// TestFleetTKeyTogglesFlatAndCursorFollowsAgent: "t" flips modes and the
// cursor stays on the SAME agent across the reorder (reconcile by key, not
// index).
func TestFleetTKeyTogglesFlatAndCursorFollowsAgent(t *testing.T) {
	m := model{width: 160, height: 20, colorize: true}
	m.agents.setRows([]Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("Explore", "E", "C", "subagent"),
		treeAgent("zeta", "Z", "L", "teammate"),
	})
	v := fleetView{m: &m}

	// Forest order: header, └@lead, ├@collector, │ └─@Explore, └@zeta.
	// Three j's from the header land on Explore.
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	treeRows := v.fleetRowsFor()
	if got := treeRows[m.fleet.reconcile(treeRows)].agent.AgentName; got != "Explore" {
		t.Fatalf("cursor before toggle on %q, want Explore", got)
	}

	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if !m.fleet.flat {
		t.Fatal("flat = false after t, want true")
	}
	flatRows := v.fleetRowsFor()
	if got := flatRows[m.fleet.reconcile(flatRows)].agent.AgentName; got != "Explore" {
		t.Errorf("cursor after toggle on %q, want still Explore", got)
	}

	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if m.fleet.flat {
		t.Error("flat = true after second t, want false (round-trip)")
	}
}

// TestFleetViewRendersTree: full View() integration — nested connectors
// visible in output, and every line still exactly panel-width.
func TestFleetViewRendersTree(t *testing.T) {
	m := model{width: 140, height: 20, colorize: true}
	m.agents.setRows([]Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("Explore", "E", "C", "subagent"),
		treeAgent("doc-scan", "D", "C", "subagent"),
	})
	v := fleetView{m: &m}
	out := display.StripANSI(v.View(140, 18))

	for _, want := range []string{"├─@Explore", "└─@doc-scan", "@collector", "4 agents"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree view output missing %q\n%s", want, out)
		}
	}
	for i, l := range strings.Split(out, "\n") {
		if w := len([]rune(l)); w != 140 {
			t.Errorf("line %d width = %d, want 140: %q", i, w, l)
		}
	}
}

// --- v2 forest (team header rows) ---

// forestNames flattens forest rows into compact assertion strings:
// headers as "H:<label>", agents as prefix+displayname+suffix.
func forestNames(rows []fleetRow) []string {
	out := make([]string, len(rows))
	for i, fr := range rows {
		if fr.isHeader {
			out[i] = "H:" + fr.label
			continue
		}
		out[i] = fr.prefix + fleetRowName(fr) + fr.suffix
	}
	return out
}

// medicGroup builds the operator's mockup-A team as an agentGroup fixture:
// lead + two teammates, two spawns under @collector, one grandchild.
func medicGroup() agentGroup {
	lead := treeAgent("", "L", "", "lead")
	lead.TeamName = "medic"
	lead.CurrentFocus = "ship fleet view multi-team hierarchy"
	return agentGroup{sessionID: "sess-medic", rows: []Agent{
		lead,
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("Explore", "E", "C", "subagent"),
		treeAgent("grep-bot", "G", "E", "subagent"),
		treeAgent("doc-scan", "D", "C", "subagent"),
		treeAgent("ux-design", "U", "L", "teammate"),
	}}
}

// TestFleetForestRowsNestsTeammatesUnderLead pins the full v2 connector
// grammar (operator correction 2026-07-14, superseding mockup A's flat
// members list): the lead is the tree's sole root and gets the header's
// dash-less connector, every teammate nests one level beneath it with its
// own dash-less connector plus a flat leading space marking the nest, and
// Agent-tool spawns nest deeper still under a │/blank guide, dashed to
// mark them as ephemeral rather than durable members.
func TestFleetForestRowsNestsTeammatesUnderLead(t *testing.T) {
	got := forestNames(fleetForestRows([]agentGroup{medicGroup()}, nil, false))
	want := []string{
		"H:#medic",
		"└@lead",
		" ├@collector",
		" │ ├─@Explore",
		" │ │ └─@grep-bot",
		" │ └─@doc-scan",
		" └@ux-design",
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFleetTeamMemberRowsGuidesOrphanedSubtree covers the case
// fleetTreeRows' single-root cont=nil walk can't see on its own: a second
// header-level root (a subagent whose parent_ref is unresolvable) means
// the lead is no longer the header's only child, so the lead's OWN
// subtree (collector, and collector's own spawn) needs a "│ " guide
// re-threaded onto every descendant to show the header has more coming
// after it — otherwise collector reads as a peer of lead rather than its
// child, and a deeper spawn detaches from the guide chain entirely.
func TestFleetTeamMemberRowsGuidesOrphanedSubtree(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("Explore", "E", "C", "subagent"),
		treeAgent("orphan", "O", "GONE", "subagent"),
	}
	got := forestNames(fleetTeamMemberRows(rows))
	want := []string{"├@lead", " │ └@collector", " │   └─@Explore", "└↳@orphan"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFleetTeamMemberRowsGuidesStrayTeammateSubtree: same guide-reflow
// requirement, but the second header-level root is a TEAMMATE with an
// unresolvable parent_ref (a plain root, not the ↳-marked orphan a
// subagent gets) rather than a subagent — the lead's subtree still needs
// its "│ " guide.
func TestFleetTeamMemberRowsGuidesStrayTeammateSubtree(t *testing.T) {
	rows := []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("stray", "S", "MISSING", "teammate"),
	}
	got := forestNames(fleetTeamMemberRows(rows))
	want := []string{"├@lead", " │ └@collector", "└@stray"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFleetForestRowsBareSoloGetsHeader: a teamless single-agent session
// now gets a header too (operator correction 2026-07-13: every session is
// a grouping unit) — one header row labeled solo·<prefix8>, with the lead
// nested beneath as an ordinary single-member row.
func TestFleetForestRowsBareSoloGetsHeader(t *testing.T) {
	g := agentGroup{sessionID: "82f1c9d0-dead-beef", rows: []Agent{treeAgent("", "S", "", "lead")}}
	got := fleetForestRows([]agentGroup{g}, nil, false)
	if len(got) != 2 {
		t.Fatalf("rows = %v, want header + one lead member row", forestNames(got))
	}
	if !got[0].isHeader || got[0].label != "solo·82f1c9d0" {
		t.Errorf("header = %+v, want isHeader with label solo·82f1c9d0", got[0])
	}
	if got[1].isHeader || fleetRowName(got[1]) != "@lead" {
		t.Errorf("member row = %q, want a non-header @lead row", fleetRowName(got[1]))
	}
}

// TestFleetForestRowsTeamlessMultiAgentGetsHeader: with members to anchor
// and totals to show, a teamless multi-agent session DOES get a header,
// labeled via sessionAlias's fallback chain.
func TestFleetForestRowsTeamlessMultiAgentGetsHeader(t *testing.T) {
	g := agentGroup{sessionID: "abcd1234-x", rows: []Agent{
		treeAgent("", "L", "", "lead"),
		treeAgent("echo", "W", "L", "teammate"),
	}}
	got := fleetForestRows([]agentGroup{g}, nil, false)
	if len(got) != 3 || !got[0].isHeader {
		t.Fatalf("rows = %v, want header + 2 members", forestNames(got))
	}
	if got[0].label != "?·abcd1234" {
		t.Errorf("teamless header label = %q, want ?·abcd1234 (sessionAlias fallback)", got[0].label)
	}
}

// TestFleetForestRowsDuplicateTeamNamesDisambiguated: two sessions sharing
// a team name get ·<prefix8> on the second onward.
func TestFleetForestRowsDuplicateTeamNamesDisambiguated(t *testing.T) {
	g1, g2 := medicGroup(), medicGroup()
	g2.sessionID = "sess2-medic"
	got := fleetForestRows([]agentGroup{g1, g2}, nil, false)
	var labels []string
	for _, fr := range got {
		if fr.isHeader {
			labels = append(labels, fr.label)
		}
	}
	if len(labels) != 2 || labels[0] != "#medic" || labels[1] != "#medic·sess2-me" {
		t.Errorf("labels = %v, want [#medic #medic·sess2-me]", labels)
	}
}

// TestFleetForestRowsCollapsedTeam: a collapsed team is exactly its header
// row, flagged so the renderer can show the member count.
func TestFleetForestRowsCollapsedTeam(t *testing.T) {
	got := fleetForestRows([]agentGroup{medicGroup()}, map[string]bool{"sess-medic": true}, false)
	if len(got) != 1 || !got[0].isHeader || !got[0].collapsed {
		t.Fatalf("rows = %v, want single collapsed header", forestNames(got))
	}
	if len(got[0].members) != 6 {
		t.Errorf("header members = %d, want 6 (aggregates still cover the hidden rows)", len(got[0].members))
	}
}

// TestFleetForestRowsFlatModeHasNoHeaders: flat mode bypasses the forest —
// no headers, v1 flat annotations.
func TestFleetForestRowsFlatModeHasNoHeaders(t *testing.T) {
	got := fleetForestRows([]agentGroup{medicGroup()}, nil, true)
	if len(got) != 6 {
		t.Fatalf("flat rows = %v, want 6", forestNames(got))
	}
	for _, fr := range got {
		if fr.isHeader {
			t.Fatalf("flat rows contain a header: %v", forestNames(got))
		}
	}
	names := forestNames(got)
	if names[2] != "@Explore ↳collector" {
		t.Errorf("flat spawn = %q, want @Explore ↳collector", names[2])
	}
}

// TestFleetEnterTogglesCollapse drives the Update path: enter on a header
// folds the team (cursor stays on the header), enter again reopens; enter
// on an agent row is a no-op.
func TestFleetEnterTogglesCollapse(t *testing.T) {
	m := model{width: 160, height: 24, colorize: true}
	g := medicGroup()
	m.agents.setRows(g.rows)
	v := fleetView{m: &m}

	press := func(k string) {
		var msg tea.KeyMsg
		if k == "enter" {
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		} else {
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		v.Update(msg)
	}

	rows := v.fleetRowsFor()
	if !rows[0].isHeader {
		t.Fatalf("row 0 = %v, want header", forestNames(rows))
	}
	sid := rows[0].sessionID

	press("enter")
	if !m.fleet.collapsed[sid] {
		t.Fatal("team not collapsed after enter on header")
	}
	rows = v.fleetRowsFor()
	if len(rows) != 1 {
		t.Fatalf("collapsed rows = %v, want header only", forestNames(rows))
	}
	if got := m.fleet.reconcile(rows); got != 0 || rows[got].selectionKey() != "session:"+sid {
		t.Errorf("cursor after collapse = %d (%q), want the header", got, rows[got].selectionKey())
	}

	press("enter")
	if m.fleet.collapsed[sid] {
		t.Fatal("team still collapsed after second enter")
	}

	// enter on an agent row: no-op.
	press("j")
	press("enter")
	if m.fleet.collapsed[sid] {
		t.Error("enter on an agent row collapsed the team")
	}
}

// TestFleetTeamHeaderTotalsBoldAndAligned renders the full View and checks
// the header's economics: cost sourced from the lead's durable
// SessionTotalCostUSD (token_ledger session total, operator decision
// 2026-07-13 — deliberately NOT sum(SessionCostUSD) across visible gauge
// rows, since that drops swept/closed teammates), bold on the header total
// ONLY, and the header's $ landing in the same screen column as member
// rows' $ (which still show their own per-agent SessionCostUSD, unchanged).
func TestFleetTeamHeaderTotalsBoldAndAligned(t *testing.T) {
	m := model{width: 140, height: 20, colorize: true}
	g := medicGroup()
	g.rows = g.rows[:2] // lead + collector, simpler sums
	g.rows[0].SessionCostUSD, g.rows[0].TokensInTotal, g.rows[0].TokensOutTotal = 12.3, 39000, 40000
	// Deliberately different from sum(SessionCostUSD) below (12.3+4.6=16.9)
	// so the assertions can prove the header reads THIS field, not a sum.
	g.rows[0].SessionTotalCostUSD = 30.5
	g.rows[1].SessionCostUSD, g.rows[1].TokensInTotal, g.rows[1].TokensOutTotal = 4.6, 13000, 17000
	m.agents.setRows(g.rows)
	v := fleetView{m: &m}

	raw := strings.Split(v.View(140, 18), "\n")
	header, leadRow := raw[2], raw[3]
	if !strings.Contains(header, display.BOLD) {
		t.Errorf("header = %q, want bold escape on its cost total", header)
	}
	if strings.Contains(leadRow, display.BOLD) {
		t.Errorf("lead row = %q, want no bold", leadRow)
	}

	sHeader, sLead := display.StripANSI(header), display.StripANSI(leadRow)
	if !strings.Contains(sHeader, "$31") {
		t.Fatalf("header = %q, want lead's SessionTotalCostUSD $31 (30.5 rounded), not the gauge-row sum $17", sHeader)
	}
	if !strings.Contains(sLead, "$12") {
		t.Fatalf("lead row = %q, want its own SessionCostUSD $12 unchanged", sLead)
	}
	if !strings.Contains(sHeader, "#medic") || !strings.Contains(sHeader, "[ship fleet view multi-team hierarchy]") {
		t.Errorf("header = %q, want #medic label + [objective]", sHeader)
	}
	ci, cj := runeIndex(sHeader, "$31"), runeIndex(sLead, "$12")
	if ci == -1 || cj == -1 || ci != cj {
		t.Errorf("cost column misaligned: header $ at %d, lead $ at %d\nheader=%q\nlead=%q", ci, cj, sHeader, sLead)
	}
	// IN sums: 39k+13k = 52k on the header, 39k on the lead — both 3 chars
	// right-aligned in the same 5-cell column, so equal rune offsets.
	ii, ij := runeIndex(sHeader, "52k"), runeIndex(sLead, "39k")
	if ii == -1 || ij == -1 || ii != ij {
		t.Errorf("IN column misaligned: header at %d, lead at %d\nheader=%q\nlead=%q", ii, ij, sHeader, sLead)
	}
}

// TestFleetTeamHealthDotShape: filled when the team is live with gauge
// data, dim hollow when nothing live or no data. Color mapping is
// healthDotStyle's (already covered); under go test lipgloss strips
// styling, so only the glyph shape is assertable here.
func TestFleetTeamHealthDotShape(t *testing.T) {
	liveWithData := []Agent{
		{AgentName: "a", Liveness: "live", PressureLevel: "warning"},
		{AgentName: "b", Liveness: "idle", PressureLevel: "critical"},
	}
	if got := display.StripANSI(fleetTeamHealthDot(liveWithData)); got != "◉" {
		t.Errorf("live team dot = %q, want ◉", got)
	}
	noneLive := []Agent{{AgentName: "a", Liveness: "closed", PressureLevel: "ok"}}
	if got := display.StripANSI(fleetTeamHealthDot(noneLive)); got != "○" {
		t.Errorf("dead team dot = %q, want ○", got)
	}
	liveNoData := []Agent{{AgentName: "a", Liveness: "live"}}
	if got := display.StripANSI(fleetTeamHealthDot(liveNoData)); got != "○" {
		t.Errorf("no-gauge team dot = %q, want ○", got)
	}
}

// TestFleetLeadRendersAtLead: operator decision 5 — the lead is a member
// row named "@lead", and (2026-07-14) the tree's sole root, so its own
// connector is always "└" rather than a peer "├".
func TestFleetLeadRendersAtLead(t *testing.T) {
	m := model{width: 140, height: 20, colorize: true}
	m.agents.setRows(medicGroup().rows)
	v := fleetView{m: &m}
	out := display.StripANSI(v.View(140, 18))
	if !strings.Contains(out, "└@lead") {
		t.Errorf("view output missing └@lead:\n%s", out)
	}
}

// TestFleetForestRowsGrandchildSpawnNestsUnderTeammate pins the exact
// scenario from the operator's bug report: a teammate's own Agent-tool
// spawn (a "sub-subagent" two levels below the lead) must keep nesting
// correctly now that the teammate itself sits one level deeper than
// before. Shape: #aka-crew — @lead (root), @wms-engine and @docs as its
// teammates, @wms-child as wms-engine's own spawn.
func TestFleetForestRowsGrandchildSpawnNestsUnderTeammate(t *testing.T) {
	lead := treeAgent("", "L", "", "lead")
	lead.TeamName = "aka-crew"
	g := agentGroup{sessionID: "sess-aka", rows: []Agent{
		lead,
		treeAgent("wms-engine", "W", "L", "teammate"),
		treeAgent("wms-child", "X", "W", "subagent"),
		treeAgent("docs", "D", "L", "teammate"),
	}}
	got := forestNames(fleetForestRows([]agentGroup{g}, nil, false))
	want := []string{
		"H:#aka-crew",
		"└@lead",
		" ├@wms-engine",
		" │ └─@wms-child",
		" └@docs",
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFleetForestRowsDepthCapUnderTeammate: fleetMaxTreeDepth still caps a
// spawn chain rooted under a TEAMMATE (not just one rooted directly under
// the lead, which TestFleetTreeRowsDepthCapped already covers) — teammate
// nesting spends one level of the same budget, so this exercises the cap
// through the team-header path (fleetTeamMemberRows no longer touches
// descendant prefixes at all, so this proves fleetTreeRows' own capping is
// sufficient on its own).
func TestFleetForestRowsDepthCapUnderTeammate(t *testing.T) {
	lead := treeAgent("", "L", "", "lead")
	lead.TeamName = "deep-crew"
	g := agentGroup{sessionID: "sess-deep", rows: []Agent{
		lead,
		treeAgent("collector", "C", "L", "teammate"),
		treeAgent("d1", "A", "C", "subagent"),
		treeAgent("d2", "B", "A", "subagent"),
		treeAgent("d3", "D3", "B", "subagent"),
		treeAgent("d4", "D4", "D3", "subagent"),
	}}
	got := fleetForestRows([]agentGroup{g}, nil, false)
	// index 0 = header, 1 = lead, 2 = collector, 3 = d1, 4 = d2, 5 = d3, 6 = d4.
	// collector itself already spends one level of the cont budget (unlike
	// TestFleetTreeRowsDepthCapped's chain, which hangs directly off the
	// root lead), so d2 — not d3 — is the one that lands exactly at the cap.
	capW := lipgloss.Width(got[4].prefix) // d2 — at the cap (cont length 3)
	for _, i := range []int{5, 6} {       // d3, d4 — beyond the cap
		if w := lipgloss.Width(got[i].prefix); w != capW {
			t.Errorf("row %d prefix %q width = %d, want capped at %d (d2's %q)", i, got[i].prefix, w, capW, got[4].prefix)
		}
	}
}
