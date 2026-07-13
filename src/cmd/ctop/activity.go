package main

import (
	"strings"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/render"
)

const activityBufCap = 1000

type activityFilter int

const (
	filterAll activityFilter = iota
	filterSelected
	filterAlerts
)

func (f activityFilter) label() string {
	switch f {
	case filterSelected:
		return "selected"
	case filterAlerts:
		return "alerts"
	default:
		return "all"
	}
}

// activityEntry pairs a record with the teamMap snapshot at event time,
// mirroring cmd/feed's bufEntry so FormatLine's teamFor closure works
// identically here.
type activityEntry struct {
	rec      render.Record
	teamSnap map[string]string
}

// activityModel is the Activity feed sub-model: ring buffer, teamMap,
// filter, viewOffset, paused (§2.7).
type activityModel struct {
	buf          []activityEntry // newest first, like cmd/feed
	teamMap      map[string]string
	filter       activityFilter
	selectedName string // agent_name of the current grid selection, for filterSelected
	viewOffset   int
	paused       bool
	agentWidth   int
	sessionWidth int
}

func newActivityModel() activityModel {
	return activityModel{
		teamMap:      make(map[string]string),
		agentWidth:   5,
		sessionWidth: render.SessionColMin,
	}
}

// append adds one incoming record, matching cmd/feed's teamMap maintenance
// and column-width tracking (main.go:289-306).
func (m *activityModel) append(r render.Record) {
	if r.Team != "" {
		m.teamMap[r.Session] = r.Team
	} else if r.Tool == "TeamDelete" {
		delete(m.teamMap, r.Session)
	}

	agent := r.AgentName
	if agent == "" {
		agent = "@lead"
	}
	teamFor := func(sid string) string { return m.teamMap[sid] }
	label, _ := render.SessionLabel(r, teamFor)
	if len(agent) > m.agentWidth {
		m.agentWidth = len(agent)
	}
	if lw := len(label); lw > m.sessionWidth && lw <= render.SessionColMax {
		m.sessionWidth = lw
	}

	if !render.IsDisplayable(r) {
		return
	}

	snap := make(map[string]string, len(m.teamMap))
	for k, v := range m.teamMap {
		snap[k] = v
	}
	e := activityEntry{rec: r, teamSnap: snap}
	m.buf = append([]activityEntry{e}, m.buf...)
	if len(m.buf) > activityBufCap {
		m.buf = m.buf[:activityBufCap]
	}

	// Frozen view (manual scroll or paused): keep showing the same rows by
	// advancing the offset past the newly-inserted entry (matches cmd/feed's
	// scrolled-viewport behavior, main.go:317-322).
	if m.paused || m.viewOffset > 0 {
		m.viewOffset++
		if max := m.maxOffset(); m.viewOffset > max {
			m.viewOffset = max
		}
	}
}

func (m *activityModel) maxOffset() int {
	n := len(m.filtered()) - 1
	if n < 0 {
		return 0
	}
	return n
}

func (m *activityModel) cycleFilter() {
	m.filter = (m.filter + 1) % 3
}

func (m *activityModel) togglePause() {
	m.paused = !m.paused
}

func (m *activityModel) scrollUp(n int) {
	m.viewOffset += n
	if max := m.maxOffset(); m.viewOffset > max {
		m.viewOffset = max
	}
}

func (m *activityModel) scrollDown(n int) {
	m.viewOffset -= n
	if m.viewOffset < 0 {
		m.viewOffset = 0
	}
}

func (m *activityModel) resumeFollow() {
	m.viewOffset = 0
}

// filtered returns the buffer entries matching the current filter, newest
// first — filtering happens at render time over the full buffer (§2.7).
func (m *activityModel) filtered() []activityEntry {
	if m.filter == filterAll {
		return m.buf
	}
	out := make([]activityEntry, 0, len(m.buf))
	for _, e := range m.buf {
		switch m.filter {
		case filterSelected:
			agent := e.rec.AgentName
			if agent == "" {
				agent = "@lead"
			}
			if agent == m.selectedName {
				out = append(out, e)
			}
		case filterAlerts:
			if e.rec.Tag == "WARN" {
				out = append(out, e)
			}
		}
	}
	return out
}

// View renders the panel at the given content width/height. No bordered box
// any more — a 1-line title band (see agents.go's titleBand doc comment)
// replaces it, so the outer rendered size is exactly width x height,
// matching what model.go budgets per panel.
func (m *activityModel) View(width, height int, focused, colorize bool) string {
	bodyH := height - 1 // title band row
	if bodyH < 0 {
		bodyH = 0
	}

	title := "activity"
	if m.filter != filterAll {
		title += " · filter: " + m.filter.label() + " (f)"
	}
	if m.paused {
		title += " · paused (p)"
	}

	entries := m.filtered()
	scrolled := m.viewOffset > 0

	start := m.viewOffset
	if start > len(entries) {
		start = len(entries)
	}
	end := start + bodyH
	if end > len(entries) {
		end = len(entries)
	}
	window := entries[start:end]

	var body []string
	for i := len(window) - 1; i >= 0; i-- {
		e := window[i]
		teamFor := func(sid string) string { return e.teamSnap[sid] }
		lines := render.FormatLine(e.rec, nil, m.agentWidth, m.sessionWidth, teamFor)
		for _, l := range lines {
			body = append(body, display.TruncateLine(l, width))
		}
	}
	if len(body) > bodyH {
		body = body[len(body)-bodyH:]
	}
	body = padRows(body, bodyH)

	if scrolled {
		title += "  ◂ scrolled (End)"
	}

	band := plainTitleBand(title, width, focused, colorize)
	return band + "\n" + strings.Join(body, "\n")
}
