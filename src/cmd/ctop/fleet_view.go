package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/tui"
)

// fleetModel is the Fleet view's own state: its cursor over the derived
// forest row list (see fleetForestRows — team header rows plus their
// nested members), independent of the health view's own session
// collapse/expand state. AGE reads straight from
// agentsModel.activityTsFor (see renderAgentRow) — no separate first-seen
// tracking of its own. flat toggles the whole forest off ("t" key): the
// pure AgentAF flat list, headers gone, lineage kept as a dim ↳parent
// suffix. collapsed tracks which teams the operator has folded to just
// their header row ("enter"), keyed by sessionID — default (absent) is
// expanded, per operator decision 2026-07-13.
type fleetModel struct {
	cursor    int
	cursorKey string // fleetRow.selectionKey() under the cursor, reconciled against row reorders
	flat      bool   // "t" toggle: true = no headers/indentation, ↳parent suffix instead
	collapsed map[string]bool
}

// reconcile returns the cursor index for the current row order: the row
// matching cursorKey if it still exists, else the stored index clamped.
func (a *fleetModel) reconcile(rows []fleetRow) int {
	if a.cursorKey != "" {
		for i, r := range rows {
			if r.selectionKey() == a.cursorKey {
				return i
			}
		}
	}
	if a.cursor >= len(rows) {
		return 0
	}
	return a.cursor
}

// setCursor commits a new cursor position, re-deriving cursorKey so the
// selection survives the next poll's reordering.
func (a *fleetModel) setCursor(rows []fleetRow, idx int) {
	a.cursor = idx
	if idx >= 0 && idx < len(rows) {
		a.cursorKey = rows[idx].selectionKey()
	} else {
		a.cursorKey = ""
	}
}

// --- spawn-tree derivation ---

// fleetRow is one display line of the Fleet view's derived forest: a team
// header, or an agent with its lineage decoration. For agent rows, prefix
// is the plain uncolored connector ("├" member, "│ ├─" spawn, "↳" orphan)
// and suffix is flat mode's " ↳parent" annotation — both render dim
// inside the AGENT cell; at most one is ever set. Header rows carry the
// team identity and their visible members for aggregate rendering; agent
// is the group's lead there (runtime-icon source).
type fleetRow struct {
	agent       Agent
	prefix      string
	suffix      string
	teamMaxCost float64 // agent rows only: highest SessionCostUSD among this row's group, for the COST column's per-group green gradient

	isHeader  bool
	sessionID string  // header only: collapse key + cursor identity
	label     string  // header only: "#team" (·prefix8-disambiguated) or sessionAlias fallback
	focus     string  // header only: objective text, "" to omit the bracket
	members   []Agent // header only: visible member rows, for aggregates
	collapsed bool    // header only
}

// selectionKey is the cursor identity for a row: headers key by session
// (View 1's "session:<id>" convention), agents by their SelectionKey — so
// the cursor survives polls, collapses, and tree/flat toggles.
func (fr fleetRow) selectionKey() string {
	if fr.isHeader {
		return "session:" + fr.sessionID
	}
	return fr.agent.SelectionKey()
}

// fleetMaxTreeDepth caps the indentation depth: rows nested deeper keep
// their branch glyph but render at this depth, so a runaway spawn chain
// can't push names out of the AGENT column.
const fleetMaxTreeDepth = 3

// fleetIsLead reports whether a is a session lead — by roster relationship
// when labeled, else by the lead's defining empty agent_name.
func fleetIsLead(a Agent) bool {
	return a.Relationship == "lead" || a.AgentName == ""
}

// fleetWantsNest reports whether r should nest beneath its spawner rather
// than sit top-level. Labeling-robust by design (approved 2026-07-13):
// relationship=="subagent" nests wherever hookd's Gate 1 refinement labels
// one; a parent_ref resolving to a visible NON-lead row nests regardless
// of label (covers grandchildren before the refinement lands). Teammates
// of a lead stay flat — they are peers in the team mental model — which
// also means today's degraded labeling (everything "teammate" with
// parent_ref → lead) renders identically to the pre-hierarchy view.
func fleetWantsNest(r Agent, parent *Agent) bool {
	if r.Relationship == "subagent" {
		return true
	}
	return parent != nil && !fleetIsLead(*parent)
}

// fleetConnector renders the plain tree-lineage prefix for a nested row.
// cont holds one continuation flag per ancestor level (true = that
// ancestor has more siblings below it, so its guide line "│" continues).
// Depth beyond fleetMaxTreeDepth drops the OLDEST continuation cells —
// the branch glyph and local structure stay intact.
func fleetConnector(cont []bool, orphan bool) string {
	if orphan {
		return "↳"
	}
	if len(cont) == 0 {
		return ""
	}
	if len(cont) > fleetMaxTreeDepth {
		cont = cont[len(cont)-fleetMaxTreeDepth:]
	}
	var b strings.Builder
	for _, c := range cont[:len(cont)-1] {
		if c {
			b.WriteString("│ ")
		} else {
			b.WriteString("  ")
		}
	}
	if cont[len(cont)-1] {
		b.WriteString("├─")
	} else {
		b.WriteString("└─")
	}
	return b.String()
}

