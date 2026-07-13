package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/render"
	"github.com/bmjdotnet/teamster/internal/tui"
)

func strPtr(s string) *string { return &s }

func TestSortGroupMembersPressureOrder(t *testing.T) {
	m := &agentsModel{}
	g := agentGroup{rows: []Agent{
		{AgentName: "b", PressureLevel: "ok", ContextFillPct: 0.5},
		{AgentName: "a", PressureLevel: "critical", ContextFillPct: 0.9},
		{AgentName: "c", PressureLevel: "warning", ContextFillPct: 0.8},
	}}
	m.sortGroupMembers(&g)
	want := []string{"a", "c", "b"} // critical, warning, ok
	for i, w := range want {
		if g.rows[i].AgentName != w {
			t.Errorf("rows[%d] = %q, want %q (full order: %v)", i, g.rows[i].AgentName, w, agentNames(g.rows))
		}
	}
}

func TestSortGroupMembersPressureTiesBrokenByFillThenName(t *testing.T) {
	m := &agentsModel{}
	g := agentGroup{rows: []Agent{
		{AgentName: "z", PressureLevel: "ok", ContextFillPct: 0.1},
		{AgentName: "a", PressureLevel: "ok", ContextFillPct: 0.5},
		{AgentName: "m", PressureLevel: "ok", ContextFillPct: 0.5},
	}}
	m.sortGroupMembers(&g)
	// fill desc first (0.5, 0.5, 0.1), then name asc among ties (a before m).
	want := []string{"a", "m", "z"}
	for i, w := range want {
		if g.rows[i].AgentName != w {
			t.Errorf("rows[%d] = %q, want %q (full order: %v)", i, g.rows[i].AgentName, w, agentNames(g.rows))
		}
	}
}

func TestSortGroupMembersFillMode(t *testing.T) {
	m := &agentsModel{sort: sortFill}
	g := agentGroup{rows: []Agent{
		{AgentName: "low", ContextFillPct: 0.1},
		{AgentName: "high", ContextFillPct: 0.9},
		{AgentName: "mid", ContextFillPct: 0.5},
	}}
	m.sortGroupMembers(&g)
	want := []string{"high", "mid", "low"}
	for i, w := range want {
		if g.rows[i].AgentName != w {
			t.Errorf("rows[%d] = %q, want %q", i, g.rows[i].AgentName, w)
		}
	}
}

func TestSortGroupMembersNameMode(t *testing.T) {
	m := &agentsModel{sort: sortName}
	g := agentGroup{rows: []Agent{
		{AgentName: "zeta"},
		{AgentName: "alpha"},
		{AgentName: "mid"},
	}}
	m.sortGroupMembers(&g)
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if g.rows[i].AgentName != w {
			t.Errorf("rows[%d] = %q, want %q", i, g.rows[i].AgentName, w)
		}
	}
}

func TestSortGroupMembersFillModeTieBrokenByName(t *testing.T) {
	// Equal ContextFillPct must not depend on input order — otherwise rows
	// visibly jump between polls even though nothing about them changed.
	m := &agentsModel{sort: sortFill}
	g := agentGroup{rows: []Agent{
		{AgentName: "z", ContextFillPct: 0.5},
		{AgentName: "a", ContextFillPct: 0.5},
	}}
	m.sortGroupMembers(&g)
	want := []string{"a", "z"}
	for i, w := range want {
		if g.rows[i].AgentName != w {
			t.Errorf("rows[%d] = %q, want %q (full order: %v)", i, g.rows[i].AgentName, w, agentNames(g.rows))
		}
	}
}

func TestSortGroupMembersLastModeTieBrokenByName(t *testing.T) {
	m := &agentsModel{sort: sortLast}
	g := agentGroup{rows: []Agent{
		{AgentName: "z"}, // both have no known activity -> equal zero time
		{AgentName: "a"},
	}}
	m.sortGroupMembers(&g)
	want := []string{"a", "z"}
	for i, w := range want {
		if g.rows[i].AgentName != w {
			t.Errorf("rows[%d] = %q, want %q (full order: %v)", i, g.rows[i].AgentName, w, agentNames(g.rows))
		}
	}
}

func TestSortGroupMembersPinsLeadFirst(t *testing.T) {
	for _, sm := range []sortMode{sortPressure, sortFill, sortName, sortLast} {
		m := &agentsModel{sort: sm}
		g := agentGroup{rows: []Agent{
			{AgentName: "zeta", PressureLevel: "critical", ContextFillPct: 0.99},
			{AgentName: "", PressureLevel: "ok", ContextFillPct: 0.01},
			{AgentName: "alpha", PressureLevel: "ok", ContextFillPct: 0.5},
		}}
		m.sortGroupMembers(&g)
		if g.rows[0].AgentName != "" {
			t.Errorf("sort=%v: rows[0] = %q, want lead (\"\") pinned first (full order: %v)",
				sm, g.rows[0].AgentName, agentNames(g.rows))
		}
	}
}

func TestPinLeadsFirstSortsLeadsBySessionID(t *testing.T) {
	// Multiple lead rows must not reorder relative to each other every poll
	// just because the API returned them differently — SessionID gives a
	// deterministic order among them.
	rows := []Agent{
		{AgentName: "teammate"},
		{AgentName: "", SessionID: "zzzz-session"},
		{AgentName: "", SessionID: "aaaa-session"},
		{AgentName: "", SessionID: "mmmm-session"},
	}
	pinLeadsFirst(rows)
	if rows[0].AgentName != "" || rows[1].AgentName != "" || rows[2].AgentName != "" {
		t.Fatalf("expected the 3 lead rows first, got %v", agentNames(rows))
	}
	got := []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID}
	want := []string{"aaaa-session", "mmmm-session", "zzzz-session"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("lead rows by session = %v, want %v", got, want)
			break
		}
	}
	if rows[3].AgentName != "teammate" {
		t.Errorf("rows[3] = %q, want teammate last", rows[3].AgentName)
	}
}

func TestGroupBySessionPartitionsBySessionID(t *testing.T) {
	rows := []Agent{
		{AgentName: "", SessionID: "s1"},
		{AgentName: "@a", SessionID: "s1"},
		{AgentName: "", SessionID: "s2"},
	}
	groups := groupBySession(rows)
	if len(groups) != 2 {
		t.Fatalf("groupBySession = %d groups, want 2", len(groups))
	}
	byID := map[string]int{}
	for _, g := range groups {
		byID[g.sessionID] = len(g.rows)
	}
	if byID["s1"] != 2 || byID["s2"] != 1 {
		t.Errorf("group sizes = %v, want s1:2 s2:1", byID)
	}
}

func TestFilterGroupsByMaxAgeDropsWholeStaleGroup(t *testing.T) {
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	m.recordActivity(render.Record{Session: "fresh-sess", AgentName: "@a", Tag: "ACT", Display: "just now"})
	m.lastActions[activityKey("stale-sess", "@b")] = lastAction{tag: "ACT", display: "long ago", ts: time.Now().Add(-2 * time.Hour)}

	groups := []agentGroup{
		{sessionID: "fresh-sess", rows: []Agent{{AgentName: "@a", SessionID: "fresh-sess"}}},
		{sessionID: "stale-sess", rows: []Agent{{AgentName: "@b", SessionID: "stale-sess"}}},
	}
	out := m.filterGroupsByMaxAge(groups)
	if len(out) != 1 || out[0].sessionID != "fresh-sess" {
		t.Errorf("filterGroupsByMaxAge(1h) kept %d groups, want only fresh-sess", len(out))
	}
}

func TestFilterGroupsByMaxAgeKeepsGroupIfAnyMemberFresh(t *testing.T) {
	// A group with a stale lead and stale teammates is dropped as a whole
	// unit — but one fresh member (lead or teammate) keeps the whole group,
	// rather than each row being independently judged.
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	m.lastActions[activityKey("sess-1", "")] = lastAction{tag: "ACT", display: "old", ts: time.Now().Add(-3 * time.Hour)}
	m.lastActions[activityKey("sess-1", "@teammate")] = lastAction{tag: "ACT", display: "fresh", ts: time.Now()}

	groups := []agentGroup{
		{sessionID: "sess-1", rows: []Agent{
			{AgentName: "", SessionID: "sess-1"},
			{AgentName: "@teammate", SessionID: "sess-1"},
		}},
	}
	out := m.filterGroupsByMaxAge(groups)
	if len(out) != 1 {
		t.Error("group with one fresh member should survive as a whole unit even if the lead is stale")
	}
}

func TestFilterGroupsByMaxAgeDisabledAtZero(t *testing.T) {
	m := &agentsModel{maxAge: 0}
	groups := []agentGroup{
		{sessionID: "s1", rows: []Agent{{AgentName: "@a"}}},
		{sessionID: "s2", rows: []Agent{{AgentName: "@b"}}},
	}
	out := m.filterGroupsByMaxAge(groups)
	if len(out) != 2 {
		t.Errorf("filterGroupsByMaxAge(0) kept %d groups, want all 2 (unfiltered)", len(out))
	}
}

func TestFilterGroupsByMaxAgeKeepsZeroRecencyGroupWithLiveMember(t *testing.T) {
	// No SSE record and no API last_activity_ts, but a live/idle member — a
	// brand-new agent that hasn't posted its first activity yet, not
	// evidence of staleness.
	m := &agentsModel{maxAge: time.Hour}
	groups := []agentGroup{{sessionID: "s1", rows: []Agent{{AgentName: "@unknown", Liveness: "live"}}}}
	if out := m.filterGroupsByMaxAge(groups); len(out) != 1 {
		t.Error("filterGroupsByMaxAge should keep a zero-recency group that has a live member")
	}
}

