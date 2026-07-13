package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/render"
	"github.com/bmjdotnet/teamster/internal/tui"
)

type sortMode int

const (
	sortPressure sortMode = iota
	sortFill
	sortName
	sortLast
)

func (s sortMode) label() string {
	switch s {
	case sortFill:
		return "fill"
	case sortName:
		return "name"
	case sortLast:
		return "last"
	default:
		return "pressure"
	}
}

// defaultAgePresets is the built-in cycle the "r" key steps through when
// neither --age-presets nor TEAMSTER_CTOP_AGE_PRESETS override it: 1h → 6h →
// all (unfiltered).
var defaultAgePresets = []time.Duration{time.Hour, 6 * time.Hour, 0}

// parseAgePresets parses a comma-separated --age-presets/TEAMSTER_CTOP_AGE_PRESETS
// value (e.g. "30m,2h,12h,0") into the cycle cycleAgePreset steps through.
// "0" means "all" (no filter), matching --max-age's own convention.
func parseAgePresets(s string) ([]time.Duration, error) {
	fields := strings.Split(s, ",")
	out := make([]time.Duration, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if f == "0" {
			out = append(out, 0)
			continue
		}
		d, err := time.ParseDuration(f)
		if err != nil {
			return nil, fmt.Errorf("invalid age preset %q: %w", f, err)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no age presets found in %q", s)
	}
	return out, nil
}

// resolveAgePresets returns the "r"-key cycle plus the index within it that
// maxAge (the --max-age starting value) corresponds to. If maxAge exactly
// matches one of presets, that's the starting index. Otherwise maxAge is
// prepended — the cycle starts on the operator's actual starting value, and
// the first "r" press moves on to the configured presets, same as today's
// "first press always lands on presets[1]" behavior when --max-age doesn't
// match the hardcoded 1h.
func resolveAgePresets(presets []time.Duration, maxAge time.Duration) ([]time.Duration, int) {
	for i, p := range presets {
		if p == maxAge {
			return presets, i
		}
	}
	out := make([]time.Duration, 0, len(presets)+1)
	out = append(out, maxAge)
	out = append(out, presets...)
	return out, 0
}

// agentGroup is one Teamster session's rows: the lead (if present, always
// rows[0] after sortGroupMembers) followed by teammates in m.sort order.
type agentGroup struct {
	sessionID string
	rows      []Agent
}

// tokensInTotal/tokensOutTotal/toolCallsTotal sum a per-agent metric across
// every row in the group — unlike SessionCostUSD (see visRow's cost field),
// these ARE genuinely per-agent contributions (each teammate's own token
// usage), so summing them for the collapsed row's header display is correct.
func (g agentGroup) tokensInTotal() int64 {
	var n int64
	for _, r := range g.rows {
		n += r.TokensInTotal
	}
	return n
}

func (g agentGroup) tokensOutTotal() int64 {
	var n int64
	for _, r := range g.rows {
		n += r.TokensOutTotal
	}
	return n
}

func (g agentGroup) toolCallsTotal() int64 {
	var n int64
	for _, r := range g.rows {
		n += r.ToolCallsTotal
	}
	return n
}

// visRow is one rendered/navigable line in the agents grid: either a
// session's header row (the collapsed summary, or the anchor line of an
// expanded session) or one member row inside an expanded session.
// recomputeRows rebuilds this list from m.groups + m.expanded on every poll,
// sort/age-filter change, and expand/collapse toggle. Cursor movement and
// View() both walk this list instead of m.rows, so an expanded session's
// children become their own navigable rows and a collapsed session is
// exactly one.
type visRow struct {
	sessionID   string
	agent       Agent // header row: the group's representative (rows[0] — the lead if one exists); member row: itself
	isHeader    bool
	hasChildren bool // only meaningful when isHeader: group has >1 row
	expanded    bool // only meaningful when isHeader && hasChildren
	tokensIn    int64
	tokensOut   int64
	toolCalls   int64
}

// selectionKey is the visRow analog of Agent.SelectionKey: a header row is
// keyed by its session ID (stable across polls regardless of which agent
// ends up as rows[0]), a member row by its own agent identity.
func (vr visRow) selectionKey() string {
	if vr.isHeader {
		return "session:" + vr.sessionID
	}
	return vr.agent.SelectionKey()
}

// agentsModel is the Agents grid sub-model: rows, sort, selection, age filter.
type agentsModel struct {
	// allRows is the last full fetch (post --team filter, pre age filter) —
	// kept around so cycleAgePreset/cycleSort can re-derive the display
	// instantly without waiting for the next poll.
	allRows []Agent
	groups  []agentGroup // grouped by session, filtered, sorted
	rows    []Agent      // flattened groups — every agent, header or member; used for grid-wide aggregates (agentColWidth, the TOTAL row, status-bar counts), not for cursor navigation

	// expanded tracks which sessions the operator has opened, keyed by
	// sessionID. Absent (including nil map) means collapsed — the default.
	expanded map[string]bool

	// visRows is the flattened, expand-aware row list View() renders and
	// cursor/select_/current navigate — one entry per session header plus
	// one per member row of every currently-expanded session.
	visRows []visRow

	sort   sortMode
	maxAge time.Duration // active age filter; <=0 means unfiltered
	// agePresets is the "r"-key cycle; nil falls back to defaultAgePresets
	// (cycleAgePreset applies the fallback, so a zero-value agentsModel — as
	// several tests construct directly — still cycles 1h → 6h → all).
	agePresets []time.Duration
	ageIdx     int    // position in agePresets, advanced by cycleAgePreset
	selected   string // SelectionKey (of an Agent) committed via "l" — binds the Detail panel, "" = Alerts view
	cursorKey  string // visRow.selectionKey() of whatever row the cursor highlights, independent of "selected"
	cursor     int

	// lastActions is the SSE-derived per-agent last-tagged-action tracker
	// (ACTIVITY/LAST columns, and group recency) — keyed by
	// activityKey(sessionID, agentName), so it updates in real time as
	// activity events arrive instead of waiting for the next 5s health
	// poll. Agent name alone isn't a unique key: every session's lead has
	// AgentName == "", so a multi-session dashboard needs the session ID
	// too or all leads collapse onto one shared entry.
	lastActions map[string]lastAction

	// inactiveAfter is the --inactive-after threshold: an alive agent with
	// no known activity for longer than this reads as "inactive" in the
	// STATUS/ACTIVITY dots and the row's own dimming (see isInactive,
	// rowDimLevel). <=0 disables the check entirely (matches maxAge's own
	// <=0-means-unfiltered convention), so a zero-value agentsModel — as
	// most tests construct directly — never reads a row as inactive.
	inactiveAfter time.Duration
}

// lastAction is one agent's most recent tagged activity-feed event — the
// same displayable events (ACT/READ/DONE/THNK/EDIT/TEAM/COMM/...) the
// Activity panel renders.
type lastAction struct {
	tag     string
	display string
	ts      time.Time
}

// activityKey builds the lastActions map key from a session ID and agent
// name. sessionID is truncated to its first 12 characters to match the
// truncation internal/server/server.go already applies before it ever
// reaches the SSE stream (the "session" field on render.Record is always
// <=12 chars) — the health API's Agent.SessionID is the full UUID, so
// without truncating both sides consistently here, the SSE-derived tracker
// and the grid's lookup would never land in the same bucket.
func activityKey(sessionID, agentName string) string {
	if len(sessionID) > 12 {
		sessionID = sessionID[:12]
	}
	return sessionID + "|" + agentName
}

// sessionPrefix8 returns the first 8 characters of a session ID (or the
// whole thing if shorter) — the short visual identifier used to
// disambiguate rows that share an empty agent_name or team_name (every
// session's lead, or any teammate whose team_name hasn't been set yet).
func sessionPrefix8(sessionID string) string {
	if len(sessionID) > 8 {
		return sessionID[:8]
	}
	return sessionID
}

// sanitizeActivity strips characters that would break a single-line grid
// row: completeActivity messages are free text and may contain newlines or
// markdown emphasis (`**bold**`), neither of which render sanely inside a
// fixed-height table cell.
func sanitizeActivity(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "**", "")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// recordActivity updates the tracker from an incoming SSE record. Records
// with no tag or no display text (bare PostToolUse acks, etc.) carry
// nothing worth showing in the grid and are skipped.
func (m *agentsModel) recordActivity(r render.Record) {
	if r.Tag == "" || r.Display == "" {
		return
	}
	if m.lastActions == nil {
		m.lastActions = make(map[string]lastAction)
	}
	ts := parseRFC3339(r.TS)
	if ts.IsZero() {
		ts = time.Now()
	}
	m.lastActions[activityKey(r.Session, r.AgentName)] = lastAction{tag: r.Tag, display: sanitizeActivity(r.Display), ts: ts}
}

// resolvedActivity merges r's two activity sources into one {tag, display,
// ts} — the single point every recency- and rendering-related computation
// (activityTsFor, isInactive, activityGlyphAndTextFor) reads through, so
// none of them can ever disagree about which source is current. The health
// API poll (r.LastActivityTag/LastActivityDisplay/LastActivityTs) is
// authoritative; the SSE tracker (m.lastActions) only overlays it when its
// own timestamp is strictly newer, giving snappier between-poll updates
// without ever diverging in field coverage — both sources always carry the
// same tag+display shape, so there is no "SSE has color, API doesn't" split.
func (m *agentsModel) resolvedActivity(r Agent) (tag, text string, ts time.Time) {
	tag, text, ts = r.LastActivityTag, r.LastActivityDisplay, parseRFC3339(strOrEmpty(r.LastActivityTs))
	if la, ok := m.lastActions[activityKey(r.SessionID, r.AgentName)]; ok && la.ts.After(ts) {
		tag, text, ts = la.tag, la.display, la.ts
	}
	return tag, text, ts
}

// activityTsFor returns the best-known last-activity timestamp for a row —
// see resolvedActivity. Used by the LAST column, the age filter, member sort
// (sortLast), and group-recency ordering.
func (m *agentsModel) activityTsFor(r Agent) time.Time {
	_, _, ts := m.resolvedActivity(r)
	return ts
}

// isInactive reports whether r has had no known activity for longer than
// m.inactiveAfter — the "alive but stale-activity" tier between healthy
// (processing/idle) and closed (see rowDimLevel, statusDots,
// activityCellFor). inactiveAfter<=0 disables the check entirely.
func (m *agentsModel) isInactive(r Agent) bool {
	if m.inactiveAfter <= 0 {
		return false
	}
	return time.Since(m.activityTsFor(r)) > m.inactiveAfter
}

// midTurnFor reports whether r is actively mid-turn right now, per the SSE
// tracker's last known tagged event for it — false when no such event is
// known yet this session (a health-API-only row can't distinguish
// processing from idle; see activityCellFor's own fallback for the
// ACTIVITY column's equivalent rule).
func (m *agentsModel) midTurnFor(r Agent, now time.Time) bool {
	la, ok := m.lastActions[activityKey(r.SessionID, r.AgentName)]
	return ok && isMidTurn(la, now)
}

// dimGreyRGB approximates tui.ColorDim (#484f58) as a plain RGB triple, for
// contexts with no tag color to key off — the ACTIVITY glyph's fallback
// when no SSE-tagged event is known yet for a row.
var dimGreyRGB = [3]int{72, 79, 88}

// midTurnWindow is how recent a tagged SSE event must be to read the
// activity glyph as "mid-turn" (hollow) rather than "idle" (filled).
const midTurnWindow = 10 * time.Second

// isMidTurn reports whether la looks like the agent is actively mid-turn
// right now: a non-terminal tag (DONE/COMP close a turn) seen within
// midTurnWindow of now. Once that event ages past the window — or the last
// tag was a close — the glyph reads as idle.
func isMidTurn(la lastAction, now time.Time) bool {
	if la.tag == "DONE" || la.tag == "COMP" {
		return false
	}
	return now.Sub(la.ts) < midTurnWindow
}

// activityState is the ACTIVITY column's 4-state work indicator (§ctop
// AgentAF redesign item 2) — a work-state signal, deliberately independent
// of the STATUS column's health signal (see healthDot in format.go).
type activityState int

const (
	activityProcessing activityState = iota // mid-turn: actively consuming context right now
	activityIdle                            // finished its last turn, awaiting the next dispatch — healthy, not degraded
	activityInactive                        // idle too long: no activity for --inactive-after
	activityClosed                          // session gone
)

// activityStateFor picks the ACTIVITY column's state: closed wins over
// everything (a dead session's row never reads as "processing" no matter
// what its last tagged event was), then midTurn (an actively-consuming-
// context turn) reads as processing, then inactive (no activity for
// --inactive-after), else idle.
func activityStateFor(midTurn, closed, inactive bool) activityState {
	switch {
	case closed:
		return activityClosed
	case midTurn:
		return activityProcessing
	case inactive:
		return activityInactive
	default:
		return activityIdle
	}
}

// activityStateColor picks the glyph color for state: the tag's own color
// only for processing (mid-turn work is a "what is it doing" signal, so it
// keeps the tag's identity); every other state uses a fixed color
// independent of the tag — green for idle (deliberately matches the feed's
// [DONE] green, so the two visually agree), dim grey for inactive/closed.
func activityStateColor(state activityState, tagColor [3]int) [3]int {
	switch state {
	case activityProcessing:
		return tagColor
	case activityIdle:
		return successRGB
	default: // activityInactive, activityClosed
		return dimGreyRGB
	}
}

// activityGlyph renders the ACTIVITY column's leading 1-char work-state
// glyph in rgb (see activityStateColor for how callers pick rgb), using the
// same "◉"/"○" (FISHEYE/WHITE CIRCLE) filled/hollow pair as the STATUS
// column's healthDot/runtimeDot, per an explicit operator request to make
// the two columns visually consistent — this supersedes an earlier
// decision to use "⬤"/"◯" (BLACK LARGE CIRCLE/LARGE CIRCLE) here
// specifically because they render at a closer visual weight than ◉/○ in
// common monospace fonts; the operator preferred the shared pair anyway.
//   - processing: flashes every second between "◉" and "○" — a blink/pulse
//     effect, not a brightness change. The 1s displayTickMsg already forces
//     a re-render every second, so this alone makes it animate.
//   - idle/inactive: a steady filled "◉" — no animation at all.
//   - closed: a steady hollow "○" — no animation.
//
// now is threaded through for deterministic testing.
func activityGlyph(state activityState, rgb [3]int, now time.Time) string {
	glyph := "◉"
	switch state {
	case activityProcessing:
		glyph = "◉"
		if now.Second()%2 == 1 {
			glyph = "○"
		}
	case activityClosed:
		glyph = "○"
	}
	return display.RGB(rgb[0], rgb[1], rgb[2]) + glyph + " " + display.RESET
}

// activityCellFor renders the ACTIVITY column's content: the state glyph
// (see activityGlyph/activityStateFor) followed by
// display.RenderDisplay-processed text (the same tag-color + __param__ +
// @agent/#team highlighting the Activity panel already uses for these
// events, via internal/render.FormatLine).
func (m *agentsModel) activityCellFor(r Agent) string {
	glyph, text := m.activityGlyphAndTextFor(r)
	return glyph + text
}

// activityGlyphAndTextFor computes the ACTIVITY column's state glyph and
// display text as two separate pieces — activityCellFor concatenates them
// into the AGENT view's single combined cell; the Fleet view (fleet_view.go)
// instead renders the glyph as its own leading column and the text as a
// separate one, so it calls this directly. Tag+display text always come
// from resolvedActivity (health API poll, overlaid by a newer SSE event) —
// one rendering path regardless of source, so it's never possible for one
// source to render tag-colored and the other dim/default. "Processing"
// detection is intentionally still SSE-only (ok && isMidTurn(la, now)): a
// health-API-only row (no live SSE event seen yet this ctop session) can't
// distinguish actively-processing from idle, so it reads as idle/inactive/
// closed at worst, never a false mid-turn flash.
func (m *agentsModel) activityGlyphAndTextFor(r Agent) (glyph, text string) {
	now := time.Now()
	closed := r.Liveness == "closed"
	inactive := m.isInactive(r)

	la, ok := m.lastActions[activityKey(r.SessionID, r.AgentName)]
	state := activityStateFor(ok && isMidTurn(la, now), closed, inactive)

	tag, txt, _ := m.resolvedActivity(r)
	if tag == "" {
		glyph = activityGlyph(state, activityStateColor(state, dimGreyRGB), now)
		return glyph, sanitizeActivity(txt)
	}
	tc := display.TagColor(tag)
	glyph = activityGlyph(state, activityStateColor(state, tc), now)
	c := display.RGB(tc[0], tc[1], tc[2])
	return glyph, c + display.RenderDisplay(sanitizeActivity(txt), tc, r.SessionID) + display.RESET
}

func pressureRank(level string) int {
	switch level {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

// memberLess returns the comparator used to order one group's non-lead
// members (m.sort selects which). Every branch falls back to agent name as
// its final tie-break: sort.SliceStable alone only guarantees stability
// relative to *this* input order, and the input order (the health API's own
// `ORDER BY updated_at DESC`) shifts a little on every poll, so an
// underspecified comparator makes tied rows visibly jump between refreshes
// even though nothing about them changed.
func (m *agentsModel) memberLess(rows []Agent) func(i, j int) bool {
	switch m.sort {
	case sortFill:
		return func(i, j int) bool {
			if rows[i].ContextFillPct != rows[j].ContextFillPct {
				return rows[i].ContextFillPct > rows[j].ContextFillPct
			}
			return rows[i].AgentName < rows[j].AgentName
		}
	case sortName:
		return func(i, j int) bool { return rows[i].AgentName < rows[j].AgentName }
	case sortLast:
		return func(i, j int) bool {
			ti, tj := m.activityTsFor(rows[i]), m.activityTsFor(rows[j])
			if !ti.Equal(tj) {
				return ti.After(tj)
			}
			return rows[i].AgentName < rows[j].AgentName
		}
	default: // sortPressure: critical>warning>ok, then fill desc, then name
		return func(i, j int) bool {
			pi, pj := pressureRank(rows[i].PressureLevel), pressureRank(rows[j].PressureLevel)
			if pi != pj {
				return pi < pj
			}
			if rows[i].ContextFillPct != rows[j].ContextFillPct {
				return rows[i].ContextFillPct > rows[j].ContextFillPct
			}
			return rows[i].AgentName < rows[j].AgentName
		}
	}
}

// pinLeadsFirst moves every lead row (AgentName == "") in rows to the
// front, in place, sorted among themselves by SessionID for determinism.
// Used both per-group (where at most one lead can ever appear, making the
// inner sort a no-op) and would handle a flat multi-session list the same
// way if ever needed — the operator's own session should never sort into
// the pack.
func pinLeadsFirst(rows []Agent) {
	leads := make([]Agent, 0, 1)
	rest := make([]Agent, 0, len(rows))
	for _, r := range rows {
		if r.AgentName == "" {
			leads = append(leads, r)
		} else {
			rest = append(rest, r)
		}
	}
	sort.SliceStable(leads, func(i, j int) bool {
		return leads[i].SessionID < leads[j].SessionID
	})
	copy(rows, leads)
	copy(rows[len(leads):], rest)
}

// groupBySession partitions rows into per-session groups, preserving the
// first-seen order of session IDs — arbitrary, since sortGroupsByRecency
// reorders groups immediately after.
func groupBySession(rows []Agent) []agentGroup {
	order := make([]string, 0, len(rows))
	byID := make(map[string][]Agent, len(rows))
	for _, r := range rows {
		if _, ok := byID[r.SessionID]; !ok {
			order = append(order, r.SessionID)
		}
		byID[r.SessionID] = append(byID[r.SessionID], r)
	}
	groups := make([]agentGroup, 0, len(order))
	for _, id := range order {
		groups = append(groups, agentGroup{sessionID: id, rows: byID[id]})
	}
	return groups
}

// groupRecency is the most recent known activity across every row in the
// group (lead included) — the sort key for ordering groups, and (via
// filterGroupsByMaxAge) the basis for dropping an entire session at once.
func (m *agentsModel) groupRecency(g agentGroup) time.Time {
	var latest time.Time
	for _, r := range g.rows {
		if ts := m.activityTsFor(r); ts.After(latest) {
			latest = ts
		}
	}
	return latest
}

// hasLiveMembers reports whether any row in the group is currently live or
// idle — i.e. actually present right now, as opposed to stale/closed/unbound
// or a liveness the server hasn't told us yet.
func hasLiveMembers(g agentGroup) bool {
	for _, r := range g.rows {
		if r.Liveness == "live" || r.Liveness == "idle" {
			return true
		}
	}
	return false
}

// filterGroupsByMaxAge drops whole groups where no member (lead or
// teammate) has activity within maxAge — a session with a stale lead and
// stale teammates disappears as one unit, rather than teammates flickering
// in and out of their lead's group independently. A group with zero
// (unknown) recency is kept ONLY if it has a live/idle member — that's a
// brand-new agent that hasn't posted its first activity timestamp yet, not
// evidence of staleness. A zero-recency group with no live members is a dead
// session (closed/stale/unbound) that simply never got a last_activity_ts,
// and gets filtered out same as any other stale group — keeping it
// unconditionally let sessions with equal (zero) recency pile up in the
// default 1h view and made the grid oscillate between polls, since their
// relative order was never actually meaningful. maxAge<=0 disables the
// filter entirely.
func (m *agentsModel) filterGroupsByMaxAge(groups []agentGroup) []agentGroup {
	if m.maxAge <= 0 {
		return groups
	}
	cutoff := time.Now().Add(-m.maxAge)
	out := make([]agentGroup, 0, len(groups))
	for _, g := range groups {
		recency := m.groupRecency(g)
		if recency.After(cutoff) {
			out = append(out, g)
		} else if recency.IsZero() && hasLiveMembers(g) {
			out = append(out, g)
		}
	}
	return out
}

// filterStaleClosedMembers drops individual closed/stale agents within a
// group that filterGroupsByMaxAge already kept as a whole (the session
// overall is still within maxAge) — an active session can still be dragging
// along long-closed teammates (e.g. an @Explore subagent from 2h ago) that
// have nothing left to show. Only a CLOSED or STALE agent whose own last
// activity is older than maxAge (or unknown) is dropped; the lead
// (AgentName == "") and any live/idle member are never filtered, and a
// teammate that closed out recently (within the age window) still shows —
// "just wrapped up" work stays visible. maxAge<=0 disables the filter
// entirely, matching filterGroupsByMaxAge's own convention.
func (m *agentsModel) filterStaleClosedMembers(groups []agentGroup) []agentGroup {
	if m.maxAge <= 0 {
		return groups
	}
	cutoff := time.Now().Add(-m.maxAge)
	out := make([]agentGroup, 0, len(groups))
	for _, g := range groups {
		rows := make([]Agent, 0, len(g.rows))
		for _, r := range g.rows {
			stale := r.Liveness == "closed" || r.Liveness == "stale"
			if r.AgentName == "" || !stale || m.activityTsFor(r).After(cutoff) {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		g.rows = rows
		out = append(out, g)
	}
	return out
}

// sortGroupMembers orders one group's rows: the lead (if present) first,
// then teammates per m.sort.
func (m *agentsModel) sortGroupMembers(g *agentGroup) {
	sort.SliceStable(g.rows, m.memberLess(g.rows))
	pinLeadsFirst(g.rows)
}

// sortGroupsByRecency orders groups by groupRecency, most recent first, so
// the currently-active session is always at the top. Ties fall back to
// sessionID — without this, two sessions with equal recency (common when the
// health-collector and a StatusLine POST land in the same second) reorder
// between polls purely because the API's own response order isn't stable,
// making rows visibly oscillate even though nothing about them changed.
func (m *agentsModel) sortGroupsByRecency(groups []agentGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		ti, tj := m.groupRecency(groups[i]), m.groupRecency(groups[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return groups[i].sessionID < groups[j].sessionID
	})
}

func flattenGroups(groups []agentGroup) []Agent {
	var out []Agent
	for _, g := range groups {
		out = append(out, g.rows...)
	}
	return out
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// setRows installs a freshly-fetched row set (post --team filter) as the new
// baseline and re-derives the displayed groups/rows from it. Called on
// every poll result.
func (m *agentsModel) setRows(rows []Agent) {
	m.allRows = rows
	m.recomputeRows()
}

// buildVisRows flattens groups into the rendered/navigable row list: one
// header row per session (collapsed by default — see m.expanded), followed
// by its members when expanded. rows[0] within a group is always the lead
// if one exists (sortGroupMembers/pinLeadsFirst guarantee this), so it
// doubles as both the header's representative agent and the row member
// lines (rows[1:]) exclude, to avoid showing the lead twice.
func (m *agentsModel) buildVisRows(groups []agentGroup) []visRow {
	var out []visRow
	for _, g := range groups {
		if len(g.rows) == 0 {
			continue
		}
		lead := g.rows[0]
		hasChildren := len(g.rows) > 1
		out = append(out, visRow{
			sessionID:   g.sessionID,
			agent:       lead,
			isHeader:    true,
			hasChildren: hasChildren,
			expanded:    hasChildren && m.expanded[g.sessionID],
			tokensIn:    g.tokensInTotal(),
			tokensOut:   g.tokensOutTotal(),
			toolCalls:   g.toolCallsTotal(),
		})
		if hasChildren && m.expanded[g.sessionID] {
			for _, member := range g.rows[1:] {
				out = append(out, visRow{sessionID: g.sessionID, agent: member})
			}
		}
	}
	return out
}

// recomputeRows re-derives m.groups/m.rows/m.visRows from m.allRows: group by
// session, drop whole groups the age filter excludes, drop individual
// long-closed teammates within the surviving groups (filterStaleClosedMembers),
// order each group's members, order the groups themselves by recency, rebuild
// the expand-aware visRows list, then reconcile the cursor against the new
// ordering by
// cursorKey — so a refresh never silently snaps the highlighted row back to
// the top, and collapsing a session leaves the cursor sitting on its (now
// single) header row. Called on fresh poll data (setRows), the operator
// changing the age filter (cycleAgePreset), sort mode (cycleSort), or
// expand state (toggleExpand), or the 1s display tick (so a row/group aging
// out of the filter disappears promptly rather than only at the next 5s
// poll).
func (m *agentsModel) recomputeRows() {
	groups := groupBySession(m.allRows)
	groups = m.filterGroupsByMaxAge(groups)
	groups = m.filterStaleClosedMembers(groups)
	for i := range groups {
		m.sortGroupMembers(&groups[i])
	}
	m.sortGroupsByRecency(groups)
	m.groups = groups
	m.rows = flattenGroups(groups)
	m.visRows = m.buildVisRows(groups)

	if m.cursorKey != "" {
		for i, vr := range m.visRows {
			if vr.selectionKey() == m.cursorKey {
				m.cursor = i
				return
			}
		}
	}
	// cursorKey unset (first load) or its row fell out of scope: fall back
	// to the current cursor index, clamped, and re-derive cursorKey from it.
	if m.cursor >= len(m.visRows) {
		m.cursor = 0
	}
	if len(m.visRows) > 0 {
		m.cursorKey = m.visRows[m.cursor].selectionKey()
	} else {
		m.cursorKey = ""
	}
}

// cycleAgePreset advances the "r" key's age-filter cycle (m.agePresets, or
// defaultAgePresets — 1h → 6h → all — when unset) and immediately re-derives
// the display from the last fetch — no server round-trip needed, since the
// age filter is purely client-side over data already in hand.
func (m *agentsModel) cycleAgePreset() {
	presets := m.agePresets
	if len(presets) == 0 {
		presets = defaultAgePresets
	}
	m.ageIdx = (m.ageIdx + 1) % len(presets)
	m.maxAge = presets[m.ageIdx]
	m.recomputeRows()
}

func (m *agentsModel) syncCursorKey() {
	if len(m.visRows) > 0 {
		m.cursorKey = m.visRows[m.cursor].selectionKey()
	}
}

func (m *agentsModel) moveDown() {
	if len(m.visRows) == 0 {
		return
	}
	if m.cursor < len(m.visRows)-1 {
		m.cursor++
	}
	m.syncCursorKey()
}

func (m *agentsModel) moveUp() {
	if m.cursor > 0 {
		m.cursor--
	}
	m.syncCursorKey()
}

func (m *agentsModel) first() {
	m.cursor = 0
	m.syncCursorKey()
}

func (m *agentsModel) last() {
	if len(m.visRows) > 0 {
		m.cursor = len(m.visRows) - 1
	}
	m.syncCursorKey()
}

func (m *agentsModel) cycleSort() {
	m.sort = (m.sort + 1) % 4
	m.recomputeRows()
}

// toggleExpand flips the expand state of the session header row under the
// cursor. No-op on a member row or on a header with no children (a solo
// session has nothing to expand). cursorKey is set to the header's own key
// before recomputing, so collapsing never moves the cursor off the row the
// operator just acted on — it lands back on the same (now single) row.
func (m *agentsModel) toggleExpand() {
	if m.cursor < 0 || m.cursor >= len(m.visRows) {
		return
	}
	vr := m.visRows[m.cursor]
	if !vr.isHeader || !vr.hasChildren {
		return
	}
	if m.expanded == nil {
		m.expanded = make(map[string]bool)
	}
	m.expanded[vr.sessionID] = !m.expanded[vr.sessionID]
	m.cursorKey = vr.selectionKey()
	m.recomputeRows()
}

// current returns the agent represented by the row under the cursor — the
// group's representative for a session header row, or the specific member
// agent for a member row — or (Agent{}, false) if empty.
func (m *agentsModel) current() (Agent, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visRows) {
		return Agent{}, false
	}
	return m.visRows[m.cursor].agent, true
}

// select_ selects the row under the cursor and returns its SelectionKey (""
// if there are no rows) — this binds the Detail panel via the underlying
// Agent's own identity, independent of the visRow tree-navigation key.
// syncCursorKey is called defensively in case the caller moved m.cursor
// directly rather than via moveUp/moveDown/first/last.
func (m *agentsModel) select_() string {
	row, ok := m.current()
	if !ok {
		return ""
	}
	m.selected = row.SelectionKey()
	m.syncCursorKey()
	return m.selected
}

func (m *agentsModel) clearSelection() {
	m.selected = ""
}

// cellGroup accumulates rendered cell strings into the visually distinct
// column groups the style guide calls for: 1 space between cells within a
// group, 2 spaces between groups (AGENT[+HOST+MODEL] / ST+CTX /
// IN+OUT+COST+T / LAST+ACTIVITY). next() starts a new group; render() joins
// non-empty groups with "  ", silently skipping any group left entirely
// empty (e.g. the tokens/cost/toolcalls group at a width too narrow for any
// of them).
type cellGroup struct {
	groups [][]string
}

func newCellGroup() *cellGroup {
	return &cellGroup{groups: [][]string{{}}}
}

func (g *cellGroup) add(cell string) {
	last := len(g.groups) - 1
	g.groups[last] = append(g.groups[last], cell)
}

func (g *cellGroup) next() {
	g.groups = append(g.groups, nil)
}

func (g *cellGroup) render() string {
	parts := make([]string, 0, len(g.groups))
	for _, grp := range g.groups {
		if len(grp) == 0 {
			continue
		}
		parts = append(parts, strings.Join(grp, " "))
	}
	return strings.Join(parts, "  ")
}

// column set thresholds (§2.3 narrow column sets, tightened by the column
// diet — see columnsForWidth). ST (status dots) and CTX have no gate here —
// always shown, the last things to drop. TEAM is gone entirely: the
// background tint (teamTintRGB/blendBG) plus sessionAlias in the AGENT cell
// now carry that identity.
type colSet struct {
	host, model, tokens, cost, toolCalls, last, activity bool
}

// multipleHostsPresent reports whether rows spans more than one distinct
// Host value — the HOST column only earns its space when it's actually
// disambiguating something; a single-host fleet renders it as dead weight.
func multipleHostsPresent(rows []Agent) bool {
	seen := ""
	for _, r := range rows {
		if r.Host == "" {
			continue
		}
		if seen == "" {
			seen = r.Host
			continue
		}
		if r.Host != seen {
			return true
		}
	}
	return false
}

// columnsForWidth returns which columns fit at width w. multiHost gates HOST
// independently of width — it's only ever shown when the visible agent set
// actually spans more than one host (see multipleHostsPresent).
func columnsForWidth(w int, multiHost bool) colSet {
	cs := colSet{host: multiHost, model: true, tokens: true, cost: true, toolCalls: true, last: true, activity: true}
	if w < 90 {
		cs.tokens = false
		cs.cost = false
		cs.toolCalls = false
	}
	if w < 80 {
		cs.model = false
	}
	if w < 70 {
		cs.host = false
	}
	if w < 55 {
		cs.last = false
		cs.activity = false
	}
	return cs
}

func (m *agentsModel) agentColWidth() int {
	w := 8
	for _, r := range m.rows {
		n := len(r.AgentName)
		if n == 0 {
			n = len("lead") + 1 + 8 // "lead" + "·" + 8-char session prefix
			if tn := len(r.TeamName) + 1; r.TeamName != "" && tn > n {
				n = tn // "#" + team name, when the session header prefers it
			}
		} else if !strings.HasPrefix(r.AgentName, "@") {
			n++ // "@" prefix
		}
		if n > w {
			w = n
		}
	}
	if w > 12 {
		w = 12
	}
	return w
}

func agentCountLabel(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return humanizeCount(n) + " agents"
}

// visibleAgentCounts computes the status bar's headline agent numbers from
// the actually-rendered row set (m.visRows — one line per collapsed session,
// or per row of an expanded one) rather than the flattened m.rows, which
// also counts every teammate hidden inside a still-collapsed session.
// Without this, the status bar could read "8 agents (0 live · 1 idle)" — 8
// being every teammate across every session whether visible or not, while
// live/idle counted only that same full (mostly closed/stale) set. active
// is live+idle, closed is closed+stale; a row whose Liveness is "unbound" or
// unknown counts toward neither.
func (m *agentsModel) visibleAgentCounts() (visible, active, closed int) {
	for _, vr := range m.visRows {
		visible++
		switch vr.agent.Liveness {
		case "live", "idle":
			active++
		case "closed", "stale":
			closed++
		}
	}
	return visible, active, closed
}

// tintBaseRGB is the dark terminal-like background blendBG tints toward
// (#0d1117 — GitHub's dark-mode canvas color, a reasonable default "empty
// terminal" base regardless of what the operator's actual terminal theme
// is).
var tintBaseRGB = [3]int{13, 17, 23}

// teamTintRGB returns a session group's background-tint source color:
// EntityColor of its team_name (matching this file's existing,
// session-independent convention for coloring team/agent names — see
// renderRow's own `EntityColor(r.AgentName, "")` call), or of the sessionID
// when no team_name is set, so a teamless session still gets a stable,
// distinguishable tint instead of every teamless session blending into one
// color.
func teamTintRGB(g agentGroup) [3]int {
	team := ""
	if len(g.rows) > 0 {
		team = g.rows[0].TeamName
	}
	if team != "" {
		return display.EntityColor(team, "")
	}
	return display.EntityColor(g.sessionID, "")
}

// blendBG blends c toward tintBaseRGB at the given opacity (0..1): each
// channel is base + tint*opacity, clamped to [0,255]. 0.12 is the resting
// per-session tint; 0.25 (the row under the cursor) reads as a visibly
// brighter highlight without fully replacing the base the way the old
// hardcoded-256-color group shading did.
func blendBG(c [3]int, opacity float64) [3]uint8 {
	blend := func(base, tint int) uint8 {
		v := base + int(float64(tint)*opacity)
		if v > 255 {
			v = 255
		}
		if v < 0 {
			v = 0
		}
		return uint8(v)
	}
	return [3]uint8{
		blend(tintBaseRGB[0], c[0]),
		blend(tintBaseRGB[1], c[1]),
		blend(tintBaseRGB[2], c[2]),
	}
}

// applyRowTint re-injects bg immediately after every embedded SGR reset in s
// — every lipgloss/display-rendered cell ends with one, which otherwise
// wipes out a background applied earlier in the line, since neither parses
// the string it wraps — then pads to width so the tint reaches the panel's
// border with no unshaded gap, and appends one final plain RESET. Without
// that trailing reset, a line whose LAST emitted code is the bg escape (any
// row not ending in more foreground-colored text) leaks its background into
// whatever the terminal renders next — the following row, the TOTAL
// footer, or the blank padding rows below it, since SGR state persists
// across a bare "\n" rather than resetting automatically. Reintroduces the
// pre-collapse shadeReset + padToExactWidth combo (removed when per-group
// shading went away), now keyed by a per-team/session RGB instead of one
// hardcoded 256-color. No-op when bg == "" (colorize off, or nothing to
// tint).
func applyRowTint(s, bg string, width int) string {
	if bg == "" {
		return s
	}
	if w := lipgloss.Width(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return bg + strings.ReplaceAll(s, display.RESET, display.RESET+bg) + display.RESET
}

// View renders the agents grid panel at the given content width/height
// (interior cells, borders excluded by the caller). selectedComp is the
// Detail panel's currently-loaded composition breakdown for the selected
// row (nil unless a row is selected and its snapshot has been fetched) —
// preferred over that row's own (possibly slightly stale) list-response
// composition_json when both are available. colorize gates the
// team-identity background tint (see teamTintRGB/blendBG/applyRowTint) —
// off under --no-color, same as every other color this file emits, though
// those are stripped later by the top-level model.View(); gating here too
// avoids computing tints that would just get thrown away.
func (m *agentsModel) View(width, height int, focused bool, selectedComp *compositionJSON, colorize bool) string {
	title := "muster · agents (" + humanizeCount(len(m.rows)) + " · max-age " + formatAge(m.maxAge) + ")"
	cs := columnsForWidth(width, multipleHostsPresent(m.rows))
	agentW := m.agentColWidth()

	var header strings.Builder
	// Two leading spaces match the 1-char cursor marker + 1-char fold/indent
	// glyph every data row starts with (renderRow's `marker + fold +
	// nameCell`) — without them every column header sits cells left of the
	// data underneath it.
	header.WriteString("  " + padTrunc("AGENT", agentW))
	hg := newCellGroup()
	if cs.host {
		hg.add(padTrunc("HOST", 5))
	}
	if cs.model {
		hg.add(padTrunc("MODEL", 8))
	}
	hg.next()
	hg.add(padTrunc("ST", 3))
	hg.add(padTrunc("CTX", 11))
	hg.next()
	if cs.tokens {
		hg.add(padRight("IN", 5))
		hg.add(padRight("OUT", 5))
	}
	if cs.cost {
		hg.add(padRight("COST", 5))
	}
	if cs.toolCalls {
		hg.add(padRight("T", 4))
	}
	hg.next()
	if cs.activity {
		hg.add("ACTIVITY")
	}
	header.WriteString(nameGroupSep(cs) + hg.render())
	headerStr := header.String()
	if cs.last {
		headerStr = rightAnchorTrailing(headerStr, padRight("LAST", 4), width)
	}
	headerLine := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorDim).Render(padTrunc(headerStr, width))

	rowsAvail := height - 2 // title band row + header row (no border box anymore — see titleBand)
	if rowsAvail < 0 {
		rowsAvail = 0
	}

	// Precompute each session's tint source color once — teamTintRGB reads
	// from group.rows[0], never changes for the life of this View() call.
	var tints map[string][3]int
	if colorize {
		tints = make(map[string][3]int, len(m.groups))
		for _, g := range m.groups {
			tints[g.sessionID] = teamTintRGB(g)
		}
	}

	// Build one line per visRow — a session header (collapsed summary, or an
	// expanded session's anchor line) or a member row nested beneath an
	// expanded session (which keeps its parent's tint — both share
	// vr.sessionID as the tints lookup key). One line per entry means the
	// cursor index lines up directly with the line index — no separate
	// SUBTOTAL bookkeeping needed now that a collapsed session IS the
	// summary line.
	lines := make([]string, 0, len(m.visRows))
	for i, vr := range m.visRows {
		isCursor := i == m.cursor
		agent := vr.agent
		if vr.isHeader {
			agent = headerDisplayAgent(vr)
		}
		var rowComp *compositionJSON
		if m.selected != "" && agent.SelectionKey() == m.selected {
			rowComp = selectedComp
		}
		fold := foldIndicator(vr)
		text := display.TruncateLine(m.renderRow(agent, isCursor, cs, agentW, rowComp, fold, vr.isHeader, width), width)
		if colorize {
			opacity := 0.12
			if isCursor {
				opacity = 0.25
			}
			rgb := blendBG(tints[vr.sessionID], opacity)
			bg := display.BGRGB(rgb[0], rgb[1], rgb[2])
			text = applyRowTint(text, bg, width)
		}
		lines = append(lines, text)
	}

	start := 0
	if m.cursor >= rowsAvail {
		start = m.cursor - rowsAvail + 1
	}
	end := start + rowsAvail
	if end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		start = len(lines)
	}
	if start < 0 {
		start = 0
	}

	body := append([]string{}, lines[start:end]...)
	if len(body) < rowsAvail && len(m.rows) > 0 {
		body = append(body, display.TruncateLine(m.renderTotalRow(cs, agentW, width), width))
	}
	body = padRows(body, rowsAvail)
	// Every line rendered by this panel must be exactly `width` visible
	// cells: with the bordered box gone (see titleBand), there's no outer
	// lipgloss.Width() call left to pad short lines for us — a data row
	// under --no-color (skips applyRowTint's own padding), the TOTAL row,
	// and blank filler rows from padRows would otherwise come out ragged
	// against detail/activity's still-bordered (and therefore always
	// full-width) panels below.
	for i := range body {
		if w := lipgloss.Width(body[i]); w < width {
			body[i] += strings.Repeat(" ", width-w)
		}
	}

	band := m.titleBand(title, width, focused, colorize)
	return band + "\n" + headerLine + "\n" + strings.Join(body, "\n")
}

// foldIndicator renders the 1-cell glyph shown right after the cursor
// marker: a caret for a session header with subagents (▸ collapsed, ▾
// expanded), a dim connector dot for a member row nested under an expanded
// session, or a blank cell for a solo session's header — nothing to fold.
func foldIndicator(vr visRow) string {
	if !vr.isHeader {
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("·")
	}
	if !vr.hasChildren {
		return " "
	}
	if vr.expanded {
		return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("▾")
	}
	return lipgloss.NewStyle().Foreground(tui.ColorDim).Render("▸")
}

// headerDisplayAgent returns the Agent renderRow renders for a session
// header line: vr.agent (the group's representative — the lead, if one
// exists) with its token/tool-call totals overridden to the group's
// aggregate (see agentGroup.tokensInTotal etc.) — those genuinely are
// per-agent contributions, unlike SessionCostUSD, which the representative's
// own value already carries as the whole session's total (Claude Code's own
// cost meter, posted only by the main statusLine — see the doc comment on
// activityKey's neighbors in this file for the teammate-side-effect this
// relies on), so cost is left untouched. When the representative has no
// AgentName (the lead), the AGENT column shows sessionAlias's "#team_name" /
// truncated-focus / "solo·<prefix>" / "?·<prefix>" form instead of
// renderRow's bare "lead·<prefix>" fallback — with TEAM gone, this is the
// only textual session identity left in the row. CurrentFocus is passed
// through as the focus fallback: the health API's team_name is empty for
// plenty of live sessions, but the WMS focus string usually isn't.
func headerDisplayAgent(vr visRow) Agent {
	a := vr.agent
	a.TokensInTotal = vr.tokensIn
	a.TokensOutTotal = vr.tokensOut
	a.ToolCallsTotal = vr.toolCalls
	if a.AgentName == "" {
		a.AgentName = sessionAlias(a.TeamName, a.SessionID, vr.hasChildren, a.CurrentFocus)
	}
	return a
}

// rowDim is the row-level dimming ladder (§ctop AgentAF redesign item 3):
// processing and idle both read as fully healthy now — idle is a finished
// turn awaiting the next dispatch, not degradation — so the ONLY thing that
// dims a row is being inactive (alive but no activity for
// --inactive-after) or closed.
type rowDim int

const (
	dimNone  rowDim = iota // processing or idle (any liveness that isn't closed, and not inactive) — full color, unchanged
	dimHalve               // inactive — each cell's own hue survives, channel-halved (see halveRGB/renderDim)
	dimFlat                // closed — the entire row flattens to ColorDim, name included, hollow status+activity dots
)

// rowDimLevel derives the dimming level from Liveness and the row's own
// inactive bool (see agentsModel.isInactive) — liveness alone is no longer
// enough: a "stale" liveness with recent-enough activity (isInactive false)
// reads as full color same as live/idle, and any non-closed liveness with
// isInactive true dims the same way a genuinely "live" row would once it
// goes quiet.
func rowDimLevel(liveness string, inactive bool) rowDim {
	return dimNone
}

// halveRGB returns rgb with each channel halved (integer division) — the
// "inactive" row's dim treatment: brightness drops, hue survives, unlike
// "closed"'s flat ColorDim substitution.
func halveRGB(rgb [3]int) [3]int {
	return [3]int{rgb[0] / 2, rgb[1] / 2, rgb[2] / 2}
}

// renderDim renders text in rgb, halved when dim==dimHalve — the shared
// primitive every per-cell color in a data row goes through so an
// "inactive" row keeps each cell's own hue (just darker) instead of the
// "closed" row's flat single-color treatment. dimNone/dimFlat both render
// rgb unchanged here: dimFlat's flattening is applied once, as a whole-row
// post-process, by renderRow itself (see rowDimLevel's doc comment) — not
// per-cell, since StripANSI there needs the row's real colors intact first.
func renderDim(rgb [3]int, text string, dim rowDim) string {
	if dim == dimHalve {
		rgb = halveRGB(rgb)
	}
	return display.RGB(rgb[0], rgb[1], rgb[2]) + text + display.RESET
}

// metricStyle is the cyan applied to every economics-block cell (IN/OUT/
// COST/T, and the TOTAL row's aggregates — see renderSummaryRow) so the
// numbers read as one visually grouped block instead of blending into the
// default text color.
var metricStyle = lipgloss.NewStyle().Foreground(tui.ColorMetric)

func (m *agentsModel) renderRow(r Agent, selected bool, cs colSet, agentW int, comp *compositionJSON, fold string, isHeader bool, width int) string {
	marker := " "
	if selected {
		marker = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorAccent).Render("▸")
	}

	now := time.Now()
	inactive := m.isInactive(r)
	midTurn := m.midTurnFor(r, now)
	dim := rowDimLevel(r.Liveness, inactive)

	ac := display.EntityColor(r.AgentName, "")
	if strings.HasPrefix(r.AgentName, "#") {
		// Color the "#team_name" alias by the raw team name, matching
		// teamTintRGB's own EntityColor(team, "") — not by the "#"-prefixed
		// string, which would hash to an unrelated color and desync the text
		// from the row's own background tint.
		ac = display.EntityColor(strings.TrimPrefix(r.AgentName, "#"), "")
	}
	if dim == dimHalve {
		ac = halveRGB(ac)
	}
	var nameCell string
	if r.AgentName == "" {
		// Every session's lead has AgentName == "" — with no per-lead text
		// or column to tell them apart, a multi-session dashboard shows N
		// indistinguishable "lead" rows. The dim "·<session prefix>" suffix
		// gives each one a unique, stable identity without a new column. Once
		// team_name is known, the row's own background tint already carries
		// that identity, so the suffix (and the space it costs) is dropped.
		colored := display.RGB(ac[0], ac[1], ac[2]) + "lead" + display.RESET
		if r.TeamName == "" {
			if sess := sessionPrefix8(r.SessionID); sess != "" {
				colored += lipgloss.NewStyle().Foreground(tui.ColorDim).Render("·" + sess)
			}
		}
		nameCell = padTrunc(colored, agentW)
	} else {
		name := r.AgentName
		// headerDisplayAgent synthesizes a sessionAlias AgentName for a
		// session header ("#team_name" / a truncated focus string /
		// "solo·<prefix>" / "?·<prefix>") — none of these are real API
		// agent_names, so none get the "@" prefix a plain teammate name
		// gets. isHeader is the authoritative signal for that (a sessionAlias
		// value can be arbitrary free-text — the focus fallback — so content
		// alone, e.g. a leading "@"/"#" or a "·", can no longer be trusted to
		// tell the two apart).
		if !isHeader && !strings.HasPrefix(name, "@") && !strings.HasPrefix(name, "#") {
			name = "@" + name
		}
		nameCell = display.RGB(ac[0], ac[1], ac[2]) + padTrunc(name, agentW) + display.RESET
	}
	nameSection := marker + fold + nameCell

	rb := newCellGroup()
	if cs.host {
		rb.add(padTrunc(r.Host, 5))
	}
	if cs.model {
		rb.add(padTrunc(modelAbbrev(r.Model), 8))
	}
	rb.next()
	rb.add(padTrunc(statusDots(r.Liveness, r.Runtime, r.PressureLevel, inactive, midTurn, now), 3))

	// The list API carries composition_json on every row now, not just a
	// fetched snapshot — prefer the Detail panel's freshly-fetched snapshot
	// composition for the selected row (comp, passed in, most current),
	// else fall back to this row's own value, so gridSegmentedBar's
	// composition coloring is the default for every row rather than the
	// selected one only.
	rowComp := comp
	if rowComp == nil {
		rowComp = parseComposition(r.CompositionJSON)
	}
	rb.add(padTrunc(gridSegmentedBar(r.ContextFillPct, r.PressureLevel, rowComp, dim), 11))
	rb.next()

	if cs.tokens {
		rb.add(renderDim(metricRGB, padRight(humanizeTokens(r.TokensInTotal), 5), dim))
		rb.add(renderDim(metricRGB, padRight(humanizeTokens(r.TokensOutTotal), 5), dim))
	}
	if cs.cost {
		rb.add(renderDim(metricRGB, padRightNoTrunc(fmtCost(r.SessionCostUSD), 5), dim))
	}
	if cs.toolCalls {
		rb.add(renderDim(metricRGB, padRight(fmtToolCount(r.ToolCallsTotal), 4), dim))
	}
	rb.next()
	// ACTIVITY is the row's only variable-width cell (raw display text, no
	// padTrunc) — LAST/AGE is pulled out of this cellGroup entirely (see
	// below) rather than added here, since a fixed 2-space gap after a
	// ragged-width ACTIVITY cell would still leave AGE landing at a
	// different screen column on every row.
	if cs.activity {
		rb.add(m.activityCellFor(r))
	}
	rest := rb.render()

	var ageCell string
	if cs.last {
		ageCell = padRight(relativeTime(m.activityTsFor(r)), 4)
	}

	// "closed" flattens the whole row to ColorDim — name included — as a
	// post-process: strip whatever colors each cell picked for itself and
	// re-render the whole chunk in one flat style. "inactive" (dimHalve)
	// never reaches here — every color-producing cell above already baked
	// its own halved RGB in via ac/renderDim/gridSegmentedBar's dim param,
	// so StripANSI would destroy exactly the hue those cells were trying to
	// preserve.
	if dim == dimFlat {
		nameSection = lipgloss.NewStyle().Foreground(tui.ColorDim).Render(display.StripANSI(nameSection))
		rest = lipgloss.NewStyle().Foreground(tui.ColorDim).Render(display.StripANSI(rest))
		if ageCell != "" {
			ageCell = lipgloss.NewStyle().Foreground(tui.ColorDim).Render(display.StripANSI(ageCell))
		}
	}

	line := nameSection + nameGroupSep(cs) + rest
	if ageCell != "" {
		line = rightAnchorTrailing(line, ageCell, width)
	}
	return line
}

// rightAnchorTrailing pads s with spaces so that appending trailing (a
// short, fixed-width cell — the AGE/LAST column, in both the header and
// every data row) lands flush against the panel's right edge at width,
// separated from whatever precedes it by the standard 2-space group gap —
// the htop-style "rightmost column always lines up" behavior AGE needs,
// independent of how long a variable-width cell earlier in the row (e.g.
// ACTIVITY's raw display text) happens to be. Falls back to a plain 2-space
// join when s already fills (or overflows) the available width — the
// caller's own display.TruncateLine(..., width) clips any remainder the
// same way it always has, so a too-narrow panel or too-long ACTIVITY text
// degrades exactly as before rather than panicking or corrupting output.
func rightAnchorTrailing(s, trailing string, width int) string {
	target := width - 2 - lipgloss.Width(trailing)
	if w := lipgloss.Width(s); target > w {
		s += strings.Repeat(" ", target-w)
	}
	return s + "  " + trailing
}

// renderTotalRow renders the grand-total footer line across every row
// currently loaded (every session's every agent, header or member), not
// just the visible slice. The normally-blank TEAM/HOST/MODEL/ST/CTX span
// shows the most-recently-active session's current_focus instead of dead
// space, when one is available: m.groups is sorted by recency
// (sortGroupsByRecency), so m.groups[0]'s rows[0] — the lead, per
// pinLeadsFirst — IS that session.
func (m *agentsModel) renderTotalRow(cs colSet, agentW, width int) string {
	var focus string
	if len(m.groups) > 0 && len(m.groups[0].rows) > 0 {
		focus = m.groups[0].rows[0].CurrentFocus
	}
	return renderSummaryRow("TOTAL", m.rows, cs, agentW, focus, width)
}

// blankSpanWidth returns the total visible-cell width of the leading
// group-separator (nameGroupSep) plus the HOST/MODEL/ST/CTX groups a data
// row renders before its tokens group starts — built via the exact same
// cellGroup machinery renderRow uses (for just the first two groups), so
// the TOTAL row's focus text or blank filler always lines up under real
// rows' columns regardless of which columns cs currently shows.
func blankSpanWidth(cs colSet) int {
	hg := newCellGroup()
	if cs.host {
		hg.add(strings.Repeat(" ", 5))
	}
	if cs.model {
		hg.add(strings.Repeat(" ", 8))
	}
	hg.next()
	hg.add(strings.Repeat(" ", 3))  // ST
	hg.add(strings.Repeat(" ", 11)) // CTX
	return len(nameGroupSep(cs)) + lipgloss.Width(hg.render())
}

// renderSummaryRow renders the grand TOTAL footer line: bold, same columns
// as a data row but only sums. The HOST/MODEL/ST/CTX span — nothing to sum
// there — renders focus (dim, "◎ " prefixed, truncated to fit) if given,
// else stays blank as before. Its "  " prefix matches the marker+fold
// columns every data row starts with (see renderRow), so it lines up under
// them. Token/cost/tool-call aggregates use the same cellGroup grouping (and
// cyan metricStyle) as a data row's own economics block, so this row's
// columns stay aligned with the rows above it as the column diet's group
// spacing evolves.
func renderSummaryRow(label string, rows []Agent, cs colSet, agentW int, focus string, width int) string {
	var sb strings.Builder
	sb.WriteString("  " + padTrunc(label, agentW))

	sep := nameGroupSep(cs)
	span := blankSpanWidth(cs)
	if focus != "" {
		text := lipgloss.NewStyle().Foreground(tui.ColorDim).Render(padTrunc("◎ "+focus, span-len(sep)))
		// Re-establish Bold (wiped by the dim style's own embedded RESET)
		// so the tokens/cost/etc columns after it stay bold like the rest
		// of this row — see the outer Bold wrap at the bottom.
		sb.WriteString(sep + text + display.BOLD)
	} else {
		sb.WriteString(sep + padTrunc("", span-len(sep)))
	}

	var cost float64
	var tokIn, tokOut, toolCalls int64
	for _, r := range rows {
		cost += r.SessionCostUSD
		tokIn += r.TokensInTotal
		tokOut += r.TokensOutTotal
		toolCalls += r.ToolCallsTotal
	}

	rb := newCellGroup()
	if cs.tokens {
		rb.add(metricStyle.Render(padRight(humanizeTokens(tokIn), 5)))
		rb.add(metricStyle.Render(padRight(humanizeTokens(tokOut), 5)))
	}
	if cs.cost {
		rb.add(metricStyle.Render(padRightNoTrunc(fmtCost(cost), 5)))
	}
	if cs.toolCalls {
		rb.add(metricStyle.Render(padRight(fmtToolCount(toolCalls), 4)))
	}
	rb.next()
	if cs.activity {
		rb.add(agentCountLabel(len(rows)))
	}
	if rest := rb.render(); rest != "" {
		sb.WriteString("  " + rest)
	}

	line := sb.String()
	if cs.last {
		line = rightAnchorTrailing(line, padRight("", 4), width)
	}
	return lipgloss.NewStyle().Bold(true).Render(line)
}

func padRows(rows []string, n int) []string {
	for len(rows) < n {
		rows = append(rows, "")
	}
	return rows
}

// borderTitle embeds a title into the top border area as a plain text line
// above the content (simple, robust across terminal widths — avoids
// depending on lipgloss's border-title rendering quirks). Still used by
// detail.go/activity.go/cost_view.go/focus_view.go's bordered panels — the
// agents panel dropped its border in favor of titleBand below.
func borderTitle(title string, width int) string {
	return lipgloss.NewStyle().Bold(true).Foreground(tui.ColorAccent).Render(padTrunc(title, width))
}

// nameGroupSep picks the separator between the AGENT cell and the first
// column group: a single space when HOST or MODEL is shown (they're
// visually part of the AGENT group), else the standard 2-space group gap,
// since the next thing rendered is the ST+CTX group.
func nameGroupSep(cs colSet) string {
	if cs.host || cs.model {
		return " "
	}
	return "  "
}

// titleBandBgRGB / titleBandFocusedBgRGB are the agents panel's 1-line
// title-band background (replacing the old bordered box — see View() and
// borderTitle's doc comment): tui.ColorHighBg (#161b22) at rest, a touch
// brighter when focused, so the operator can tell which panel currently has
// keyboard focus without a border to highlight it.
var titleBandBgRGB = [3]int{22, 27, 34}
var titleBandFocusedBgRGB = [3]int{30, 38, 48}

// titleBand renders the agents panel's 1-line header band: the title text
// bold-accented on the left, an optional dim composition legend
// right-aligned when composition data is present anywhere in the
// currently-loaded rows (see anyCompositionData) — full-width background,
// reusing applyRowTint's reinject-after-RESET trick so the title's own
// embedded style resets don't wipe the band's background out partway
// through the line the way a naive single Render() would. colorize gates
// only the background (same convention as View()'s own row tint) — the
// title/legend foreground colors are always emitted and rely on the
// top-level model.View()'s blanket ANSI strip under --no-color, matching
// every other color this file emits.
func (m *agentsModel) titleBand(title string, width int, focused, colorize bool) string {
	left := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorAccent).Render(title)
	var right string
	if m.anyCompositionData() {
		right = compositionLegend()
	}
	return renderTitleBand(left, right, width, focused, colorize)
}

// anyCompositionData reports whether at least one currently-loaded row has
// a parseable composition_json — the CTX bar's segmented coloring is only
// ever active for such rows, so the title band's composition legend is only
// worth showing when it actually applies to something on screen.
func (m *agentsModel) anyCompositionData() bool {
	for _, r := range m.rows {
		if parseComposition(r.CompositionJSON) != nil {
			return true
		}
	}
	return false
}

// compositionLegend renders the dim "▓text ▓tool ▓think ░free" key for the
// CTX bar's composition coloring, each glyph in the same color
// compositionBar uses for that segment.
func compositionLegend() string {
	text := lipgloss.NewStyle().Foreground(tui.ColorText).Render("▓text")
	tool := lipgloss.NewStyle().Foreground(tui.ColorAccent).Render("▓tool")
	think := lipgloss.NewStyle().Foreground(tui.ColorWarn).Render("▓think")
	free := lipgloss.NewStyle().Foreground(tui.ColorDim).Render("░free")
	return strings.Join([]string{text, tool, think, free}, " ")
}