// fleetTreeRows derives the Fleet view's display list from the
// already-sorted flat agent list. Tree mode (flat=false): top-level rows
// (leads, teammates, peers — anything that doesn't nest per
// fleetWantsNest) keep the input's sort order; each parent's children are
// pulled up directly beneath it, ordered among themselves by that same
// sort. A row that should nest but whose parent_ref doesn't resolve to a
// visible row is promoted to top level with the "↳" orphan marker — a
// child never disappears just because its parent did. Rows unreachable
// from any top-level root (a parent_ref cycle — bad data) are promoted
// the same way rather than dropped or looped on. Flat mode: input order
// untouched, nested-eligible rows annotated with a " ↳parent" suffix
// instead.
func fleetTreeRows(rows []Agent, flat bool) []fleetRow {
	byRoster := make(map[string]int, len(rows))
	for i, r := range rows {
		if r.RosterID != nil && *r.RosterID != "" {
			byRoster[*r.RosterID] = i
		}
	}
	parentIdx := func(r Agent) (int, bool) {
		if r.ParentRef == nil || *r.ParentRef == "" {
			return 0, false
		}
		i, ok := byRoster[*r.ParentRef]
		return i, ok
	}

	if flat {
		out := make([]fleetRow, 0, len(rows))
		for _, r := range rows {
			var parent *Agent
			if pi, ok := parentIdx(r); ok {
				parent = &rows[pi]
			}
			switch {
			case !fleetWantsNest(r, parent):
				out = append(out, fleetRow{agent: r})
			case parent == nil:
				out = append(out, fleetRow{agent: r, prefix: "↳"})
			default:
				pname := strings.TrimPrefix(parent.AgentName, "@")
				if pname == "" {
					pname = "lead"
				}
				out = append(out, fleetRow{agent: r, suffix: " ↳" + pname})
			}
		}
		return out
	}

	children := make(map[int][]int)
	orphan := make(map[int]bool)
	var roots []int
	for i, r := range rows {
		pi, ok := parentIdx(r)
		var parent *Agent
		if ok {
			parent = &rows[pi]
		}
		switch {
		case !fleetWantsNest(r, parent):
			roots = append(roots, i)
		case parent == nil:
			orphan[i] = true
			roots = append(roots, i)
		default:
			children[pi] = append(children[pi], i)
		}
	}

	out := make([]fleetRow, 0, len(rows))
	emitted := make(map[int]bool, len(rows))
	var walk func(i int, cont []bool)
	walk = func(i int, cont []bool) {
		if emitted[i] {
			return
		}
		emitted[i] = true
		out = append(out, fleetRow{agent: rows[i], prefix: fleetConnector(cont, orphan[i])})
		kids := children[i]
		for k, ci := range kids {
			next := append(append([]bool{}, cont...), k < len(kids)-1)
			walk(ci, next)
		}
	}
	for _, i := range roots {
		walk(i, nil)
	}
	for i := range rows { // cycle fallback — see doc comment
		if !emitted[i] {
			orphan[i] = true
			walk(i, nil)
		}
	}
	return out
}

// fleetDisplayName is the AGENT-cell name for a row — shared by
// renderAgentRow and fleetAgentColWidth so the width calc can never
// disagree with the render. The lead renders "@lead" (operator decision
// 2026-07-13): under the v2 forest it's a member row among members, and
// the @ keeps the member column visually uniform.
func fleetDisplayName(r Agent) string {
	if r.AgentName == "" {
		return "@lead"
	}
	if !strings.HasPrefix(r.AgentName, "@") {
		return "@" + r.AgentName
	}
	return r.AgentName
}

// fleetRowName is fleetDisplayName applied to a row's agent.
func fleetRowName(fr fleetRow) string {
	return fleetDisplayName(fr.agent)
}

// fleetAgentColWidth sizes the AGENT column for the derived row list:
// widest prefix+name+suffix among agent rows (headers are free-flow and
// don't participate), floor 8, cap 18 (up from the AGENT view's 12 — tree
// indentation needs the extra room; ACTIVITY absorbs the difference via
// fleetLayoutFor's remainder budgeting).
func fleetAgentColWidth(rows []fleetRow) int {
	w := 8
	for _, fr := range rows {
		if fr.isHeader {
			continue
		}
		n := lipgloss.Width(fr.prefix) + lipgloss.Width(fleetRowName(fr)) + lipgloss.Width(fr.suffix)
		if n > w {
			w = n
		}
	}
	if w > 18 {
		w = 18
	}
	return w
}

// --- team forest derivation (v2) ---

// fleetTeamMemberRows converts one team's v1 spawn tree into member-level
// rows per the v2 connector grammar (operator-approved mockup A): members
// — lead first, teammates after — get a single dash-less connector
// ("├"/"└"), Agent-tool spawns keep their v1 dashed connectors nested one
// level deeper under a "│ "/"  " guide that tracks whether their owning
// member has more siblings below. A within-team orphan (spawn whose
// parent isn't visible) keeps its ↳ marker after the member connector.
func fleetTeamMemberRows(rows []Agent) []fleetRow {
	tree := fleetTreeRows(rows, false)
	var topIdx []int
	for i, fr := range tree {
		if fr.prefix == "" || fr.prefix == "↳" {
			topIdx = append(topIdx, i)
		}
	}
	for k, ti := range topIdx {
		conn, guide := "├", "│ "
		if k == len(topIdx)-1 {
			conn, guide = "└", "  "
		}
		if tree[ti].prefix == "↳" {
			tree[ti].prefix = conn + "↳"
		} else {
			tree[ti].prefix = conn
		}
		end := len(tree)
		if k+1 < len(topIdx) {
			end = topIdx[k+1]
		}
		for j := ti + 1; j < end; j++ {
			tree[j].prefix = guide + tree[j].prefix
		}
	}
	return tree
}

// fleetTeamAggregates sums the header row's token totals across a team's
// visible members. Cost is NOT summed here (operator decision 2026-07-13):
// summing per-agent SessionCostUSD across only the currently-VISIBLE gauge
// rows silently drops any teammate swept for going idle >2h, or closed
// before a pricing-table fix ever re-priced their frozen row — see
// renderTeamHeaderRow, which instead reads the lead's SessionTotalCostUSD
// (a durable token_ledger SUM covering every agent_name for the session,
// swept or not).
func fleetTeamAggregates(members []Agent) (in, out int64) {
	for _, r := range members {
		in += r.TokensInTotal
		out += r.TokensOutTotal
	}
	return in, out
}

// maxSessionCost returns the highest SessionCostUSD among rows — the COST
// column gradient's comparison ceiling within a team (see fleetRow.teamMaxCost).
func maxSessionCost(rows []Agent) float64 {
	var max float64
	for _, r := range rows {
		if r.SessionCostUSD > max {
			max = r.SessionCostUSD
		}
	}
	return max
}

// fleetTeamHealthDot renders the header's aggregate health dot: the WORST
// pressure_level among members with gauge data (critical > warning > ok),
// filled and colored via the same healthDotStyle mapping the per-agent dot
// uses. A team with no live member, or none with gauge data, gets the dim
// hollow "○" — nothing trustworthy to report.
func fleetTeamHealthDot(members []Agent) string {
	live, hasData := false, false
	worst := ""
	rank := func(level string) int {
		switch level {
		case "critical":
			return 2
		case "warning":
			return 1
		default:
			return 0
		}
	}
	for _, r := range members {
		if r.Liveness == "live" || r.Liveness == "idle" {
			live = true
		}
		if r.PressureLevel != "" || r.CollectorStatus != "" {
			hasData = true
			if rank(r.PressureLevel) > rank(worst) {
				worst = r.PressureLevel
			}
		}
	}
	if !live || !hasData {
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("○")
	}
	return healthDotStyle(worst).Render("◉")
}