func TestFilterGroupsByMaxAgeDropsZeroRecencyGroupWithNoLiveMembers(t *testing.T) {
	// Zero recency AND no live/idle member is a dead session (closed/stale/
	// unbound) that simply never got a last_activity_ts — not a new agent.
	// Keeping these unconditionally is what made the grid oscillate: every
	// such group ties at zero recency, so their relative order was never
	// actually meaningful.
	m := &agentsModel{maxAge: time.Hour}
	groups := []agentGroup{{sessionID: "s1", rows: []Agent{{AgentName: "@unknown", Liveness: "closed"}}}}
	if out := m.filterGroupsByMaxAge(groups); len(out) != 0 {
		t.Error("filterGroupsByMaxAge should drop a zero-recency group with no live members")
	}
}

func TestFilterStaleClosedMembersDropsAncientClosedTeammate(t *testing.T) {
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	m.lastActions[activityKey("s1", "@Explore")] = lastAction{tag: "DONE", ts: time.Now().Add(-2 * time.Hour)}
	m.lastActions[activityKey("s1", "")] = lastAction{tag: "ACT", ts: time.Now()}

	groups := []agentGroup{{sessionID: "s1", rows: []Agent{
		{AgentName: "", SessionID: "s1", Liveness: "live"},
		{AgentName: "@Explore", SessionID: "s1", Liveness: "closed"},
	}}}
	out := m.filterStaleClosedMembers(groups)
	if len(out) != 1 || len(out[0].rows) != 1 {
		t.Fatalf("filterStaleClosedMembers = %+v, want the ancient closed teammate dropped, lead kept", out)
	}
	if out[0].rows[0].AgentName != "" {
		t.Errorf("survivor = %q, want the lead", out[0].rows[0].AgentName)
	}
}

func TestFilterStaleClosedMembersKeepsRecentlyClosedTeammate(t *testing.T) {
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	m.lastActions[activityKey("s1", "@ctop")] = lastAction{tag: "DONE", ts: time.Now().Add(-20 * time.Minute)}

	groups := []agentGroup{{sessionID: "s1", rows: []Agent{
		{AgentName: "", SessionID: "s1", Liveness: "live"},
		{AgentName: "@ctop", SessionID: "s1", Liveness: "closed"},
	}}}
	out := m.filterStaleClosedMembers(groups)
	if len(out) != 1 || len(out[0].rows) != 2 {
		t.Fatalf("filterStaleClosedMembers = %+v, want the recently-closed teammate kept (finished within the age window)", out)
	}
}

func TestFilterStaleClosedMembersDropsAncientStaleTeammate(t *testing.T) {
	// "stale" is as much noise as "closed" once it's outside the age
	// window — a teammate with no recent activity, just under a different
	// liveness label.
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	m.lastActions[activityKey("s1", "@b")] = lastAction{tag: "ACT", ts: time.Now().Add(-3 * time.Hour)}

	groups := []agentGroup{{sessionID: "s1", rows: []Agent{
		{AgentName: "", SessionID: "s1", Liveness: "live"},
		{AgentName: "@b", SessionID: "s1", Liveness: "stale"},
	}}}
	out := m.filterStaleClosedMembers(groups)
	if len(out) != 1 || len(out[0].rows) != 1 {
		t.Fatalf("filterStaleClosedMembers = %+v, want the ancient stale teammate dropped, lead kept", out)
	}
}

func TestFilterStaleClosedMembersNeverDropsLeadOrLiveMembers(t *testing.T) {
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	// Lead has no recorded activity at all (zero recency) and is itself
	// "closed" — must still survive, since AgentName == "" is exempt
	// regardless of liveness/staleness.
	groups := []agentGroup{{sessionID: "s1", rows: []Agent{
		{AgentName: "", SessionID: "s1", Liveness: "closed"},
		{AgentName: "@a", SessionID: "s1", Liveness: "live"},
	}}}
	out := m.filterStaleClosedMembers(groups)
	if len(out) != 1 || len(out[0].rows) != 2 {
		t.Fatalf("filterStaleClosedMembers = %+v, want both rows kept (lead exempt, teammate live)", out)
	}
}

func TestFilterStaleClosedMembersDisabledAtZeroMaxAge(t *testing.T) {
	m := &agentsModel{maxAge: 0}
	groups := []agentGroup{{sessionID: "s1", rows: []Agent{
		{AgentName: "@ancient", SessionID: "s1", Liveness: "closed"},
	}}}
	out := m.filterStaleClosedMembers(groups)
	if len(out) != 1 || len(out[0].rows) != 1 {
		t.Errorf("filterStaleClosedMembers(maxAge=0) should be a no-op, got %+v", out)
	}
}

func TestHasLiveMembers(t *testing.T) {
	tests := []struct {
		name string
		rows []Agent
		want bool
	}{
		{"live", []Agent{{Liveness: "live"}}, true},
		{"idle", []Agent{{Liveness: "idle"}}, true},
		{"closed", []Agent{{Liveness: "closed"}}, false},
		{"stale", []Agent{{Liveness: "stale"}}, false},
		{"unknown/empty", []Agent{{Liveness: ""}}, false},
		{"mixed - one live", []Agent{{Liveness: "closed"}, {Liveness: "live"}}, true},
	}
	for _, tt := range tests {
		if got := hasLiveMembers(agentGroup{rows: tt.rows}); got != tt.want {
			t.Errorf("hasLiveMembers(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSortGroupsByRecencyMostRecentFirst(t *testing.T) {
	m := &agentsModel{lastActions: make(map[string]lastAction)}
	m.lastActions[activityKey("old-sess", "")] = lastAction{ts: time.Now().Add(-2 * time.Hour)}
	m.lastActions[activityKey("new-sess", "")] = lastAction{ts: time.Now()}

	groups := []agentGroup{
		{sessionID: "old-sess", rows: []Agent{{AgentName: "", SessionID: "old-sess"}}},
		{sessionID: "new-sess", rows: []Agent{{AgentName: "", SessionID: "new-sess"}}},
	}
	m.sortGroupsByRecency(groups)
	if groups[0].sessionID != "new-sess" {
		t.Errorf("sortGroupsByRecency: groups[0] = %q, want new-sess first", groups[0].sessionID)
	}
}

func TestRecomputeRowsGroupsFiltersAndSorts(t *testing.T) {
	// End-to-end: setRows should group by session, drop the stale session
	// entirely, and put the active session first.
	m := &agentsModel{maxAge: time.Hour, lastActions: make(map[string]lastAction)}
	m.lastActions[activityKey("active", "")] = lastAction{ts: time.Now()}
	m.lastActions[activityKey("dead", "")] = lastAction{ts: time.Now().Add(-3 * time.Hour)}

	m.setRows([]Agent{
		{AgentName: "", SessionID: "dead"},
		{AgentName: "@x", SessionID: "dead"},
		{AgentName: "", SessionID: "active"},
		{AgentName: "@y", SessionID: "active"},
	})

	if len(m.groups) != 1 || m.groups[0].sessionID != "active" {
		t.Fatalf("recomputeRows groups = %+v, want only the active session", m.groups)
	}
	if len(m.rows) != 2 {
		t.Fatalf("recomputeRows rows = %v, want 2 (active session only)", agentNames(m.rows))
	}
	if m.rows[0].AgentName != "" {
		t.Errorf("rows[0] = %q, want the active session's lead first", m.rows[0].AgentName)
	}
}

func TestRenderRowDisambiguatesLeadsBySessionSuffix(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	rowA := Agent{AgentName: "", SessionID: "225102e768b6-restofuuid-a"}
	rowB := Agent{AgentName: "", SessionID: "9cb4c04fc95e-restofuuid-b"}
	outA := display.StripANSI(m.renderRow(rowA, false, cs, 16, nil, " ", false, 160))
	outB := display.StripANSI(m.renderRow(rowB, false, cs, 16, nil, " ", false, 160))
	if !strings.Contains(outA, "lead·225102e7") {
		t.Errorf("renderRow(rowA) = %q, want it to contain %q", outA, "lead·225102e7")
	}
	if !strings.Contains(outB, "lead·9cb4c04f") {
		t.Errorf("renderRow(rowB) = %q, want it to contain %q", outB, "lead·9cb4c04f")
	}
	if outA == outB {
		t.Error("two different-session lead rows rendered identically")
	}
}

func TestRenderRowLeadDropsSessionSuffixWhenTeamKnown(t *testing.T) {
	// TEAM column is gone — once team_name is known, the row's background
	// tint already carries that identity, so the "·<prefix>" disambiguator
	// is dropped to save space (unlike the no-team case, which still needs it
	// — see TestRenderRowDisambiguatesLeadsBySessionSuffix).
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "", TeamName: "wms-build", SessionID: "225102e768b6-restofuuid"}
	out := display.StripANSI(m.renderRow(row, false, cs, 16, nil, " ", false, 160))
	if !strings.Contains(out, "lead") {
		t.Errorf("renderRow(team known) = %q, want it to still contain \"lead\"", out)
	}
	if strings.Contains(out, "225102e7") {
		t.Errorf("renderRow(team known) = %q, want no session-prefix suffix (tint already carries identity)", out)
	}
}

func TestRenderRowNoLongerHasPressColumn(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "@a", PressureLevel: "critical"}
	out := display.StripANSI(m.renderRow(row, false, cs, 16, nil, " ", false, 160))
	if strings.Contains(out, "CRIT") {
		t.Errorf("renderRow = %q, want no PRESS badge (dropped column) even for a critical row", out)
	}
}

func TestRenderRowSplitsInOutTokenColumns(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "@a", TokensInTotal: 1_200_000, TokensOutTotal: 38_000}
	out := display.StripANSI(m.renderRow(row, false, cs, 16, nil, " ", false, 160))
	if !strings.Contains(out, "1.2M") || !strings.Contains(out, "38k") {
		t.Errorf("renderRow = %q, want it to contain separate IN (1.2M) and OUT (38k) values", out)
	}
}

