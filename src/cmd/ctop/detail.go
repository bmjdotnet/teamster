package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/tui"
)

type detailMode int

const (
	detailAlerts detailMode = iota
	detailSnapshot
)

// detailModel is the Detail-or-Alerts sub-model (§2.6). Default view is
// Alerts; selecting an agent in the grid switches to Snapshot.
type detailModel struct {
	mode        detailMode
	alerts      []Agent
	rosterID    string // "" when nothing selected -> Alerts view
	selectedRow *Agent // fallback grid-row fields when the agent has no roster_id
	snapshot    *Snapshot
	snapshotErr error
}

func (d *detailModel) setAlerts(alerts []Agent) {
	sort.SliceStable(alerts, func(i, j int) bool {
		return pressureRank(alerts[i].PressureLevel) < pressureRank(alerts[j].PressureLevel)
	})
	d.alerts = alerts
}

// selectAgent switches to Snapshot mode for row. If row has no roster_id,
// there is nothing to fetch — show the grid row's own fields.
func (d *detailModel) selectAgent(row Agent) {
	d.mode = detailSnapshot
	rowCopy := row
	d.selectedRow = &rowCopy
	d.snapshot = nil
	d.snapshotErr = nil
	if row.RosterID != nil && *row.RosterID != "" {
		d.rosterID = *row.RosterID
	} else {
		d.rosterID = ""
	}
}

func (d *detailModel) clear() {
	d.mode = detailAlerts
	d.rosterID = ""
	d.selectedRow = nil
	d.snapshot = nil
	d.snapshotErr = nil
}

func (d *detailModel) setSnapshot(snap *Snapshot, err error) {
	d.snapshot = snap
	d.snapshotErr = err
}

// View renders the panel at the given content width/height. No bordered
// box any more — a 1-line title band (see agents.go's titleBand doc
// comment) replaces it, so the outer rendered size is exactly width x
// height, matching what model.go budgets per panel.
func (d *detailModel) View(width, height int, focused, colorize bool) string {
	bodyH := height - 1 // title band row
	if bodyH < 0 {
		bodyH = 0
	}
	var title, body string
	if d.mode == detailSnapshot && d.selectedRow != nil {
		title = "detail · @" + d.selectedRow.AgentName
		body = d.viewSnapshot(width, bodyH)
	} else {
		title = "alerts (" + humanizeCount(len(d.alerts)) + ")"
		body = d.viewAlerts(width, bodyH)
	}

	band := plainTitleBand(title, width, focused, colorize)
	return band + "\n" + body
}

func (d *detailModel) viewAlerts(width, height int) string {
	if len(d.alerts) == 0 {
		msg := lipgloss.NewStyle().Foreground(tui.ColorDim).Render("all agents ok")
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, msg)
	}
	var lines []string
	for _, a := range d.alerts {
		if len(lines) >= height {
			break
		}
		since := relativeTime(parseRFC3339(strOrEmpty(a.LastActivityTs)))
		line := fmt.Sprintf("%s @%-14s %-20s %3.0f%%  since %s  %s",
			pressureBadge(a.PressureLevel), a.AgentName, a.Host+"/"+a.TeamName,
			a.ContextFillPct*100, since, a.CurrentFocus)
		lines = append(lines, padTrunc(line, width))
	}
	return strings.Join(padRows(lines, height), "\n")
}