// fleetForestRows derives the v2 display list: one header row per session
// (recency-ordered — groups arrive already sorted by agentsModel), its
// members and spawns nested beneath unless collapsed. EVERY session gets a
// header (operator correction 2026-07-13: every session is a grouping unit,
// including a bare single-agent one — the earlier "solo sessions get no
// header" rule was wrong) — a teamless solo session is labeled
// "solo·<prefix8>" with its own focus in the bracket, a teamless
// MULTI-agent session is labeled via sessionAlias's fallback chain (whose
// label already embeds a truncated focus, so no separate bracket). Two
// sessions sharing a team name are disambiguated with a ·<prefix8> label
// suffix from the second onward. Flat mode bypasses the forest entirely:
// v1's flat list over every visible agent, in group order.
func fleetForestRows(groups []agentGroup, collapsed map[string]bool, flat bool) []fleetRow {
	if flat {
		// No team boundaries survive the flatten, so the COST gradient's
		// comparison group widens to every visible agent.
		visible := fleetVisibleRows(flattenGroups(groups))
		maxCost := maxSessionCost(visible)
		rows := fleetTreeRows(visible, true)
		for i := range rows {
			rows[i].teamMaxCost = maxCost
		}
		return rows
	}

	seenTeam := make(map[string]int)
	var out []fleetRow
	for _, g := range groups {
		rows := fleetVisibleRows(g.rows)
		if len(rows) == 0 {
			continue
		}
		lead := rows[0] // lead-first per sortGroupMembers/pinLeadsFirst
		maxCost := maxSessionCost(rows)

		team := lead.TeamName
		var label, focus string
		switch {
		case team != "":
			label = "#" + team
			if seenTeam[team] > 0 {
				label += "·" + sessionPrefix8(g.sessionID)
			}
			seenTeam[team]++
			focus = sanitizeActivity(lead.CurrentFocus)
		case len(rows) > 1:
			label = sessionAlias("", g.sessionID, true, lead.CurrentFocus)
		default:
			// Bare solo session: sessionAlias's isMultiAgent=false branch
			// gives "solo·<prefix8>" — unlike the multi-agent case, that
			// label carries no focus text of its own, so the objective
			// still earns its own bracket.
			label = sessionAlias("", g.sessionID, false, lead.CurrentFocus)
			focus = sanitizeActivity(lead.CurrentFocus)
		}

		out = append(out, fleetRow{
			agent:     lead,
			isHeader:  true,
			sessionID: g.sessionID,
			label:     label,
			focus:     focus,
			members:   rows,
			collapsed: collapsed[g.sessionID],
		})
		if !collapsed[g.sessionID] {
			members := fleetTeamMemberRows(rows)
			for i := range members {
				members[i].teamMaxCost = maxCost
			}
			out = append(out, members...)
		}
	}
	return out
}

// --- CTX gradient ---

// ctxGradientAnchors are the interpolation stops for the Fleet view's CTX
// temperature ramp: green at low fill through yellow and orange to red at
// high fill. Hues match the tui palette (ColorSuccess / #e3b341 /
// GitHub-orange / ColorError) rather than pure RGB primaries, so the view
// stays in-family with the rest of ctop.
var ctxGradientAnchors = []struct {
	pos float64
	rgb [3]int
}{
	{0.00, successRGB},               // #3fb950 green
	{0.45, [3]int{0xe3, 0xb3, 0x41}}, // #e3b341 yellow
	{0.70, [3]int{0xf0, 0x88, 0x3e}}, // #f0883e orange
	{0.90, errorRGB},                 // #f85149 red
}

// ctxGradientRGB interpolates the CTX ramp at fill (0..1, clamped). Unlike
// the health view's stepped ctxPctColor, this is a continuous truecolor
// blend — the Fleet view's percentage and bar shift smoothly from green
// toward red as pressure rises.
func ctxGradientRGB(fill float64) [3]int {
	if fill <= ctxGradientAnchors[0].pos {
		return ctxGradientAnchors[0].rgb
	}
	last := ctxGradientAnchors[len(ctxGradientAnchors)-1]
	if fill >= last.pos {
		return last.rgb
	}
	for i := 1; i < len(ctxGradientAnchors); i++ {
		lo, hi := ctxGradientAnchors[i-1], ctxGradientAnchors[i]
		if fill > hi.pos {
			continue
		}
		t := (fill - lo.pos) / (hi.pos - lo.pos)
		return [3]int{
			lo.rgb[0] + int(math.Round(t*float64(hi.rgb[0]-lo.rgb[0]))),
			lo.rgb[1] + int(math.Round(t*float64(hi.rgb[1]-lo.rgb[1]))),
			lo.rgb[2] + int(math.Round(t*float64(hi.rgb[2]-lo.rgb[2]))),
		}
	}
	return last.rgb
}

// --- AGE gradient ---

// fleetAgeTerminalRGB is the AGE column's color for a row that is NOT
// actively processing (idle/closed): a flat brown/dim-amber regardless of
// elapsed time — an idle agent's age isn't an urgency signal the way a
// long-running active turn's is, so it deliberately doesn't ramp.
var fleetAgeTerminalRGB = [3]int{139, 90, 43}

// fleetAgeGradientAnchors are the interpolation stops for the AGE column's
// active-state ramp, keyed on elapsed minutes rather than a 0..1 fraction
// (unlike ctxGradientAnchors): dim green while a turn is fresh, escalating
// through yellow and orange to red as it runs long. Same hue family as the
// CTX ramp, anchored at 5/10/15 minutes per operator spec.
var fleetAgeGradientAnchors = []struct {
	mins float64
	rgb  [3]int
}{
	{0, halveRGB(successRGB)},     // dim green
	{5, [3]int{0xe3, 0xb3, 0x41}}, // #e3b341 yellow
	{10, [3]int{0xf0, 0x88, 0x3e}}, // #f0883e orange
	{15, errorRGB},                // #f85149 red
}