func TestRenderRowHeaderAliasNeverGetsAtPrefix(t *testing.T) {
	// sessionAlias's focus fallback is arbitrary free text — no leading "#"
	// or "·" to content-sniff — so isHeader is the only thing that can
	// correctly suppress the "@" a plain teammate name would get.
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "agent-health"}
	out := display.StripANSI(m.renderRow(row, false, cs, 16, nil, " ", true, 160))
	if strings.Contains(out, "@agent-health") {
		t.Errorf("renderRow(isHeader=true) = %q, want no \"@\" prefix on a header alias", out)
	}
	if !strings.Contains(out, "agent-health") {
		t.Errorf("renderRow(isHeader=true) = %q, want the alias text itself present", out)
	}

	out2 := display.StripANSI(m.renderRow(row, false, cs, 16, nil, " ", false, 160))
	if !strings.Contains(out2, "@agent-health") {
		t.Errorf("renderRow(isHeader=false) = %q, want a real agent name to still get the \"@\" prefix", out2)
	}
}

func TestCycleAgePresetSequence(t *testing.T) {
	m := &agentsModel{}
	want := []string{"6h", "all", "1h"}
	for _, w := range want {
		m.cycleAgePreset()
		if got := formatAge(m.maxAge); got != w {
			t.Errorf("cycleAgePreset() = %q, want %q", got, w)
		}
	}
}

func TestCycleAgePresetUsesConfiguredPresets(t *testing.T) {
	m := &agentsModel{agePresets: []time.Duration{30 * time.Minute, 2 * time.Hour, 0}}
	want := []string{"2h", "all", "30m"}
	for _, w := range want {
		m.cycleAgePreset()
		if got := formatAge(m.maxAge); got != w {
			t.Errorf("cycleAgePreset() with configured presets = %q, want %q", got, w)
		}
	}
}

func TestParseAgePresets(t *testing.T) {
	got, err := parseAgePresets("30m,2h,12h,0")
	if err != nil {
		t.Fatalf("parseAgePresets() error = %v", err)
	}
	want := []time.Duration{30 * time.Minute, 2 * time.Hour, 12 * time.Hour, 0}
	if len(got) != len(want) {
		t.Fatalf("parseAgePresets() = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("parseAgePresets()[%d] = %v, want %v", i, got[i], w)
		}
	}
}

// TestResolveInactiveAfter mirrors the --age-presets precedence tests
// (TestParseAgePresets/TestParseAgePresetsRejectsGarbage): flag wins if
// set, else env, else the compiled default; an invalid duration string is
// an error regardless of which source it came from.
func TestResolveInactiveAfter(t *testing.T) {
	def := 10 * time.Minute
	if got, err := resolveInactiveAfter("", "", def); err != nil || got != def {
		t.Errorf("resolveInactiveAfter(\"\", \"\", def) = (%v, %v), want (%v, nil)", got, err, def)
	}
	if got, err := resolveInactiveAfter("", "5m", def); err != nil || got != 5*time.Minute {
		t.Errorf("resolveInactiveAfter(\"\", \"5m\", def) = (%v, %v), want (5m, nil)", got, err)
	}
	if got, err := resolveInactiveAfter("15m", "5m", def); err != nil || got != 15*time.Minute {
		t.Errorf("resolveInactiveAfter(\"15m\", \"5m\", def) = (%v, %v), want (15m, nil) — flag wins over env", got, err)
	}
	if _, err := resolveInactiveAfter("not-a-duration", "", def); err == nil {
		t.Error("resolveInactiveAfter(garbage flag) should return an error")
	}
	if _, err := resolveInactiveAfter("", "not-a-duration", def); err == nil {
		t.Error("resolveInactiveAfter(garbage env) should return an error")
	}
}

func TestParseAgePresetsRejectsGarbage(t *testing.T) {
	if _, err := parseAgePresets("1h,not-a-duration"); err == nil {
		t.Error("parseAgePresets(garbage) should return an error")
	}
	if _, err := parseAgePresets(""); err == nil {
		t.Error("parseAgePresets(\"\") should return an error (no presets found)")
	}
}

func TestResolveAgePresetsMatchesExistingIndex(t *testing.T) {
	presets := []time.Duration{time.Hour, 6 * time.Hour, 0}
	got, idx := resolveAgePresets(presets, 6*time.Hour)
	if idx != 1 {
		t.Errorf("resolveAgePresets() idx = %d, want 1 (6h matches presets[1])", idx)
	}
	if len(got) != len(presets) {
		t.Errorf("resolveAgePresets() should not modify presets when maxAge already matches, got %v", got)
	}
}

func TestResolveAgePresetsPrependsUnmatchedMaxAge(t *testing.T) {
	presets := []time.Duration{time.Hour, 6 * time.Hour, 0}
	got, idx := resolveAgePresets(presets, 15*time.Minute)
	if idx != 0 {
		t.Errorf("resolveAgePresets() idx = %d, want 0 (prepended)", idx)
	}
	want := []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 0}
	if len(got) != len(want) {
		t.Fatalf("resolveAgePresets() = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("resolveAgePresets()[%d] = %v, want %v", i, got[i], w)
		}
	}
}

func TestCycleSortWrapsThroughAllFour(t *testing.T) {
	m := &agentsModel{}
	seen := map[sortMode]bool{m.sort: true}
	for i := 0; i < 3; i++ {
		m.cycleSort()
		seen[m.sort] = true
	}
	if len(seen) != 4 {
		t.Errorf("cycleSort did not visit all 4 modes in 3 steps: %v", seen)
	}
	m.cycleSort() // 4th cycle should return to start
	if m.sort != sortPressure {
		t.Errorf("after 4 cycles, sort = %v, want sortPressure (full wraparound)", m.sort)
	}
}

// TestSetRowsSelectionSurvivesReorder is the core regression for design
// §2.3's "Selection is keyed by roster_id ... so it survives refresh
// reordering": a poll that returns the same agents in a different order must
// not silently move the cursor/selection to an unrelated agent.
func TestSetRowsSelectionSurvivesReorder(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "a", SessionID: "s1", RosterID: strPtr("r1")},
		{AgentName: "b", SessionID: "s2", RosterID: strPtr("r2")},
	})
	m.cursor = 1 // select "b" (r2)
	sel := m.select_()
	if sel != "r2" {
		t.Fatalf("select_() = %q, want r2", sel)
	}

	// Refresh arrives with the same two agents in the opposite order.
	m.setRows([]Agent{
		{AgentName: "b", SessionID: "s2", RosterID: strPtr("r2")},
		{AgentName: "a", SessionID: "s1", RosterID: strPtr("r1")},
	})

	row, ok := m.current()
	if !ok || row.AgentName != "b" {
		t.Errorf("after reorder, cursor row = %+v (ok=%v), want agent b (selection should follow roster_id r2)", row, ok)
	}
}

// TestCursorSurvivesRefreshWithoutCommittedSelection is the regression for a
// bug found in live testing (session-explorer smoke test): moving the
// cursor with j/k alone — without pressing enter to commit a Detail
// selection — must still survive the next poll's setRows(), or the
// highlighted row silently snaps back to index 0 every refresh interval.
func TestCursorSurvivesRefreshWithoutCommittedSelection(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "a", SessionID: "s1", RosterID: strPtr("r1")},
		{AgentName: "b", SessionID: "s2", RosterID: strPtr("r2")},
	})
	m.moveDown() // cursor -> "b", no select_() call

	// A poll refresh arrives with the same two agents reordered.
	m.setRows([]Agent{
		{AgentName: "b", SessionID: "s2", RosterID: strPtr("r2")},
		{AgentName: "a", SessionID: "s1", RosterID: strPtr("r1")},
	})

	row, ok := m.current()
	if !ok || row.AgentName != "b" {
		t.Errorf("after reorder, cursor row = %+v (ok=%v), want agent b (cursor should track cursorKey even without a committed selection)", row, ok)
	}
}

func TestSetRowsFallsBackToSessionAgentKeyWhenNoRosterID(t *testing.T) {
	a := Agent{AgentName: "x", SessionID: "sess-1"}
	if got := a.SelectionKey(); got != "sess-1|x" {
		t.Errorf("SelectionKey() with nil RosterID = %q, want %q", got, "sess-1|x")
	}
}

func TestColumnsForWidthLadder(t *testing.T) {
	tests := []struct {
		width     int
		multiHost bool
		want      colSet
	}{
		{120, true, colSet{host: true, model: true, tokens: true, cost: true, toolCalls: true, last: true, activity: true}},
		{89, true, colSet{host: true, model: true, tokens: false, cost: false, toolCalls: false, last: true, activity: true}},
		{79, true, colSet{host: true, model: false, tokens: false, cost: false, toolCalls: false, last: true, activity: true}},
		{69, true, colSet{host: false, model: false, tokens: false, cost: false, toolCalls: false, last: true, activity: true}},
		{54, true, colSet{host: false, model: false, tokens: false, cost: false, toolCalls: false, last: false, activity: false}},
	}
	for _, tt := range tests {
		got := columnsForWidth(tt.width, tt.multiHost)
		if got != tt.want {
			t.Errorf("columnsForWidth(%d, multiHost=%v) = %+v, want %+v", tt.width, tt.multiHost, got, tt.want)
		}
	}
}

