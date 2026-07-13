package health_test

import (
	"encoding/json"
	"testing"

	mcphealth "github.com/bmjdotnet/teamster/internal/mcp/health"
)

// TestDashboardJSON_ListAgentsIsUnscoped is the core D1 guarantee: unlike the
// agent-facing health_listAgents MCP path (which scopes to the caller's own
// team, see health_test.go's TestListAgentsScopedToCallerTeam), DashboardJSON
// must return every agent regardless of team, since it never has caller
// identity to scope by (callerSessionID/callerTeam are always "").
func TestDashboardJSON_ListAgentsIsUnscoped(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rA := "r-a"
	rB := "r-b"
	seedRosterEntry(t, ms, rA, "sess-alpha", "@a", "alpha", "teammate")
	seedRosterEntry(t, ms, rB, "sess-beta", "@b", "beta", "lead")
	seedGaugeRow(t, gs, "host-a", "sess-alpha", "@a", "claude_code", "opus", "ok", &rA, 10.0)
	seedGaugeRow(t, gs, "host-b", "sess-beta", "@b", "claude_code", "opus", "ok", &rB, 20.0)

	payload, cerr := mcphealth.DashboardJSON(ms, gs, nil, "health_listAgents", map[string]interface{}{})
	if cerr != nil {
		t.Fatalf("DashboardJSON: %v", cerr)
	}

	var views []struct {
		AgentName string `json:"agent_name"`
		TeamName  string `json:"team_name"`
	}
	if err := json.Unmarshal(payload, &views); err != nil {
		t.Fatalf("unmarshal payload: %v (payload: %s)", err, payload)
	}

	if len(views) != 2 {
		t.Fatalf("expected both teams' agents unscoped, got %d: %+v", len(views), views)
	}
	teams := map[string]bool{}
	for _, v := range views {
		teams[v.TeamName] = true
	}
	if !teams["alpha"] || !teams["beta"] {
		t.Fatalf("expected both alpha and beta present, got teams: %+v", teams)
	}
}

func TestDashboardJSON_GetAgentSnapshot(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rid := "r-snap"
	seedRosterEntry(t, ms, rid, "sess-snap", "@lead", "team-x", "lead")
	seedGaugeRow(t, gs, "host-a", "sess-snap", "@lead", "claude_code", "opus", "ok", &rid, 40.0)

	payload, cerr := mcphealth.DashboardJSON(ms, gs, nil, "health_getAgentSnapshot", map[string]interface{}{
		"roster_id": rid,
	})
	if cerr != nil {
		t.Fatalf("DashboardJSON: %v", cerr)
	}

	var snap struct {
		RosterID *string `json:"roster_id"`
	}
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if snap.RosterID == nil || *snap.RosterID != rid {
		t.Fatalf("roster_id = %v, want %s", snap.RosterID, rid)
	}
}

// TestDashboardJSON_GetTeamSummaryIsUnscoped confirms the dashboard can query
// any team's summary by name — no FORBIDDEN check like the agent-facing path
// (health_test.go's TestGetTeamSummaryRejectsOtherTeam), since there is no
// caller team to enforce against.
func TestDashboardJSON_GetTeamSummaryIsUnscoped(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rid := "r-beta"
	seedRosterEntry(t, ms, rid, "sess-beta", "@b", "beta", "lead")
	seedGaugeRow(t, gs, "host-b", "sess-beta", "@b", "claude_code", "opus", "ok", &rid, 60.0)

	payload, cerr := mcphealth.DashboardJSON(ms, gs, nil, "health_getTeamSummary", map[string]interface{}{
		"team_name": "beta",
	})
	if cerr != nil {
		t.Fatalf("DashboardJSON: %v", cerr)
	}

	var summary struct {
		TeamName   string `json:"team_name"`
		AgentCount int    `json:"agent_count"`
	}
	if err := json.Unmarshal(payload, &summary); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if summary.TeamName != "beta" || summary.AgentCount != 1 {
		t.Fatalf("summary = %+v, want team beta with 1 agent", summary)
	}
}

func TestDashboardJSON_GetPressureAlertsIsUnscoped(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rA := "r-a-warn"
	rB := "r-b-crit"
	seedRosterEntry(t, ms, rA, "sess-alpha", "@a", "alpha", "teammate")
	seedRosterEntry(t, ms, rB, "sess-beta", "@b", "beta", "teammate")
	seedGaugeRow(t, gs, "host-a", "sess-alpha", "@a", "claude_code", "opus", "warning", &rA, 75.0)
	seedGaugeRow(t, gs, "host-b", "sess-beta", "@b", "claude_code", "opus", "critical", &rB, 95.0)

	payload, cerr := mcphealth.DashboardJSON(ms, gs, nil, "health_getPressureAlerts", map[string]interface{}{})
	if cerr != nil {
		t.Fatalf("DashboardJSON: %v", cerr)
	}

	var alerts []struct {
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(payload, &alerts); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("expected both teams' alerts unscoped, got %d: %+v", len(alerts), alerts)
	}
}

func TestDashboardJSON_UnknownTool(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	_, cerr := mcphealth.DashboardJSON(ms, gs, nil, "nonexistent_tool", map[string]interface{}{})
	if cerr == nil {
		t.Fatal("expected error for unknown tool")
	}
	if cerr.Code != -32601 {
		t.Fatalf("expected code -32601, got %d", cerr.Code)
	}
}