// fleetAgeColor picks the AGE column's text color: fleetAgeTerminalRGB when
// the row isn't actively processing right now (see agentsModel.midTurnFor —
// the same signal the ACTIVITY dot's own blink keys on), else a continuous
// truecolor blend along fleetAgeGradientAnchors so a long-running active
// turn visibly heats up from green through red.
func fleetAgeColor(age time.Duration, isProcessing bool) [3]int {
	if !isProcessing {
		return fleetAgeTerminalRGB
	}
	mins := age.Minutes()
	if mins <= fleetAgeGradientAnchors[0].mins {
		return fleetAgeGradientAnchors[0].rgb
	}
	last := fleetAgeGradientAnchors[len(fleetAgeGradientAnchors)-1]
	if mins >= last.mins {
		return last.rgb
	}
	for i := 1; i < len(fleetAgeGradientAnchors); i++ {
		lo, hi := fleetAgeGradientAnchors[i-1], fleetAgeGradientAnchors[i]
		if mins > hi.mins {
			continue
		}
		t := (mins - lo.mins) / (hi.mins - lo.mins)
		return [3]int{
			lo.rgb[0] + int(math.Round(t*float64(hi.rgb[0]-lo.rgb[0]))),
			lo.rgb[1] + int(math.Round(t*float64(hi.rgb[1]-lo.rgb[1]))),
			lo.rgb[2] + int(math.Round(t*float64(hi.rgb[2]-lo.rgb[2]))),
		}
	}
	return last.rgb
}

// fleetCtxBarCells is the Fleet view's context-fill bar width.
const fleetCtxBarCells = 12

// fleetCtxBar renders the Fleet view's horizontal fill bar: each filled cell
// colored by the gradient at that cell's own position (so a hot bar reads
// green→red along its length), empty cells dim. Follows fillBar's
// convention for untrustworthy data: fill > 1.0 renders as a dim "--"
// instead of asserting a wrong number.
func fleetCtxBar(fill float64, dim rowDim) string {
	if fill > 1.0 {
		return padTrunc(unreliableFill(), fleetCtxBarCells)
	}
	if fill < 0 {
		fill = 0
	}
	filled := int(math.Round(fill * fleetCtxBarCells))
	var b strings.Builder
	for i := 0; i < filled; i++ {
		p := (float64(i) + 0.5) / fleetCtxBarCells
		b.WriteString(renderDim(ctxGradientRGB(p), "█", dim))
	}
	if filled < fleetCtxBarCells {
		b.WriteString(renderDim(dimGreyRGB, strings.Repeat("░", fleetCtxBarCells-filled), dim))
	}
	return b.String()
}

// fleetModelAbbrev is the Fleet view's MODEL column: strip the "claude-"
// prefix only — no family-letter/version abbreviation — "claude-opus-4-6"
// -> "opus-4-6", "claude-sonnet-5" -> "sonnet-5", "claude-fable-5" ->
// "fable-5". fleetModelW (8) fits every current family name in full;
// anything longer hard-truncates (no ellipsis — at this width an ellipsis
// would eat a large fraction of the column). Empty renders "--". Wider and
// more readable than the old letter+version abbreviation now that MODEL
// sits in a more prominent position (before the status dot, operator
// request) rather than squeezed for CTX-bar/ACTIVITY room.
func fleetModelAbbrev(model string) string {
	if model == "" {
		return "--"
	}
	base := model
	if i := strings.Index(base, "[1m]"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimPrefix(base, "claude-")
	r := []rune(base)
	if len(r) > fleetModelW {
		r = r[:fleetModelW]
	}
	return string(r)
}

// --- view ---

// fleetHeaderTintDivisor darkens a team's EntityColor hash down to a
// background tint — dark enough to stay out of the way, but each team's
// own hue still shows through faintly so headers read as distinct rows
// rather than a single flat grey shared with (and easily confused for) the
// CTX bar's own dim grey (operator report 2026-07-13). /9 read as too dark
// to tell hues apart at a glance — human color perception loses saturation
// fast near-black — so this was raised to /6 (still subtle, per operator's
// own suggested range of /7-/6) for a visibly-tinted rather than
// visibly-grey header row.
const fleetHeaderTintDivisor = 6

// fleetHeaderTintRGB derives a team header row's background tint from the
// same colorSrc that drives its label's foreground hash (see
// renderTeamHeaderRow) — correlating the two means a team's header
// background and its label color are always the same hue family, just at
// very different brightness.
func fleetHeaderTintRGB(colorSrc string) string {
	c := display.EntityColor(colorSrc, "")
	return display.BGRGB(uint8(c[0]/fleetHeaderTintDivisor), uint8(c[1]/fleetHeaderTintDivisor), uint8(c[2]/fleetHeaderTintDivisor))
}

// fleetView is View 4 — ctop's default screen (operator decision
// 2026-07-13): the multi-team fleet overview (v2 forest) — a slim global
// stats band, then one collapsible tree per session — a header row
// (runtime icon, session label + objective, BOLD durable session cost
// total) with its members (lead first, "├"/"└") and their Agent-tool spawns
// ("├─"/"└─", "│ " guides) nested beneath, every session getting a header
// including a bare solo one — a grand TOTAL footer, and a flowing activity
// log beneath it all (see fleetBodyHeights/View). It reads entirely from
// the root model's agents/activity data (m.agents.groups, m.activity) — no
// fetching of its own.
type fleetView struct{ m *model }

func (v fleetView) Title() string { return "fleet" }

func (v fleetView) KeyBindings() string {
	return "FLEET\n" +
		"  j/k, ↓/↑         move cursor\n" +
		"  g / G            first / last row\n" +
		"  enter            collapse/expand the team under the cursor\n" +
		"  s                cycle sort (pressure → fill → name → last-activity)\n" +
		"  r                cycle max-age filter\n" +
		"  t                toggle team tree / flat (↳parent) lineage\n" +
		"\n" +
		"ACTIVITY LOG (bottom) always follows live — switch to health (1) to scroll/pause/filter it"
}

// fleetRowsFor derives the view's current display list — the single
// derivation both Update (cursor math) and View (rendering) go through, so
// the two can never disagree about row order or membership.
func (v fleetView) fleetRowsFor() []fleetRow {
	return fleetForestRows(v.m.agents.groups, v.m.fleet.collapsed, v.m.fleet.flat)
}

func (v fleetView) Update(msg tea.Msg) (view, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return v, nil
	}
	rows := v.fleetRowsFor()
	idx := v.m.fleet.reconcile(rows)
	switch key.String() {
	case "j", "down":
		if idx < len(rows)-1 {
			idx++
		}
	case "k", "up":
		if idx > 0 {
			idx--
		}
	case "g":
		idx = 0
	case "G":
		if len(rows) > 0 {
			idx = len(rows) - 1
		}
	case "s":
		v.m.agents.cycleSort()
		rows = v.fleetRowsFor()
	case "r":
		v.m.agents.cycleAgePreset()
		rows = v.fleetRowsFor()
	case "t":
		v.m.fleet.flat = !v.m.fleet.flat
		rows = v.fleetRowsFor()
		// Re-reconcile by cursorKey so the highlight follows the SAME agent
		// to its new position in the reordered list, not the same index.
		idx = v.m.fleet.reconcile(rows)
	case "enter":
		// Collapse/expand the team header under the cursor. No-op on agent
		// rows and in flat mode (no headers to fold there).
		if idx < 0 || idx >= len(rows) || !rows[idx].isHeader {
			return v, nil
		}
		if v.m.fleet.collapsed == nil {
			v.m.fleet.collapsed = make(map[string]bool)
		}
		sid := rows[idx].sessionID
		v.m.fleet.collapsed[sid] = !v.m.fleet.collapsed[sid]
		rows = v.fleetRowsFor()
		idx = v.m.fleet.reconcile(rows)
	default:
		return v, nil
	}
	if idx >= len(rows) {
		idx = 0
	}
	v.m.fleet.setCursor(rows, idx)
	return v, nil
}