func TestColumnsForWidthHostGatedByMultiHost(t *testing.T) {
	if got := columnsForWidth(120, false); got.host {
		t.Error("columnsForWidth(120, multiHost=false).host = true, want false (a single-host fleet hides HOST regardless of width)")
	}
	if got := columnsForWidth(120, true); !got.host {
		t.Error("columnsForWidth(120, multiHost=true).host = false, want true")
	}
}

func TestMultipleHostsPresent(t *testing.T) {
	if multipleHostsPresent(nil) {
		t.Error("multipleHostsPresent(nil) = true, want false")
	}
	if multipleHostsPresent([]Agent{{Host: "a"}, {Host: "a"}}) {
		t.Error("multipleHostsPresent(single host) = true, want false")
	}
	if !multipleHostsPresent([]Agent{{Host: "a"}, {Host: "b"}}) {
		t.Error("multipleHostsPresent(two hosts) = false, want true")
	}
	if multipleHostsPresent([]Agent{{Host: ""}, {Host: ""}}) {
		t.Error("multipleHostsPresent(all empty) = true, want false")
	}
}

func TestAgentColWidthCapAndMinimum(t *testing.T) {
	m := &agentsModel{}
	if w := m.agentColWidth(); w != 8 {
		t.Errorf("agentColWidth() with no rows = %d, want 8 (minimum)", w)
	}
	m.rows = []Agent{{AgentName: "this-is-a-very-long-agent-name"}}
	if w := m.agentColWidth(); w != 12 {
		t.Errorf("agentColWidth() with a long name = %d, want 12 (cap)", w)
	}
}

func agentNames(rows []Agent) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.AgentName
	}
	return out
}

func TestActivityCellForDistinguishesLeadsBySession(t *testing.T) {
	m := &agentsModel{}
	m.recordActivity(render.Record{Session: "aaaaaaaaaaaa", AgentName: "", Tag: "THNK", Display: "session A thinking"})
	m.recordActivity(render.Record{Session: "bbbbbbbbbbbb", AgentName: "", Tag: "THNK", Display: "session B thinking"})

	rowA := Agent{SessionID: "aaaaaaaaaaaa-full-uuid-suffix", AgentName: ""}
	rowB := Agent{SessionID: "bbbbbbbbbbbb-full-uuid-suffix", AgentName: ""}

	cellA := display.StripANSI(m.activityCellFor(rowA))
	cellB := display.StripANSI(m.activityCellFor(rowB))
	if !strings.Contains(cellA, "session A thinking") {
		t.Errorf("activityCellFor(rowA) = %q, want it to contain %q", cellA, "session A thinking")
	}
	if !strings.Contains(cellB, "session B thinking") {
		t.Errorf("activityCellFor(rowB) = %q, want it to contain %q", cellB, "session B thinking")
	}
}

func TestActivityCellForShowsProcessingGlyphInsteadOfTagBracket(t *testing.T) {
	m := &agentsModel{}
	m.recordActivity(render.Record{Session: "sess1", AgentName: "@a", Tag: "EDIT", Display: "Editing Tonka.md"})
	row := Agent{SessionID: "sess1", AgentName: "@a"}
	cell := display.StripANSI(m.activityCellFor(row))
	if strings.Contains(cell, "[EDIT]") {
		t.Errorf("activityCellFor = %q, want no [TAG] bracket (replaced by a state glyph)", cell)
	}
	if !strings.HasPrefix(cell, "◉") && !strings.HasPrefix(cell, "○") {
		t.Errorf("activityCellFor = %q, want it to start with a processing glyph (◉ or ○ — event is fresh)", cell)
	}
	if !strings.Contains(cell, "Editing Tonka.md") {
		t.Errorf("activityCellFor = %q, want it to contain the display text", cell)
	}
}

// TestActivityCellForFallsBackToAPIFieldWithoutSSEEvent covers the
// no-known-tagged-event path: "processing" isn't reachable there (isMidTurn
// needs a lastAction), so a live row with no SSE record falls back to the
// steady idle glyph, and a closed row falls back to the hollow closed
// glyph — never a flashing mid-turn glyph like before this redesign.
func TestActivityCellForFallsBackToAPIFieldWithoutSSEEvent(t *testing.T) {
	m := &agentsModel{}
	liveRow := Agent{SessionID: "sess1", AgentName: "@a", LastActivityDisplay: "Reading foo.go", Liveness: "live"}
	got := display.StripANSI(m.activityCellFor(liveRow))
	if !strings.Contains(got, "Reading foo.go") {
		t.Errorf("activityCellFor (no SSE tag known) = %q, want it to contain %q", got, "Reading foo.go")
	}
	if !strings.HasPrefix(got, "◉") {
		t.Errorf("activityCellFor (no SSE tag known, live) = %q, want the steady idle glyph ◉ (processing isn't reachable without a known tagged event)", got)
	}
	full := m.activityCellFor(liveRow)
	green := display.RGB(successRGB[0], successRGB[1], successRGB[2])
	if !strings.Contains(full, green) {
		t.Errorf("activityCellFor (no SSE tag known, live) = %q, want the steady idle glyph colored green (matches the feed's [DONE])", full)
	}

	closedRow := Agent{SessionID: "sess2", AgentName: "@b", LastActivityDisplay: "Reading bar.go", Liveness: "closed"}
	if got := display.StripANSI(m.activityCellFor(closedRow)); !strings.HasPrefix(got, "○") {
		t.Errorf("activityCellFor (no SSE tag known, closed) = %q, want it to start with the hollow closed glyph ○", got)
	}
}

// TestActivityCellForFallbackUsesTagColorWhenKnown covers the architecture
// gap this fix closes: an idle agent whose SSE event was evicted from
// lastActions must still color its activity text by last_activity_tag
// (persisted in the gauge/health API), not fall back to dim/default.
func TestActivityCellForFallbackUsesTagColorWhenKnown(t *testing.T) {
	m := &agentsModel{}
	row := Agent{SessionID: "sess1", AgentName: "@a", LastActivityDisplay: "editing foo.go", LastActivityTag: "EDIT", Liveness: "live"}
	full := m.activityCellFor(row)
	stripped := display.StripANSI(full)
	if !strings.Contains(stripped, "editing foo.go") {
		t.Errorf("activityCellFor (fallback, tag known) = %q, want it to contain %q", stripped, "editing foo.go")
	}
	tc := display.TagColor("EDIT")
	tagColor := display.RGB(tc[0], tc[1], tc[2])
	if !strings.Contains(full, tagColor) {
		t.Errorf("activityCellFor (fallback, tag known) = %q, want the EDIT tag color %q in the text", full, tagColor)
	}
}

// TestActivityCellForFallsBackToInactiveWhenStale covers the fourth state:
// an alive row with no SSE record whose last known activity (from the
// health API's last_activity_ts) is older than --inactive-after reads as
// inactive — steady dim, not idle-green.
func TestActivityCellForFallsBackToInactiveWhenStale(t *testing.T) {
	m := &agentsModel{inactiveAfter: 10 * time.Minute}
	oldTs := time.Now().Add(-time.Hour).Format(time.RFC3339)
	row := Agent{SessionID: "sess3", AgentName: "@c", LastActivityDisplay: "old stuff", LastActivityTs: &oldTs, Liveness: "live"}
	stripped := display.StripANSI(m.activityCellFor(row))
	if !strings.HasPrefix(stripped, "◉") {
		t.Errorf("activityCellFor (inactive) = %q, want the steady filled glyph ◉", stripped)
	}
	full := m.activityCellFor(row)
	dim := display.RGB(dimGreyRGB[0], dimGreyRGB[1], dimGreyRGB[2])
	if !strings.Contains(full, dim) {
		t.Errorf("activityCellFor (inactive) = %q, want dim grey color %q, not idle green", full, dim)
	}
}

func TestIsMidTurn(t *testing.T) {
	now := time.Now()
	if !isMidTurn(lastAction{tag: "EDIT", ts: now.Add(-5 * time.Second)}, now) {
		t.Error("a 5s-old non-terminal tag should read as mid-turn")
	}
	if isMidTurn(lastAction{tag: "EDIT", ts: now.Add(-15 * time.Second)}, now) {
		t.Error("a 15s-old event should read as idle, not mid-turn")
	}
	if isMidTurn(lastAction{tag: "DONE", ts: now}, now) {
		t.Error("a fresh DONE tag should still read as idle — it closes a turn")
	}
	if isMidTurn(lastAction{tag: "COMP", ts: now}, now) {
		t.Error("a fresh COMP tag should still read as idle — it closes a turn")
	}
}

func TestActivityGlyphIdleAndInactiveAreSteadyWithNoAnimation(t *testing.T) {
	rgb := [3]int{10, 20, 30}
	even := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	odd := time.Date(2024, 1, 1, 0, 0, 11, 0, time.UTC)
	for _, state := range []activityState{activityIdle, activityInactive} {
		evenGlyph := activityGlyph(state, rgb, even)
		oddGlyph := activityGlyph(state, rgb, odd)
		if evenGlyph != oddGlyph {
			t.Errorf("activityGlyph(%v) should be identical across seconds (no animation), got %q vs %q", state, evenGlyph, oddGlyph)
		}
		if got := display.StripANSI(evenGlyph); !strings.HasPrefix(got, "◉") {
			t.Errorf("activityGlyph(%v) = %q, want the steady filled glyph ◉", state, got)
		}
	}
}

