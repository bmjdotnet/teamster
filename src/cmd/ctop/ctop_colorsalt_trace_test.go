package main

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/render"
)

// Validation trace for GitHub issue #20: does fleet_view.go's tree-row
// color for a nested sub-subagent equal render.go's activity-view color
// for that same sub-subagent's own activity line? Calls the actual
// production rendering functions (renderAgentRow, render.FormatLine) —
// not a reimplementation of their salt logic — on data constructed the
// way server.go's registerNewSubagentInstance actually produces it:
// SessionID := event.SessionID for every level (lead, teammate,
// sub-subagent), since a Task-tool sub-subagent has no top-level session
// of its own — it runs inside its spawner's physical session.
func TestIssue20_FleetTreeVsActivityView_SubSubagent(t *testing.T) {
	const hostSessionID = "sess-teammate-abc123" // the physical session hosting both the teammate AND its own Task-tool sub-subagent — this pattern reproduces hub-local (Linux) too, not just macOS

	teammateRosterID := "roster-teammate-1"
	subSubRosterID := "roster-subsub-1"
	subSubagent := Agent{
		RosterID:     &subSubRosterID,
		SessionID:    hostSessionID, // per registerNewSubagentInstance: sessionID := event.SessionID
		AgentName:    "@scanner",
		Liveness:     "live",
		Relationship: "subagent",
		ParentRef:    &teammateRosterID,
	}
	teammate := Agent{
		RosterID:     &teammateRosterID,
		SessionID:    hostSessionID,
		AgentName:    "@colorhash",
		Liveness:     "live",
		Relationship: "teammate",
	}

	// Fleet tree side: real fleetTreeRows + real renderAgentRow, exactly
	// the path fleet_view.go's Fleet view uses.
	rows := fleetTreeRows([]Agent{teammate, subSubagent}, false)
	m := model{width: 160, colorize: true}
	v := fleetView{m: &m}
	cs := fleetColumnsForWidth(160)
	cs, layout := fleetLayoutFor(160, 8, cs)

	var fleetOut string
	found := false
	for _, fr := range rows {
		if fr.isHeader || fr.agent.AgentName != "@scanner" {
			continue
		}
		fleetOut = v.renderAgentRow(fr, false, cs, layout, 160)
		found = true
	}
	if !found {
		t.Fatalf("sub-subagent row not found in fleetTreeRows output")
	}

	// Activity view side: real render.FormatLine — the same function
	// ctop's activity log (activity.go) and feed both call — on the
	// sub-subagent's own JSONL event. hookd stamps "session" with the same
	// event.SessionID as above (identical field, same physical session),
	// "agent_name" with the sub-subagent's own name.
	rec := render.Record{
		Session:   hostSessionID,
		AgentName: "@scanner",
		Tag:       "READ",
		Display:   "reading file",
	}
	lines := render.FormatLine(rec, nil, 20, 20, func(string) string { return "" })
	if len(lines) == 0 {
		t.Fatalf("render.FormatLine produced no lines for @scanner")
	}
	activityOut := strings.Join(lines, "\n")

	wantColor := display.EntityColor("@scanner", hostSessionID)
	wantRGB := display.RGB(wantColor[0], wantColor[1], wantColor[2])

	if !strings.Contains(fleetOut, wantRGB) {
		t.Errorf("fleet tree row for @scanner does not contain the expected session-salted color %v (row=%q)", wantColor, fleetOut)
	}
	if !strings.Contains(activityOut, wantRGB) {
		t.Errorf("activity view line for @scanner does not contain the expected session-salted color %v (line=%q)", wantColor, activityOut)
	}
}