// fleetVisibleRows drops the "@" ghost agent — an unnamed subagent spawn
// with no real identity, all-zero metrics — from the Fleet view's flat
// agent list. Fleet-view scoped only (a filtered copy, not a mutation of
// m.agents.rows): the AGENT view's session-grouped grid is unaffected.
func fleetVisibleRows(rows []Agent) []Agent {
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		if r.AgentName == "@" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// headerBand renders the view's full-width top strip — GLOBAL stats only
// (agent count, dispatch count, burn rate): team identity and focus moved
// into the in-grid team header rows (v2 forest), so repeating them here
// would be dead weight. Plain foreground colors on the default terminal
// background.
func (v fleetView) headerBand(width int, now time.Time) string {
	m := v.m
	rows := fleetVisibleRows(m.agents.rows)

	active, dispatched := 0, 0
	for _, r := range rows {
		if r.Liveness == "live" || r.Liveness == "idle" {
			active++
		}
		if m.agents.midTurnFor(r, now) {
			dispatched++
		}
	}

	dStyle := lipgloss.NewStyle().Foreground(tui.ColorDim)
	if dispatched > 0 {
		dStyle = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorSuccess)
	}
	parts := []string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#e3b341")).Render("fleet"),
		fmt.Sprintf("Agents: %d of %d", active, len(rows)),
		dStyle.Render(fmt.Sprintf("D:%d", dispatched)),
	}

	burn := "C:—/hr"
	if rate, ok := burnRate(m.costHistory, now); ok {
		burn = fmt.Sprintf("C:$%.2f/hr", rate)
	}
	right := lipgloss.NewStyle().Foreground(tui.ColorMetric).Render(burn)

	left := " " + strings.Join(parts, "  ")
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
	}
	line := display.TruncateLine(left+strings.Repeat(" ", gap)+right+" ", width)
	return padTrunc(line, width)
}

// fleetColSet is the Fleet view's width-degradation ladder: the CTX bar drops
// first — the dots/model/name/age/activity/economics core survives down to
// model.View's own 40-col floor.
type fleetColSet struct {
	bar bool
}

func fleetColumnsForWidth(w int) fleetColSet {
	return fleetColSet{bar: w >= 80}
}

const (
	fleetActivityDotW = 2 // activityGlyph's glyph + its own baked-in trailing space
	fleetModelW       = 8 // fits "opus-4-6"/"sonnet-5"/"fable-5"/"haiku-4" in full
	fleetCtxW         = 4
	fleetCostW        = 5
	fleetTokW         = 5
	fleetAgeW         = 4
)

// fleetMinActivityW is the floor ACTIVITY is allowed to shrink to (see
// fleetLayoutFor's squeeze step) before the CTX bar gets dropped to free up
// more room — below this, a truncated activity string reads as noise, not
// information.
const fleetMinActivityW = 20

// fleetLayout is the Fleet view's per-render column-width budget: every
// column is fixed except ACTIVITY, whose width is whatever's left over
// after every other column and gap is subtracted from the panel width —
// computed once per View() call and threaded into the header/row/total
// renderers so they can never disagree about where ACTIVITY ends and the
// economics block begins.
type fleetLayout struct {
	agentW     int // AGENT column — agentsModel.agentColWidth()'s existing dynamic sizing
	activityW  int // ACTIVITY text budget — the remainder after fixed columns+gaps
	totalFixed int // sum of every fixed column+gap (excludes ACTIVITY) — width - totalFixed == activityW, clamped at 0
}

// fleetFixedWidth sums every column and gap in one row EXCEPT the ACTIVITY
// text — literally the same separators fleetAssemble concatenates, so the
// two must be kept in sync by construction.
func fleetFixedWidth(agentW int, bar bool) int {
	w := 1 /* marker */ + fleetActivityDotW + 1 /* sep before MODEL */ + fleetModelW + 1 /* sep */ +
		1 /* health dot */ + 1 /* sep */ + agentW + 2 /* sep */ +
		fleetAgeW + 2 /* sep before activity */ + 2 /* sep after activity */ +
		fleetCtxW + 2 /* sep */ + fleetCostW + 1 /* sep */ + fleetTokW + 1 /* sep */ + fleetTokW
	if bar {
		w += 1 + fleetCtxBarCells // " " + bar
	}
	return w
}

// fleetLayoutFor computes the ACTIVITY-column budget for one render pass.
// cs is already width-gated by fleetColumnsForWidth on entry; if the
// resulting activityW would fall below fleetMinActivityW, this drops the
// CTX bar too (freeing fleetCtxBarCells+1 cells) and recomputes once more
// before finally clamping activityW at 0 for a pathologically narrow
// terminal — the header/row/total renderers all still degrade gracefully
// at 0 (padTrunc of anything to width 0 is just "").
func fleetLayoutFor(width, agentW int, cs fleetColSet) (fleetColSet, fleetLayout) {
	fixed := fleetFixedWidth(agentW, cs.bar)
	activityW := width - fixed
	if activityW < fleetMinActivityW && cs.bar {
		cs.bar = false
		fixed = fleetFixedWidth(agentW, cs.bar)
		activityW = width - fixed
	}
	if activityW < 0 {
		activityW = 0
	}
	return cs, fleetLayout{agentW: agentW, activityW: activityW, totalFixed: fixed}
}