func TestActivityGlyphClosedIsHollowAndSteady(t *testing.T) {
	rgb := dimGreyRGB
	even := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	odd := time.Date(2024, 1, 1, 0, 0, 11, 0, time.UTC)
	evenGlyph := display.StripANSI(activityGlyph(activityClosed, rgb, even))
	oddGlyph := display.StripANSI(activityGlyph(activityClosed, rgb, odd))
	if evenGlyph != oddGlyph {
		t.Errorf("closed activityGlyph should be identical across seconds (no animation), got %q vs %q", evenGlyph, oddGlyph)
	}
	if !strings.HasPrefix(evenGlyph, "○") {
		t.Errorf("activityGlyph(closed) = %q, want it to start with the hollow glyph ○", evenGlyph)
	}
}

// TestActivityGlyphProcessingCyclesFilledAndHollow is the regression for
// the ACTIVITY column sharing the STATUS column's ◉/○ filled/hollow pair
// (see activityGlyph's doc comment).
func TestActivityGlyphProcessingCyclesFilledAndHollow(t *testing.T) {
	rgb := [3]int{10, 20, 30}
	even := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	odd := time.Date(2024, 1, 1, 0, 0, 11, 0, time.UTC)

	if got := display.StripANSI(activityGlyph(activityProcessing, rgb, even)); !strings.HasPrefix(got, "◉") {
		t.Errorf("activityGlyph(processing, even second) = %q, want it to start with the filled glyph ◉", got)
	}
	if got := display.StripANSI(activityGlyph(activityProcessing, rgb, odd)); !strings.HasPrefix(got, "○") {
		t.Errorf("activityGlyph(processing, odd second) = %q, want it to start with the hollow glyph ○", got)
	}
}

func TestActivityGlyphProcessingColorDoesNotChangeAcrossTheCycle(t *testing.T) {
	// The blink is a glyph-shape change (◉ <-> ○), not a brightness change —
	// both frames must render in the exact same tag color.
	rgb := [3]int{200, 100, 50}
	even := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	odd := time.Date(2024, 1, 1, 0, 0, 11, 0, time.UTC)
	color := display.RGB(rgb[0], rgb[1], rgb[2])
	if got := activityGlyph(activityProcessing, rgb, even); !strings.Contains(got, color) {
		t.Errorf("even-second processing glyph = %q, want it to contain the full tag color %q", got, color)
	}
	if got := activityGlyph(activityProcessing, rgb, odd); !strings.Contains(got, color) {
		t.Errorf("odd-second processing glyph = %q, want the SAME full tag color %q (no dimming)", got, color)
	}
}

func TestActivityStateForPriority(t *testing.T) {
	if got := activityStateFor(false, true, false); got != activityClosed {
		t.Errorf("activityStateFor(midTurn=false,closed=true,inactive=false) = %v, want activityClosed (closed wins)", got)
	}
	if got := activityStateFor(true, true, false); got != activityClosed {
		t.Errorf("activityStateFor(midTurn=true,closed=true,inactive=false) = %v, want activityClosed (closed wins over midTurn)", got)
	}
	if got := activityStateFor(true, false, false); got != activityProcessing {
		t.Errorf("activityStateFor(midTurn=true,closed=false,inactive=false) = %v, want activityProcessing", got)
	}
	if got := activityStateFor(false, false, true); got != activityInactive {
		t.Errorf("activityStateFor(midTurn=false,closed=false,inactive=true) = %v, want activityInactive", got)
	}
	if got := activityStateFor(false, false, false); got != activityIdle {
		t.Errorf("activityStateFor(midTurn=false,closed=false,inactive=false) = %v, want activityIdle", got)
	}
}

func TestActivityStateColor(t *testing.T) {
	tag := [3]int{1, 2, 3}
	if got := activityStateColor(activityProcessing, tag); got != tag {
		t.Errorf("activityStateColor(processing) = %v, want the tag color %v", got, tag)
	}
	if got := activityStateColor(activityIdle, tag); got != successRGB {
		t.Errorf("activityStateColor(idle) = %v, want successRGB %v", got, successRGB)
	}
	if got := activityStateColor(activityInactive, tag); got != dimGreyRGB {
		t.Errorf("activityStateColor(inactive) = %v, want dimGreyRGB %v", got, dimGreyRGB)
	}
	if got := activityStateColor(activityClosed, tag); got != dimGreyRGB {
		t.Errorf("activityStateColor(closed) = %v, want dimGreyRGB %v", got, dimGreyRGB)
	}
}

func TestActivityKeyTruncatesSessionIDToTwelveChars(t *testing.T) {
	// render.Record.Session is always <=12 chars (server-truncated before
	// the SSE stream); Agent.SessionID is the full UUID. activityKey must
	// truncate the full UUID down to the same 12-char prefix the SSE side
	// already uses, or the tracker and the grid never match up.
	full := "225102e7-68b6-4d74-bb80-91afddb2faaa"
	prefix := full[:12]
	if got := activityKey(full, "@x"); got != prefix+"|@x" {
		t.Errorf("activityKey(full UUID, ...) = %q, want %q", got, prefix+"|@x")
	}
	if got := activityKey(prefix, "@x"); got != prefix+"|@x" {
		t.Errorf("activityKey(already-12-chars, ...) = %q, want %q (no-op truncation)", got, prefix+"|@x")
	}
}

func TestSanitizeActivity(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"line one\nline two", "line one line two"},
		{"line one\r\nline two", "line one line two"},
		{"**Team names empty**: Th...", "Team names empty: Th..."},
		{"a   b    c", "a b c"},
		{"  leading and trailing  \n", "leading and trailing"},
	}
	for _, tt := range tests {
		if got := sanitizeActivity(tt.in); got != tt.want {
			t.Errorf("sanitizeActivity(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAgentCountLabelSingularPlural(t *testing.T) {
	if got := agentCountLabel(1); got != "1 agent" {
		t.Errorf("agentCountLabel(1) = %q, want %q", got, "1 agent")
	}
	if got := agentCountLabel(4); got != "4 agents" {
		t.Errorf("agentCountLabel(4) = %q, want %q", got, "4 agents")
	}
	if got := agentCountLabel(0); got != "0 agents" {
		t.Errorf("agentCountLabel(0) = %q, want %q", got, "0 agents")
	}
}

func TestVisibleAgentCountsUsesVisRowsNotFlattenedRows(t *testing.T) {
	// A collapsed multi-agent session must count as ONE visible row (the
	// header), not one per hidden teammate — the whole point of the fix
	// (status bar previously read "8 agents (0 live · 1 idle)" by counting
	// every teammate whether visible or not).
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "", SessionID: "s1", Liveness: "idle"},
		{AgentName: "@a", SessionID: "s1", Liveness: "closed"},
		{AgentName: "@b", SessionID: "s1", Liveness: "closed"},
		{AgentName: "", SessionID: "s2", Liveness: "stale"},
	})
	visible, active, closed := m.visibleAgentCounts()
	if visible != 2 {
		t.Errorf("visible = %d, want 2 (one row per collapsed session, not 4 flattened agents)", visible)
	}
	if active != 1 {
		t.Errorf("active = %d, want 1 (s1's idle lead)", active)
	}
	if closed != 1 {
		t.Errorf("closed = %d, want 1 (s2's stale lead)", closed)
	}
}

func TestVisibleAgentCountsExpandedSessionCountsEachRow(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "", SessionID: "s1", Liveness: "live"},
		{AgentName: "@a", SessionID: "s1", Liveness: "closed"},
	})
	m.toggleExpand()
	visible, active, closed := m.visibleAgentCounts()
	if visible != 2 {
		t.Errorf("visible = %d, want 2 (header + expanded member)", visible)
	}
	if active != 1 || closed != 1 {
		t.Errorf("active=%d closed=%d, want active=1 (live lead) closed=1 (closed teammate)", active, closed)
	}
}

// TestRenderTotalRowSumsAllRows is the successor to the old per-group
// SUBTOTAL test — group subtotals are gone (a collapsed session row IS the
// summary now), but the grand TOTAL footer still sums every loaded row.
func TestRenderTotalRowSumsAllRows(t *testing.T) {
	m := &agentsModel{rows: []Agent{
		{AgentName: "", SessionCostUSD: 1, TokensInTotal: 100, TokensOutTotal: 10},
		{AgentName: "@a", SessionCostUSD: 2, TokensInTotal: 200, TokensOutTotal: 20},
	}}
	cs := columnsForWidth(160, false)
	out := display.StripANSI(m.renderTotalRow(cs, 16, 160))
	if !strings.Contains(out, "TOTAL") {
		t.Errorf("renderTotalRow = %q, want it to contain TOTAL", out)
	}
	if !strings.Contains(out, "$3") {
		t.Errorf("renderTotalRow = %q, want cost sum $3 (1+2)", out)
	}
	if !strings.Contains(out, "2 agents") {
		t.Errorf("renderTotalRow = %q, want \"2 agents\"", out)
	}
}