func (d *detailModel) viewSnapshot(width, height int) string {
	var lines []string
	row := d.selectedRow

	if d.snapshotErr != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(tui.ColorError).Render("snapshot fetch failed: "+d.snapshotErr.Error()))
	}

	if d.rosterID == "" {
		lines = append(lines, fmt.Sprintf("agent   @%s   host %s   runtime %s   model %s", row.AgentName, row.Host, row.Runtime, row.Model))
		lines = append(lines, fmt.Sprintf("liveness %s   pressure %s   team %s", livenessBadge(row.Liveness), pressureBadge(row.PressureLevel), fallbackDash(row.TeamName)))
		lines = append(lines, fmt.Sprintf("context %s   cost %s", SegmentedBar(row.ContextFillPct, row.PressureLevel, nil), fmtCost(row.SessionCostUSD)))
		lines = append(lines, fmt.Sprintf("focus   %s", fallbackDash(row.CurrentFocus)))
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(tui.ColorDim).Render("no snapshot: agent has no roster binding"))
		return strings.Join(padRows(truncateAll(lines, width), height), "\n")
	}

	if d.snapshot == nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(tui.ColorDim).Render("loading snapshot…"))
		return strings.Join(padRows(truncateAll(lines, width), height), "\n")
	}

	s := d.snapshot
	comp := parseComposition(s.CompositionJSON)
	lines = append(lines, fmt.Sprintf("roster  %-14s   session  %-14s   relationship %-10s   parent %-10s   liveness %s",
		truncatePlain(d.rosterID, 14), truncatePlain(s.SessionID, 14), fallbackDash(s.Relationship), fallbackDash(strOrEmpty(s.ParentRef)), livenessBadge(s.Liveness)))
	lines = append(lines, fmt.Sprintf("context %s  (%s / %s · %s free)   long-context %s   reset-suspected %s",
		SegmentedBar(s.ContextFillPct, s.PressureLevel, comp),
		thousands(s.ContextTokensUsed), thousands(s.ContextWindowTokens), thousands(s.ContextTokensFree),
		yesNo(s.LongContextActive), yesNo(s.ContextResetSuspected)))
	// D6: collector_status appears ONLY here in the Detail panel.
	lines = append(lines, fmt.Sprintf("pressure %s since —        collector %s", pressureBadge(s.PressureLevel), fallbackDash(s.CollectorStatus)))
	lines = append(lines, fmt.Sprintf("tokens  in %s · out %s   cost %s   model %s · runtime %s · host %s",
		humanizeTokens(s.TokensInTotal), humanizeTokens(s.TokensOutTotal), fmtCost(s.SessionCostUSD), modelShort(s.Model), s.Runtime, s.Host))
	lines = append(lines, fmt.Sprintf("focus   %s", fallbackDash(s.CurrentFocus)))

	if comp != nil {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf(
			"composition  %s text %3.0f%%  %s tool_use %3.0f%%  %s thinking %3.0f%%  reading %3.0f%%",
			lipgloss.NewStyle().Foreground(tui.ColorText).Render("■"), comp.TextPct*100,
			lipgloss.NewStyle().Foreground(tui.ColorAccent).Render("■"), comp.ToolUsePct*100,
			lipgloss.NewStyle().Foreground(tui.ColorWarn).Render("■"), comp.ThinkingPct*100,
			comp.ReadingPct*100,
		))
	}
	if s.ToolCallCountsJSON != nil {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Bold(true).Render("tool calls:"))
		lines = append(lines, topKeysByValue(*s.ToolCallCountsJSON)...)
	}
	if s.StatuslineJSON != nil {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Bold(true).Render("statusline:"))
		lines = append(lines, topKeysByValue(*s.StatuslineJSON)...)
	}
	if s.FidelityNotes != nil && *s.FidelityNotes != "" {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(tui.ColorDim).Render("~ "+*s.FidelityNotes))
	}

	return strings.Join(padRows(truncateAll(lines, width), height), "\n")
}

func truncateAll(lines []string, width int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = padTrunc(l, width)
	}
	return out
}

func fallbackDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func thousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// topKeysByValue parses a JSON object string and renders up to 5 "key value"
// lines sorted by value descending. Best-effort: unparseable input renders
// nothing. composition_json/tool_call_counts_json are currently never
// written by health-collector (§1.2 note), so this path is exercised only
// once a producer starts populating them — a simple textual top-N list is
// sufficient rather than a stacked bar visualization.
func topKeysByValue(raw string) []string {
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	type kv struct {
		k string
		v float64
	}
	kvs := make([]kv, 0, len(m))
	for k, v := range m {
		f, _ := toFloat(v)
		kvs = append(kvs, kv{k: k, v: f})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v })
	if len(kvs) > 5 {
		kvs = kvs[:5]
	}
	lines := make([]string, 0, len(kvs))
	for _, e := range kvs {
		lines = append(lines, fmt.Sprintf("  %-20s %v", e.k, e.v))
	}
	return lines
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return 0, false
	}
}