// fleetAssemble joins one row's already-padded, FIXED-width cells with the
// view's fixed gaps — shared by the column header, every data row, and the
// TOTAL row so their columns can never drift apart. Column order: marker,
// activity dot, a sep, MDL, health dot, AGENT, AGE, ACTIVITY text, then the
// CTX/[bar]/$/IN/OUT economics block — MODEL sits before the health dot
// (operator request, more prominent than squeezed after it). The sep after
// the activity dot is a real column gap, not just activityDot's own
// baked-in trailing space (fleetActivityDotW) — without it the glyph read
// as jammed against MODEL. Only TWO dots — no runtime dot here (operator:
// noise when every agent is the same runtime; dropped from this view only,
// the AGENT view's ST column is unaffected). ACTIVITY sits in the MIDDLE of
// the row (operator-specified reorder) but is no longer variable-width
// itself — callers padTrunc it to fleetLayout.activityW first (see
// fleetLayoutFor) — so this is a plain concatenation, same as every other
// column; nothing needs to anchor to the panel's far edge separately.
func fleetAssemble(marker, activityDot, model, healthDot, name, age, activity, ctx, bar, cost, in, out string, cs fleetColSet) string {
	var b strings.Builder
	b.WriteString(marker + activityDot + " " + model + " " + healthDot + " " + name + "  " + age + "  " + activity + "  " + ctx)
	if cs.bar {
		b.WriteString(" " + bar)
	}
	b.WriteString("  " + cost + " " + in + " " + out)
	return b.String()
}

// fleetHealthDot renders the Fleet view's health dot, keyed on
// pressure_level (context-pressure signal) rather than liveness — the
// AGENT view's healthDot/statusDots key on liveness, but a data bug
// currently reports liveness=="closed" for agents that are actually alive
// (operator report), which made every Fleet-view health dot render hollow
// regardless of actual health. Filled "◉", colored by pressure — reuses
// format.go's healthDotStyle (critical red, warning yellow, anything else
// including "ok" or an agent with no pressure alert yet green), the same
// mapping the AGENT view's own health dot already uses, just applied to a
// different glyph. Callers show the dim hollow "○" instead of calling this
// at all when an agent has no gauge data whatsoever (see renderAgentRow) —
// that's a distinct "no signal" case this function itself doesn't need to
// know about.
func fleetHealthDot(pressureLevel string) string {
	return healthDotStyle(pressureLevel).Render("◉")
}

// fleetCostGradientLowRGB/fleetCostGradientHighRGB are the COST column's
// per-group gradient endpoints: dim green for a spend near zero relative to
// the group's biggest spender, vivid green for the biggest spender itself —
// makes the highest-cost agent in a team pop at a glance.
var (
	fleetCostGradientLowRGB  = halveRGB(successRGB)
	fleetCostGradientHighRGB = [3]int{110, 255, 130}
)

// fleetCostGradientRGB scales cost's color from dim to vivid green relative
// to maxCost (fleetRow.teamMaxCost — the group's highest member cost), on a
// log scale so a $1-vs-$5 spread still reads clearly even when a $100
// outlier is also in the group (a linear scale would compress everything
// below it near zero). cost<=0 or maxCost<=0 — nothing to compare against —
// renders the dim floor.
func fleetCostGradientRGB(cost, maxCost float64) [3]int {
	if cost <= 0 || maxCost <= 0 {
		return fleetCostGradientLowRGB
	}
	t := math.Log(cost+1) / math.Log(maxCost+1)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	lo, hi := fleetCostGradientLowRGB, fleetCostGradientHighRGB
	return [3]int{
		lo[0] + int(math.Round(t*float64(hi[0]-lo[0]))),
		lo[1] + int(math.Round(t*float64(hi[1]-lo[1]))),
		lo[2] + int(math.Round(t*float64(hi[2]-lo[2]))),
	}
}

// fleetCostCell renders the COST column's value: bold and a fixed metric
// color for the team header row, where cost is the lead's durable
// SessionTotalCostUSD (see renderTeamHeaderRow) rather than a per-agent
// figure — maxCost is ignored there. Every member row, lead
// included, renders normal weight, colored by fleetCostGradientRGB against
// maxCost instead — each agent now shows its OWN recomputed cost
// (health-collector sums it per-agent from token_ledger, see
// cost_test.go/main.go on the collector side), so the gradient makes the
// team's biggest spender visually pop instead of just a flat "no longer the
// aggregate" anchor. Both dim-halved like every other economics cell when
// the row itself is dimmed. Uses display.RGB/display.BOLD directly (not
// lipgloss.NewStyle) so the bold distinction survives outside a real TTY
// the same way every other color in this file does — lipgloss silently
// no-ops styling under go test (see TestFleetCostCellBoldForLeadOnly's
// comment for how tests verify this).
func fleetCostCell(cost, maxCost float64, isHeader bool, dim rowDim) string {
	rgb := metricRGB
	if !isHeader {
		rgb = fleetCostGradientRGB(cost, maxCost)
	}
	if dim == dimHalve {
		rgb = halveRGB(rgb)
	}
	text := padRightNoTrunc(fmtCost(cost), fleetCostW)
	prefix := display.RGB(rgb[0], rgb[1], rgb[2])
	if isHeader {
		prefix = display.BOLD + prefix
	}
	return prefix + text + display.RESET
}

// headerLine renders the dim bold column-label row. The two leading dot
// columns (activity state, health) get no label — just their reserved
// width — matching the operator's spec for this reorder.
func (v fleetView) headerLine(width int, cs fleetColSet, layout fleetLayout) string {
	line := fleetAssemble(
		" ",
		strings.Repeat(" ", fleetActivityDotW),
		padTrunc("MDL", fleetModelW),
		" ",
		padTrunc("AGENT", layout.agentW),
		padRight("AGE", fleetAgeW),
		padTrunc("ACTIVITY", layout.activityW),
		padRight("CTX", fleetCtxW),
		padTrunc("", fleetCtxBarCells),
		padRight("$", fleetCostW),
		padRight("IN", fleetTokW),
		padRight("OUT", fleetTokW),
		cs,
	)
	return lipgloss.NewStyle().Bold(true).Foreground(tui.ColorDim).Render(padTrunc(line, width))
}