func TestRenderTotalRowShowsMostRecentSessionFocus(t *testing.T) {
	m := &agentsModel{
		groups: []agentGroup{
			{sessionID: "s1", rows: []Agent{{AgentName: "", CurrentFocus: "fixing the thing"}}},
		},
		rows: []Agent{{AgentName: "", CurrentFocus: "fixing the thing"}},
	}
	cs := columnsForWidth(160, false)
	out := display.StripANSI(m.renderTotalRow(cs, 16, 160))
	if !strings.Contains(out, "fixing the thing") {
		t.Errorf("renderTotalRow = %q, want it to show the most-recent session's current_focus", out)
	}
	if !strings.Contains(out, "◎") {
		t.Errorf("renderTotalRow = %q, want the focus glyph ◎", out)
	}
}

func TestRenderTotalRowBlankWhenNoFocus(t *testing.T) {
	m := &agentsModel{
		groups: []agentGroup{{sessionID: "s1", rows: []Agent{{AgentName: ""}}}},
		rows:   []Agent{{AgentName: ""}},
	}
	cs := columnsForWidth(160, false)
	out := display.StripANSI(m.renderTotalRow(cs, 16, 160))
	if strings.Contains(out, "◎") {
		t.Errorf("renderTotalRow with no focus = %q, want no focus glyph", out)
	}
}

func TestBuildVisRowsCollapsedByDefault(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "", SessionID: "s1", SessionCostUSD: 5, TokensInTotal: 100, TokensOutTotal: 10},
		{AgentName: "@teammate", SessionID: "s1", TokensInTotal: 50, TokensOutTotal: 5},
	})
	if len(m.visRows) != 1 {
		t.Fatalf("visRows = %d, want 1 (a multi-agent session collapses to one row by default)", len(m.visRows))
	}
	vr := m.visRows[0]
	if !vr.isHeader || !vr.hasChildren || vr.expanded {
		t.Errorf("header visRow = %+v, want isHeader=true hasChildren=true expanded=false", vr)
	}
	if vr.tokensIn != 150 || vr.tokensOut != 15 {
		t.Errorf("header aggregate tokens = in:%d out:%d, want in:150 out:15 (summed across the group)", vr.tokensIn, vr.tokensOut)
	}
}

func TestToggleExpandRevealsChildrenAndCollapseHidesThem(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "", SessionID: "s1", RosterID: strPtr("lead1")},
		{AgentName: "@a", SessionID: "s1", RosterID: strPtr("a1")},
		{AgentName: "@b", SessionID: "s1", RosterID: strPtr("b1")},
	})
	if len(m.visRows) != 1 {
		t.Fatalf("collapsed visRows = %d, want 1", len(m.visRows))
	}

	m.toggleExpand()
	if len(m.visRows) != 3 {
		t.Fatalf("expanded visRows = %d, want 3 (header + 2 members)", len(m.visRows))
	}
	if !m.visRows[0].isHeader || !m.visRows[0].expanded {
		t.Errorf("visRows[0] = %+v, want an expanded header", m.visRows[0])
	}
	if m.visRows[1].isHeader || m.visRows[2].isHeader {
		t.Errorf("member rows must not be headers: %+v %+v", m.visRows[1], m.visRows[2])
	}

	m.toggleExpand()
	if len(m.visRows) != 1 {
		t.Fatalf("re-collapsed visRows = %d, want 1", len(m.visRows))
	}
	if m.cursor != 0 {
		t.Errorf("cursor after collapse = %d, want 0 (stays on the session header it was on)", m.cursor)
	}
}

func TestToggleExpandNoOpOnSoloSession(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{{AgentName: "", SessionID: "solo"}})
	m.toggleExpand()
	if len(m.visRows) != 1 || m.expanded["solo"] {
		t.Errorf("toggleExpand on a solo session should be a no-op (nothing to fold), expanded=%v visRows=%d", m.expanded["solo"], len(m.visRows))
	}
}

func TestToggleExpandNoOpOnMemberRow(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "", SessionID: "s1"},
		{AgentName: "@a", SessionID: "s1"},
	})
	m.toggleExpand() // expand: visRows = [header, member]
	m.moveDown()     // cursor -> member row
	m.toggleExpand() // member rows aren't foldable: no-op
	if len(m.visRows) != 2 {
		t.Errorf("toggleExpand on a member row should be a no-op, visRows=%d", len(m.visRows))
	}
}

func TestCursorNavigationCountsExpandedChildrenAsRows(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{
		{AgentName: "", SessionID: "s1", RosterID: strPtr("lead1")},
		{AgentName: "@a", SessionID: "s1", RosterID: strPtr("a1")},
		{AgentName: "", SessionID: "s2", RosterID: strPtr("lead2")},
	})
	if len(m.visRows) != 2 {
		t.Fatalf("visRows = %d, want 2 (both sessions collapsed)", len(m.visRows))
	}

	m.toggleExpand() // cursor starts on s1's header
	if len(m.visRows) != 3 {
		t.Fatalf("visRows after expanding s1 = %d, want 3 (s1 header + 1 member + s2 header)", len(m.visRows))
	}
	m.last()
	if m.cursor != 2 {
		t.Errorf("last() cursor = %d, want 2 (the last visible row across the expanded tree, i.e. s2's header)", m.cursor)
	}
}

func TestHeaderDisplayAgentAggregatesTokensNotCost(t *testing.T) {
	// SessionCostUSD is left untouched — the representative's own value
	// already IS the session-wide total (see agents.go's headerDisplayAgent
	// doc comment) — only tokens/tool-calls are genuinely per-agent and need
	// summing across the group.
	vr := visRow{
		agent:     Agent{AgentName: "", SessionCostUSD: 9.5, TokensInTotal: 1},
		isHeader:  true,
		tokensIn:  300,
		tokensOut: 40,
		toolCalls: 7,
	}
	got := headerDisplayAgent(vr)
	if got.TokensInTotal != 300 || got.TokensOutTotal != 40 || got.ToolCallsTotal != 7 {
		t.Errorf("headerDisplayAgent tokens/toolcalls = %+v, want the visRow's aggregates", got)
	}
	if got.SessionCostUSD != 9.5 {
		t.Errorf("headerDisplayAgent cost = %v, want the representative's own value unchanged (9.5)", got.SessionCostUSD)
	}
}

func TestHeaderDisplayAgentPrefersTeamName(t *testing.T) {
	vr := visRow{agent: Agent{AgentName: "", TeamName: "wms-build"}, isHeader: true}
	got := headerDisplayAgent(vr)
	if got.AgentName != "#wms-build" {
		t.Errorf("headerDisplayAgent.AgentName = %q, want %q", got.AgentName, "#wms-build")
	}
}

func TestHeaderDisplayAgentUsesSessionAliasWhenNoTeam(t *testing.T) {
	solo := visRow{agent: Agent{AgentName: "", SessionID: "225102e768b6-rest"}, isHeader: true, hasChildren: false}
	if got := headerDisplayAgent(solo).AgentName; got != "solo·225102e7" {
		t.Errorf("headerDisplayAgent(solo, no team).AgentName = %q, want %q", got, "solo·225102e7")
	}

	multi := visRow{agent: Agent{AgentName: "", SessionID: "225102e768b6-rest"}, isHeader: true, hasChildren: true}
	if got := headerDisplayAgent(multi).AgentName; got != "?·225102e7" {
		t.Errorf("headerDisplayAgent(multi-agent, no team).AgentName = %q, want %q", got, "?·225102e7")
	}
}

func TestHeaderDisplayAgentPrefersFocusOverSessionPrefixWhenNoTeam(t *testing.T) {
	// The health API's team_name is empty for plenty of live sessions today,
	// but CurrentFocus (the WMS focus string) usually isn't — prefer it over
	// the bare "?·<prefix>" fallback for a multi-agent session.
	vr := visRow{
		agent:    Agent{AgentName: "", SessionID: "225102e768b6-rest", CurrentFocus: "agent-health-diet"},
		isHeader: true, hasChildren: true,
	}
	if got := headerDisplayAgent(vr).AgentName; got != "agent-healt…" {
		t.Errorf("headerDisplayAgent(no team, focus set).AgentName = %q, want the truncated focus %q", got, "agent-healt…")
	}
}

func TestFoldIndicatorGlyphs(t *testing.T) {
	if got := display.StripANSI(foldIndicator(visRow{isHeader: true, hasChildren: false})); got != " " {
		t.Errorf("foldIndicator(solo header) = %q, want a blank cell", got)
	}
	if got := display.StripANSI(foldIndicator(visRow{isHeader: true, hasChildren: true, expanded: false})); got != "▸" {
		t.Errorf("foldIndicator(collapsed) = %q, want ▸", got)
	}
	if got := display.StripANSI(foldIndicator(visRow{isHeader: true, hasChildren: true, expanded: true})); got != "▾" {
		t.Errorf("foldIndicator(expanded) = %q, want ▾", got)
	}
	if got := display.StripANSI(foldIndicator(visRow{isHeader: false})); got != "·" {
		t.Errorf("foldIndicator(member row) = %q, want ·", got)
	}
}

func TestTeamTintRGBUsesTeamNameThenSessionIDFallback(t *testing.T) {
	withTeam := agentGroup{sessionID: "s1", rows: []Agent{{TeamName: "wms-build"}}}
	if got, want := teamTintRGB(withTeam), display.EntityColor("wms-build", ""); got != want {
		t.Errorf("teamTintRGB(with team) = %v, want EntityColor(team_name) %v", got, want)
	}

	noTeam := agentGroup{sessionID: "sess-xyz", rows: []Agent{{TeamName: ""}}}
	if got, want := teamTintRGB(noTeam), display.EntityColor("sess-xyz", ""); got != want {
		t.Errorf("teamTintRGB(no team) = %v, want EntityColor(sessionID) fallback %v", got, want)
	}
}

