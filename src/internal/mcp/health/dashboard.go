package health

import (
	"context"
	"strings"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/store"
)

// DashboardJSON dispatches a health tool with unscoped operator identity and
// returns the marshaled JSON payload (the same bytes JSONResult would wrap).
// This is the entry point for hookd's /health/api/* dashboard routes: an
// operator dashboard deliberately sees all agents, so callerSessionID and
// callerTeam are empty (callerAllowsRow passes everything through) — unlike
// the agent-facing /mcp/health path, which scopes to the caller's own team.
//
// Single-sourced with /mcp/health: this calls the exact same handler
// functions, so field names, liveness enrichment, and default liveness scope
// can never drift between the agent view and the dashboard view.
func DashboardJSON(mainStore store.Store, gaugeStore gauge.GaugeStore, turnLookup TurnStateLookup, tool string, args map[string]interface{}) ([]byte, *CallError) {
	ctx := context.Background()
	var res Result
	var cerr *CallError
	switch tool {
	case "health_listAgents":
		res, cerr = handleListAgents(ctx, mainStore, gaugeStore, turnLookup, args, "", "")
	case "health_getAgentSnapshot":
		rid, _ := args["roster_id"].(string)
		res, cerr = handleGetAgentSnapshot(ctx, mainStore, gaugeStore, turnLookup, strings.TrimSpace(rid))
	case "health_getTeamSummary":
		tn, _ := args["team_name"].(string)
		res, cerr = handleGetTeamSummary(ctx, mainStore, gaugeStore, strings.TrimSpace(tn), "")
	case "health_getPressureAlerts":
		res, cerr = handleGetPressureAlerts(ctx, mainStore, gaugeStore, turnLookup, "", "")
	default:
		return nil, &CallError{Code: -32601, Message: "unknown tool: " + tool}
	}
	if cerr != nil {
		return nil, cerr
	}
	if len(res.Content) == 0 {
		return []byte("null"), nil
	}
	text, _ := res.Content[0]["text"].(string)
	return []byte(text), nil
}