// renderAgentRow renders one agent's line. fr carries the lineage
// decoration fleetTreeRows derived: a dim connector prefix (tree mode) or
// a dim ↳parent suffix (flat mode), both inside the AGENT cell so the
// child's own EntityColor-hashed name stays the loud part.
func (v fleetView) renderAgentRow(fr fleetRow, isCursor bool, cs fleetColSet, layout fleetLayout, width int) string {
	m := v.m
	r := fr.agent
	inactive := m.agents.isInactive(r)
	dim := rowDimLevel(r.Liveness, inactive)

	marker := " "
	if isCursor {
		marker = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorAccent).Render("▸")
	}

	nc := display.EntityColor(r.AgentName, "")
	name := fleetRowName(fr)
	if dim == dimHalve {
		nc = halveRGB(nc)
	}
	dimStyle := lipgloss.NewStyle().Foreground(tui.ColorDim)
	cell := display.RGB(nc[0], nc[1], nc[2]) + name + display.RESET
	if fr.prefix != "" {
		cell = dimStyle.Render(fr.prefix) + cell
	}
	if fr.suffix != "" {
		cell += dimStyle.Render(fr.suffix)
	}
	nameCell := padTrunc(cell, layout.agentW)

	fill := r.ContextFillPct
	var ctxCell string
	if fill > 1.0 {
		ctxCell = padRight(unreliableFill(), fleetCtxW)
	} else {
		clamped := fill
		if clamped < 0 {
			clamped = 0
		}
		ctxCell = renderDim(ctxGradientRGB(clamped), padRight(fmt.Sprintf("%.0f%%", clamped*100), fleetCtxW), dim)
	}

	now := time.Now()
	ageText := "—"
	var ageDur time.Duration
	if ts := m.agents.activityTsFor(r); !ts.IsZero() {
		ageDur = now.Sub(ts)
		ageText = relativeTime(ts)
	}
	ageCell := renderDim(fleetAgeColor(ageDur, m.agents.midTurnFor(r, now)), padRight(ageText, fleetAgeW), dim)

	activityDot, activityText := m.agents.activityGlyphAndTextFor(r)

	// A "no gauge data at all" agent (never collected — distinct from a
	// collected row that happens to have no active pressure alert, which
	// is fleetHealthDot's own "ok" default) gets the dim hollow dot instead
	// of a colored one; there is no pressure signal to color.
	healthDotCell := lipgloss.NewStyle().Foreground(tui.ColorDim).Render("○")
	if r.PressureLevel != "" || r.CollectorStatus != "" {
		healthDotCell = fleetHealthDot(r.PressureLevel)
	}

	// Model text through the same entity-color hasher as agent names, so
	// different models read as distinct colors at a glance (operator
	// request) — "opus-4-6" and "sonnet-5" never collide the way two
	// dim-grey model strings would.
	mc := display.EntityColor(r.Model, "")
	if dim == dimHalve {
		mc = halveRGB(mc)
	}
	modelCell := padTrunc(display.RGB(mc[0], mc[1], mc[2])+fleetModelAbbrev(r.Model)+display.RESET, fleetModelW)

	line := fleetAssemble(
		marker,
		activityDot,
		modelCell,
		healthDotCell,
		nameCell,
		ageCell,
		padTrunc(activityText, layout.activityW),
		ctxCell,
		fleetCtxBar(fill, dim),
		// Never bold on member rows — the lead included (operator decision
		// 2026-07-13): cost is per-agent since 039f718, and the bold weight
		// moved to the team header's authoritative total. Colored by
		// fr.teamMaxCost's gradient (operator request) so the group's
		// biggest spender pops.
		fleetCostCell(r.SessionCostUSD, fr.teamMaxCost, false, dim),
		renderDim(metricRGB, padRight(humanizeTokens(r.TokensInTotal), fleetTokW), dim),
		renderDim(metricRGB, padRight(humanizeTokens(r.TokensOutTotal), fleetTokW), dim),
		cs,
	)

	if dim == dimFlat {
		line = lipgloss.NewStyle().Foreground(tui.ColorDim).Render(display.StripANSI(line))
	}
	return display.TruncateLine(line, width)
}

// fleetEconTailWidth is the visible width of everything fleetAssemble
// renders AFTER the ACTIVITY cell — the "  "+CTX [+" "+bar] +"  "+$+" "+
// IN+" "+OUT block. The team header row is free-flow on its left side but
// must land its economics cells in exactly the columns agent rows use, so
// its label region is padded to width minus this. Kept in sync with
// fleetAssemble/fleetFixedWidth by construction (same constants, same
// separators); TestFleetTeamHeaderEconomicsAligned pins it.
func fleetEconTailWidth(cs fleetColSet) int {
	w := 2 + fleetCtxW + 2 + fleetCostW + 1 + fleetTokW + 1 + fleetTokW
	if cs.bar {
		w += 1 + fleetCtxBarCells
	}
	return w
}

// renderTeamHeaderRow renders one team's header line: the lead's runtime
// icon, the team label in its EntityColor hash, the lead's (user@host) and
// short session id (both dim), the objective in TagColor("GOAL") warm gold
// brackets, a member count when collapsed — then blank CTX/bar cells and the
// BOLD team cost total + token sums, aligned to the same economics columns
// every agent row uses (see fleetEconTailWidth). CTX/AGE are per-agent
// quantities and stay blank. No aggregate health dot — that's inferred from
// the lead's own dot on the row beneath.
func (v fleetView) renderTeamHeaderRow(fr fleetRow, isCursor bool, cs fleetColSet, width int) string {
	marker := " "
	if isCursor {
		marker = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorAccent).Render("▸")
	}

	colorSrc := strings.TrimPrefix(fr.label, "#")
	if fr.agent.TeamName == "" {
		colorSrc = fr.sessionID
	}
	lc := display.EntityColor(colorSrc, "")
	dimStyle := lipgloss.NewStyle().Foreground(tui.ColorDim)

	left := marker + runtimeDot(fr.agent.Runtime) + " " +
		lipgloss.NewStyle().Bold(true).Render(display.RGB(lc[0], lc[1], lc[2])+fr.label+display.RESET)

	// (user@host) — degrades gracefully when username isn't known (older or
	// remote sessions that never captured it): host alone, or omitted
	// entirely when neither is known.
	switch {
	case fr.agent.Username != "" && fr.agent.Host != "":
		left += " " + dimStyle.Render("("+fr.agent.Username+"@"+fr.agent.Host+")")
	case fr.agent.Host != "":
		left += " " + dimStyle.Render("("+fr.agent.Host+")")
	}
	left += " " + dimStyle.Render(sessionPrefix8(fr.sessionID))

	if fr.focus != "" {
		gc := display.TagColor("GOAL")
		left += " " + display.RGB(gc[0], gc[1], gc[2]) + "[" + fr.focus + "]" + display.RESET
	}
	if fr.collapsed {
		left += " " + dimStyle.Render("· "+agentCountLabel(len(fr.members)))
	}

	in, out := fleetTeamAggregates(fr.members)
	// Durable token_ledger session total (see fleetTeamAggregates' doc
	// comment) — fr.agent is the lead, the only row this field is
	// populated on.
	cost := fr.agent.SessionTotalCostUSD
	var tail strings.Builder
	tail.WriteString("  " + padRight("", fleetCtxW))
	if cs.bar {
		tail.WriteString(" " + padTrunc("", fleetCtxBarCells))
	}
	tail.WriteString("  " + fleetCostCell(cost, 0, true, dimNone) +
		" " + metricStyle.Render(padRight(humanizeTokens(in), fleetTokW)) +
		" " + metricStyle.Render(padRight(humanizeTokens(out), fleetTokW)))

	leftW := width - fleetEconTailWidth(cs)
	if leftW < 1 {
		leftW = 1
	}
	line := padTrunc(left, leftW) + tail.String()
	line = applyRowTint(line, fleetHeaderTintRGB(colorSrc), width)
	return display.TruncateLine(line, width)
}