func TestBlendBGMovesTowardTintAtGivenOpacity(t *testing.T) {
	tint := [3]int{200, 100, 50}
	resting := blendBG(tint, 0.12)
	cursor := blendBG(tint, 0.25)

	for i := range resting {
		if cursor[i] <= resting[i] {
			t.Errorf("channel %d: cursor tint (%d) should be brighter than resting tint (%d)", i, cursor[i], resting[i])
		}
	}
	// Still anchored near the dark base, not replaced by the tint outright.
	if resting[0] < uint8(tintBaseRGB[0]) {
		t.Errorf("blendBG(0.12) red channel = %d, want it at least the dark base (%d)", resting[0], tintBaseRGB[0])
	}
}

func TestBlendBGClampsToByteRange(t *testing.T) {
	got := blendBG([3]int{255, 255, 255}, 1.0)
	for i, v := range got {
		if v > 255 {
			t.Errorf("channel %d = %d, want <=255", i, v)
		}
	}
}

func TestApplyRowTintNoOpWhenBgEmpty(t *testing.T) {
	s := "plain text"
	if got := applyRowTint(s, "", 20); got != s {
		t.Errorf("applyRowTint with bg=\"\" should be a no-op, got %q", got)
	}
}

func TestApplyRowTintPadsAndReinjectsAfterReset(t *testing.T) {
	const bg = "\x1b[48;2;20;20;20m"
	inner := display.RGB(1, 2, 3) + "hi" + display.RESET
	got := applyRowTint(inner, bg, 10)
	// padded to 10 visible cells, plus a final RESET so the tint can't leak
	// into whatever the terminal renders on the next line.
	want := bg + display.RGB(1, 2, 3) + "hi" + display.RESET + bg + "        " + display.RESET
	if got != want {
		t.Errorf("applyRowTint = %q, want %q", got, want)
	}
}

func TestApplyRowTintDoesNotLeakIntoNextLine(t *testing.T) {
	// Regression: a row whose last emitted code is the bg escape itself
	// (nothing after it re-establishes a foreground/reset) must not bleed
	// its color into whatever comes after it once lines are joined with
	// "\n" — SGR state persists across a bare newline in a real terminal.
	const bg = "\x1b[48;2;20;20;20m"
	got := applyRowTint("plain", bg, 5)
	if !strings.HasSuffix(got, display.RESET) {
		t.Errorf("applyRowTint = %q, want it to end with a RESET", got)
	}
}

func TestViewSkipsTeamTintWhenNotColorized(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{{AgentName: "", SessionID: "s1", TeamName: "wms-build"}})
	out := m.View(140, 10, true, nil, false)
	if strings.Contains(out, "48;2;") {
		t.Errorf("View(colorize=false) contains a truecolor bg escape, want none: %q", out)
	}
}

func TestViewAppliesTeamTintWhenColorized(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{{AgentName: "", SessionID: "s1", TeamName: "wms-build"}})
	out := m.View(140, 10, true, nil, true)
	if !strings.Contains(out, "48;2;") {
		t.Error("View(colorize=true) should apply a truecolor bg tint to at least one row")
	}
}

// TestRowDimLevel is the regression for the operator's "disable ALL
// dimming" direction: rowDimLevel used to derive dimHalve/dimFlat from
// liveness/inactive (see the §ctop AgentAF redesign's dimming ladder) —
// it now always returns dimNone, unconditionally, pending a broader UX
// pass. Every liveness/inactive combination must produce the same result.
func TestRowDimLevel(t *testing.T) {
	tests := []struct {
		liveness string
		inactive bool
	}{
		{"live", false},
		{"idle", false},
		{"live", true},
		{"idle", true},
		{"stale", false},
		{"stale", true},
		{"closed", false},
		{"closed", true},
		{"unbound", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := rowDimLevel(tt.liveness, tt.inactive); got != dimNone {
			t.Errorf("rowDimLevel(%q, inactive=%v) = %v, want dimNone (all dimming disabled)", tt.liveness, tt.inactive, got)
		}
	}
}

func TestHalveRGB(t *testing.T) {
	if got := halveRGB([3]int{100, 50, 11}); got != [3]int{50, 25, 5} {
		t.Errorf("halveRGB({100,50,11}) = %v, want {50,25,5}", got)
	}
	if got := halveRGB([3]int{0, 0, 0}); got != [3]int{0, 0, 0} {
		t.Errorf("halveRGB(black) = %v, want black", got)
	}
}

func TestRenderDimHalvesOnlyWhenDimHalve(t *testing.T) {
	rgb := [3]int{100, 50, 20}
	full := display.RGB(rgb[0], rgb[1], rgb[2])
	h := halveRGB(rgb)
	half := display.RGB(h[0], h[1], h[2])

	if got := renderDim(rgb, "x", dimNone); !strings.Contains(got, full) {
		t.Errorf("renderDim(dimNone) = %q, want the full-brightness color %q", got, full)
	}
	if got := renderDim(rgb, "x", dimHalve); !strings.Contains(got, half) {
		t.Errorf("renderDim(dimHalve) = %q, want the halved color %q", got, half)
	}
	if got := renderDim(rgb, "x", dimFlat); !strings.Contains(got, full) {
		t.Errorf("renderDim(dimFlat) = %q, want rgb rendered unchanged (flattening is a separate whole-row post-process in renderRow, not renderDim's job)", got)
	}
}

func TestIsInactive(t *testing.T) {
	m := &agentsModel{}
	oldTs := time.Now().Add(-time.Hour).Format(time.RFC3339)
	row := Agent{LastActivityTs: &oldTs}
	if m.isInactive(row) {
		t.Error("isInactive with inactiveAfter unset (zero value) should always be false")
	}

	m.inactiveAfter = 10 * time.Minute
	if !m.isInactive(row) {
		t.Error("isInactive with a 1h-old activity ts and 10m threshold should be true")
	}

	recentTs := time.Now().Add(-time.Minute).Format(time.RFC3339)
	fresh := Agent{LastActivityTs: &recentTs}
	if m.isInactive(fresh) {
		t.Error("isInactive with a 1m-old activity ts and 10m threshold should be false")
	}
}

// TestRenderRowDimmingClosedKeepsFullColor and its sibling below rely on
// display.RGB producing a raw ANSI escape unconditionally (unlike
// lipgloss.NewStyle().Render, which lipgloss silently no-ops outside a real
// TTY — see TestFillBarColorKeyedOnlyOnPressureLevel's comment on the same
// quirk) — so checking for the ENTITY color's raw escape substring is a
// TTY-independent way to prove the dimming post-process left it alone,
// regardless of whether the *dim* color itself renders in this test
// environment. Dimming is disabled entirely (rowDimLevel always dimNone,
// per the operator's "disable ALL dimming" direction) — a closed row no
// longer flattens to ColorDim.
func TestRenderRowDimmingClosedKeepsFullColor(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "@a", Liveness: "closed", SessionCostUSD: 1.23}
	ac := display.EntityColor("@a", "")
	entityColor := display.RGB(ac[0], ac[1], ac[2])
	out := m.renderRow(row, false, cs, 16, nil, " ", false, 160)
	if !strings.Contains(out, entityColor) {
		t.Errorf("renderRow(closed) = %q, want the name's entity color preserved (dimming disabled)", out)
	}
	if !strings.Contains(display.StripANSI(out), "$1") {
		t.Errorf("renderRow(closed) = %q, want the cost value still present", out)
	}
}

// TestRenderRowDimmingStaleWithoutInactivityKeepsFullColor covers "stale"
// liveness with no --inactive-after configured (isInactive always false,
// see TestIsInactive): under the new rule, only the time-based inactive
// bool dims a row, so a bare "stale" liveness no longer dims by itself.
func TestRenderRowDimmingStaleWithoutInactivityKeepsFullColor(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "@a", Liveness: "stale"}
	ac := display.EntityColor("@a", "")
	entityColor := display.RGB(ac[0], ac[1], ac[2])
	out := m.renderRow(row, false, cs, 16, nil, " ", false, 160)
	if !strings.Contains(out, entityColor) {
		t.Errorf("renderRow(stale, no inactive-after) = %q, want the name's entity color preserved (full, unhalved)", out)
	}
}

// TestRenderRowDimmingInactiveKeepsFullColor is the regression for the
// operator's "disable ALL dimming" direction: an "inactive" row (alive but
// no activity for --inactive-after) used to halve its entity color (§ctop
// AgentAF redesign item 3) — rowDimLevel now always returns dimNone, so
// full-brightness color must survive even when inactive is true.
func TestRenderRowDimmingInactiveKeepsFullColor(t *testing.T) {
	m := &agentsModel{inactiveAfter: 10 * time.Minute}
	cs := columnsForWidth(160, false)
	oldTs := time.Now().Add(-time.Hour).Format(time.RFC3339)
	row := Agent{AgentName: "@a", Liveness: "live", LastActivityTs: &oldTs, SessionCostUSD: 1.23}

	ac := display.EntityColor("@a", "")
	fullColor := display.RGB(ac[0], ac[1], ac[2])

	out := m.renderRow(row, false, cs, 16, nil, " ", false, 160)
	if !strings.Contains(out, fullColor) {
		t.Errorf("renderRow(inactive) = %q, want the full-brightness entity color present (dimming disabled)", out)
	}
	if !strings.Contains(display.StripANSI(out), "$1") {
		t.Errorf("renderRow(inactive) = %q, want the cost value still present", out)
	}
	// Not the "closed" flat-grey treatment either: ColorDim's raw RGB must
	// not be the entity name's color.
	dimColor := display.RGB(dimGreyRGB[0], dimGreyRGB[1], dimGreyRGB[2])
	if strings.Contains(out, dimColor+"@a") {
		t.Errorf("renderRow(inactive) = %q, want the entity name NOT flattened to ColorDim (that's the closed treatment)", out)
	}
}