// totalRow renders the bold aggregate footer: summed cost/in/out plus the
// agent count in the ACTIVITY slot.
func (v fleetView) totalRow(cs fleetColSet, layout fleetLayout, width int) string {
	rows := fleetVisibleRows(v.m.agents.rows)
	var cost float64
	var in, out int64
	for _, r := range rows {
		cost += r.SessionCostUSD
		in += r.TokensInTotal
		out += r.TokensOutTotal
	}
	line := fleetAssemble(
		" ",
		strings.Repeat(" ", fleetActivityDotW),
		padTrunc("", fleetModelW),
		" ",
		padTrunc("TOTAL", layout.agentW),
		padRight("", fleetAgeW),
		padTrunc(agentCountLabel(len(rows)), layout.activityW),
		padRight("", fleetCtxW),
		padTrunc("", fleetCtxBarCells),
		metricStyle.Render(padRightNoTrunc(fmtCost(cost), fleetCostW)),
		metricStyle.Render(padRight(humanizeTokens(in), fleetTokW)),
		metricStyle.Render(padRight(humanizeTokens(out), fleetTokW)),
		cs,
	)
	return lipgloss.NewStyle().Bold(true).Render(display.TruncateLine(line, width))
}

// fleetGridMaxFrac caps the grid's content-driven height at this fraction of
// the view's total height, so the activity log (operator request
// 2026-07-13, mirrors the health view's own Activity panel) always gets a
// meaningful share of the screen even when the grid has few rows to show.
// fleetLogMinH is the log's own floor — a title band plus at least two
// content lines — protected even when the grid's natural content would
// otherwise want the whole view (many teams on a short terminal).
const (
	fleetGridMaxFrac = 0.75
	fleetLogMinH     = 3
)

// fleetBodyHeights splits the view's total height between the grid (top)
// and the activity log (bottom). The grid takes exactly what its content
// needs — band + column header + one line per row + TOTAL — capped at
// fleetGridMaxFrac of height, then capped further so the log never drops
// below fleetLogMinH while there's room to spare.
func fleetBodyHeights(height, rowCount int) (gridH, logH int) {
	natural := rowCount + 3 // band + column header + TOTAL
	if max := int(float64(height)*fleetGridMaxFrac + 0.5); natural > max {
		natural = max
	}
	gridH = natural
	if gridH > height-fleetLogMinH {
		gridH = height - fleetLogMinH
	}
	if gridH < 1 {
		gridH = 1
	}
	logH = height - gridH
	if logH < 0 {
		logH = 0
	}
	return gridH, logH
}

// View renders the Fleet view body: header band, column labels, one row per
// agent (scrolled to keep the cursor visible), the TOTAL row, then a
// flowing activity log below (operator request 2026-07-13) — the same
// SSE-fed m.activity buffer and render.FormatLine-based rendering the
// health view's Activity panel uses (see health_view.go), so both views
// show identical event formatting. Newest events land at the bottom,
// always auto-following: the fleet view has no tab/focus concept of its own
// to scroll or pause the log the way the health view's Activity panel can,
// so it's rendered unfocused (dimmer title band) and always caught up.
func (v fleetView) View(width, height int) string {
	m := v.m
	now := time.Now()
	rows := v.fleetRowsFor()
	cs := fleetColumnsForWidth(width)
	agentW := fleetAgentColWidth(rows)
	cs, layout := fleetLayoutFor(width, agentW, cs)
	cursor := m.fleet.reconcile(rows)

	gridH, logH := fleetBodyHeights(height, len(rows))

	out := make([]string, 0, gridH)
	out = append(out, v.headerBand(width, now))
	out = append(out, v.headerLine(width, cs, layout))

	rowsAvail := gridH - 3 // band + column header + TOTAL
	if rowsAvail < 1 {
		rowsAvail = 1
	}

	if len(rows) == 0 {
		// rowsAvail+1 — the TOTAL row's slot too; nothing to sum, so the
		// placeholder takes the whole remaining grid budget.
		msg := lipgloss.NewStyle().Foreground(tui.ColorDim).Render("no agents in scope")
		out = append(out, lipgloss.Place(width, rowsAvail+1, lipgloss.Center, lipgloss.Center, msg))
	} else {
		start := 0
		if cursor >= rowsAvail {
			start = cursor - rowsAvail + 1
		}
		end := start + rowsAvail
		if end > len(rows) {
			end = len(rows)
		}
		for i := start; i < end; i++ {
			if rows[i].isHeader {
				out = append(out, v.renderTeamHeaderRow(rows[i], i == cursor, cs, width))
			} else {
				out = append(out, v.renderAgentRow(rows[i], i == cursor, cs, layout, width))
			}
		}
		for len(out) < gridH-1 {
			out = append(out, "")
		}
		out = append(out, v.totalRow(cs, layout, width))
	}

	// Same exact-width padding rule as the agents grid (see agents.View):
	// with no bordered box, nothing else pads short lines for us.
	for i := range out {
		if w := lipgloss.Width(out[i]); w < width {
			out[i] += strings.Repeat(" ", width-w)
		}
	}
	grid := strings.Join(out, "\n")
	log := m.activity.View(width, logH, false, m.colorize)
	// JoinVertical (same technique health_view.go's multi-panel join
	// relies on) pads the log's own blank filler lines (activityModel.View
	// leaves them "", relying on its caller for exact-width) out to match
	// the grid's already-full-width lines, rather than leaving them short.
	return lipgloss.JoinVertical(lipgloss.Left, grid, log)
}