func TestRenderRowDimmingLiveKeepsFullColor(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	row := Agent{AgentName: "@a", Liveness: "live"}
	ac := display.EntityColor("@a", "")
	entityColor := display.RGB(ac[0], ac[1], ac[2])
	out := m.renderRow(row, false, cs, 16, nil, " ", false, 160)
	if !strings.Contains(out, entityColor) {
		t.Errorf("renderRow(live) = %q, want the name's entity color present (no dimming)", out)
	}
}

// TestMetricStyleIsCyan checks the style object directly (GetForeground())
// rather than rendered output — lipgloss silently no-ops styling outside a
// real TTY (see TestFillBarColorKeyedOnlyOnPressureLevel's comment on the
// same quirk under `go test`), so a rendered-string search for the cyan
// escape would be unreliable here.
func TestMetricStyleIsCyan(t *testing.T) {
	if got := metricStyle.GetForeground(); got != tui.ColorMetric {
		t.Errorf("metricStyle foreground = %v, want tui.ColorMetric %v", got, tui.ColorMetric)
	}
}

func TestCellGroupSkipsEmptyGroupsAndSpacesCorrectly(t *testing.T) {
	g := newCellGroup()
	g.add("A")
	g.next() // empty group, should be skipped
	g.next()
	g.add("B")
	g.add("C")
	if got, want := g.render(), "A  B C"; got != want {
		t.Errorf("cellGroup.render() = %q, want %q", got, want)
	}
}

func TestNameGroupSep(t *testing.T) {
	if got := nameGroupSep(colSet{host: true}); got != " " {
		t.Errorf("nameGroupSep(host=true) = %q, want single space", got)
	}
	if got := nameGroupSep(colSet{model: true}); got != " " {
		t.Errorf("nameGroupSep(model=true) = %q, want single space", got)
	}
	if got := nameGroupSep(colSet{}); got != "  " {
		t.Errorf("nameGroupSep(no host/model) = %q, want double space", got)
	}
}

// TestBlankSpanWidthMatchesRenderRowPrefix is the alignment regression for
// renderSummaryRow's TOTAL row: with tokens/cost/toolcalls/last/activity all
// gated off, renderRow's own output IS exactly the
// marker+fold+nameCell+nameGroupSep+HOST/MODEL/ST/CTX span — so its width
// must equal 2 (marker+fold) + agentW + blankSpanWidth(cs) exactly, or the
// TOTAL row's blank/focus placeholder would drift out from under the real
// rows' HOST/MODEL/ST/CTX columns.
func TestBlankSpanWidthMatchesRenderRowPrefix(t *testing.T) {
	m := &agentsModel{}
	const agentW = 12
	variants := []colSet{
		{host: true, model: true},
		{host: false, model: false},
		{host: true, model: false},
		{host: false, model: true},
	}
	for _, cs := range variants {
		out := display.StripANSI(m.renderRow(Agent{AgentName: "@a"}, false, cs, agentW, nil, "·", false, 160))
		want := 1 + 1 + agentW + blankSpanWidth(cs) // marker(1) + fold(1) + agentW + span
		if got := lipgloss.Width(out); got != want {
			t.Errorf("renderRow width (host=%v model=%v) = %d, want %d (matches blankSpanWidth)", cs.host, cs.model, got, want)
		}
	}
}

// TestRenderRowActivityBeforeLastRightJustified is the regression for §ctop
// AgentAF redesign item 5: LAST moves to the FINAL column, after the
// (variable-width) ACTIVITY text, right-justified within its own small
// field rather than anchoring a fixed screen column.
func TestRenderRowActivityBeforeLastRightJustified(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	ts := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	row := Agent{AgentName: "@a", Liveness: "live", LastActivityTs: &ts, LastActivityDisplay: "thinking hard"}
	out := display.StripANSI(m.renderRow(row, false, cs, 16, nil, " ", false, 160))

	actIdx := strings.Index(out, "thinking hard")
	if actIdx == -1 {
		t.Fatalf("renderRow output %q missing activity text", out)
	}
	lastIdx := strings.LastIndex(out, "2h") // relativeTime(2h ago) == "2h"
	if lastIdx == -1 {
		t.Fatalf("renderRow output %q missing the LAST value %q", out, "2h")
	}
	if lastIdx < actIdx {
		t.Errorf("renderRow output %q, want LAST (%q) to appear after ACTIVITY text (index %d), got LAST at index %d", out, "2h", actIdx, lastIdx)
	}
	// padRight(_, 4) right-justifies "2h" within a 4-cell field: "  2h".
	if !strings.Contains(out, "  2h") {
		t.Errorf("renderRow output %q, want LAST right-justified in its 4-cell field (\"  2h\")", out)
	}
}

// TestRenderRowAgeAlignsAcrossRaggedActivityWidths is the regression for the
// operator-reported "AGE column" bug: LAST/AGE was already the final
// cellGroup entry, but a fixed 2-space gap directly after ACTIVITY's raw,
// unpadded display text meant its screen column drifted with every row's
// activity text length — "align visually" required right-anchoring AGE to
// the panel's own width, not just its logical position in the cellGroup.
func TestRenderRowAgeAlignsAcrossRaggedActivityWidths(t *testing.T) {
	m := &agentsModel{}
	cs := columnsForWidth(160, false)
	ts := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	short := Agent{AgentName: "@a", Liveness: "live", LastActivityTs: &ts, LastActivityDisplay: "idle"}
	long := Agent{AgentName: "@b", Liveness: "live", LastActivityTs: &ts, LastActivityDisplay: "a much longer activity description here"}

	outShort := display.StripANSI(m.renderRow(short, false, cs, 16, nil, " ", false, 160))
	outLong := display.StripANSI(m.renderRow(long, false, cs, 16, nil, " ", false, 160))

	idxShort := strings.LastIndex(outShort, "2h")
	idxLong := strings.LastIndex(outLong, "2h")
	if idxShort == -1 || idxLong == -1 {
		t.Fatalf("missing AGE value: outShort=%q outLong=%q", outShort, outLong)
	}
	if idxShort != idxLong {
		t.Errorf("AGE column not aligned: short-activity row has \"2h\" at index %d, long-activity row at index %d (want equal — AGE must be right-anchored to the panel width, independent of ACTIVITY length)", idxShort, idxLong)
	}
	if got := lipgloss.Width(outShort); got != 160 {
		t.Errorf("renderRow output width = %d, want exactly 160 (right-anchored AGE pads out to the full panel width)", got)
	}
}

// TestViewHeaderOrdersActivityBeforeLast checks the same reorder in the
// column header line View() builds.
func TestViewHeaderOrdersActivityBeforeLast(t *testing.T) {
	m := &agentsModel{}
	m.setRows([]Agent{{AgentName: "@a", SessionID: "s1", Liveness: "live"}})
	out := display.StripANSI(m.View(160, 10, false, nil, false))
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("View() output too short: %q", out)
	}
	header := lines[1]
	actIdx := strings.Index(header, "ACTIVITY")
	lastIdx := strings.Index(header, "LAST")
	if actIdx == -1 || lastIdx == -1 {
		t.Fatalf("header %q missing ACTIVITY or LAST", header)
	}
	if lastIdx < actIdx {
		t.Errorf("header %q, want LAST after ACTIVITY (§ctop AgentAF redesign item 5)", header)
	}
}

func TestTitleBandSkipsBackgroundWhenNotColorized(t *testing.T) {
	m := &agentsModel{}
	out := m.titleBand("muster · agents", 60, true, false)
	if strings.Contains(out, "48;2;") {
		t.Errorf("titleBand(colorize=false) = %q, want no truecolor bg escape", out)
	}
}

func TestTitleBandAppliesBackgroundWhenColorized(t *testing.T) {
	m := &agentsModel{}
	out := m.titleBand("muster · agents", 60, true, true)
	if !strings.Contains(out, "48;2;") {
		t.Error("titleBand(colorize=true) should apply a truecolor bg")
	}
}

func TestAnyCompositionDataGatesLegend(t *testing.T) {
	m := &agentsModel{}
	if m.anyCompositionData() {
		t.Error("anyCompositionData() with no rows should be false")
	}
	comp := `{"text_pct":0.5,"tool_use_pct":0.3,"thinking_pct":0.2}`
	m.rows = []Agent{{AgentName: "@a", CompositionJSON: &comp}}
	if !m.anyCompositionData() {
		t.Error("anyCompositionData() with a parseable composition_json row should be true")
	}
}

func TestTitleBandShowsLegendOnlyWithCompositionData(t *testing.T) {
	m := &agentsModel{}
	out := display.StripANSI(m.titleBand("muster · agents", 80, false, false))
	if strings.Contains(out, "▓text") {
		t.Errorf("titleBand with no composition data = %q, want no legend", out)
	}

	comp := `{"text_pct":0.5,"tool_use_pct":0.3,"thinking_pct":0.2}`
	m.rows = []Agent{{AgentName: "@a", CompositionJSON: &comp}}
	out = display.StripANSI(m.titleBand("muster · agents", 80, false, false))
	if !strings.Contains(out, "▓text") {
		t.Errorf("titleBand with composition data = %q, want the legend", out)
	}
}
