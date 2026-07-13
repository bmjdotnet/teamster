// Package health implements the health MCP tool handlers. It is
// transport-agnostic: no imports from internal/server or net/http.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	mcproster "github.com/bmjdotnet/teamster/internal/mcp/roster"
	"github.com/bmjdotnet/teamster/internal/store"
)

type callParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
	Meta      meta                   `json:"_meta"`
}

// meta carries request identity from params._meta. Duplicated from
// internal/mcp/roster (and internal/mcp/wms) rather than imported, to keep
// this package transport-agnostic and decoupled — same rationale as the
// duplicated CallError/Result types below.
type meta struct {
	SessionID string         `json:"session_id"`
	AgentType string         `json:"agent_type"`
	CodexTurn *codexTurnMeta `json:"x-codex-turn-metadata"`
}

// codexTurnMeta is the subset of Codex's per-turn MCP metadata this package
// uses. See internal/mcp/wms.CodexTurnMeta for the full rationale.
type codexTurnMeta struct {
	SessionID string `json:"session_id"`
}

// resolveSessionID mirrors internal/mcp/roster.resolveSessionID — see there
// for the fallback-priority rationale.
func resolveSessionID(m *meta) {
	if m.CodexTurn != nil && m.CodexTurn.SessionID != "" {
		m.SessionID = m.CodexTurn.SessionID
		return
	}
	if m.SessionID != "" {
		return
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TEAMSTER_RUNTIME")), "codex") {
		m.SessionID = "unknown-codex"
		return
	}
	m.SessionID = readCurrentSessionID()
}

func readCurrentSessionID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "current-session-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolveCallerTeam resolves the calling agent's team_name via its roster
// entry (the only field with a live write path — registerPeer). Best-effort:
// any failure yields "", which callers treat as "no team" and fall back to
// session-scoping.
func resolveCallerTeam(ctx context.Context, mainStore store.Store, sessionID, agentName string) string {
	if sessionID == "" {
		return ""
	}
	rosterID, err := mainStore.ResolveRosterID(ctx, sessionID, agentName)
	if err != nil {
		return ""
	}
	entry, err := mainStore.GetRosterEntry(ctx, rosterID)
	if err != nil {
		return ""
	}
	return entry.TeamName
}

// CallError represents a JSON-RPC error for a tools/call.
type CallError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

func (e *CallError) Error() string { return e.Message }

// Result is the MCP tools/call success result.
type Result struct {
	Content []map[string]interface{} `json:"content"`
}

// TextResult wraps a text string in the MCP content envelope.
func TextResult(text string) Result {
	return Result{Content: []map[string]interface{}{{"type": "text", "text": text}}}
}

// JSONResult wraps a marshaled value in the MCP content envelope.
func JSONResult(v interface{}) Result {
	data, _ := json.Marshal(v)
	return TextResult(string(data))
}

func validationErr(msg string) *CallError {
	return &CallError{Code: -32602, Message: msg, Reason: "INVALID_ARGUMENT"}
}

func notFoundErr(msg string) *CallError {
	return &CallError{Code: -32000, Message: msg, Reason: "NOT_FOUND"}
}

func internalErr(msg string) *CallError {
	return &CallError{Code: -32000, Message: msg}
}

// FormatError renders a CallError as a map for JSON-RPC error responses.
func FormatError(e *CallError) map[string]interface{} {
	result := map[string]interface{}{
		"code":    e.Code,
		"message": e.Message,
	}
	if e.Reason != "" {
		result["reason"] = e.Reason
	}
	return result
}

// TurnStateLookup reports whether (sessionID, agentName) is currently
// mid-turn. Turn state is hookd's in-memory, per-process signal (see
// internal/server/turn_state.go); this package stays transport-agnostic, so
// callers pass a lookup func rather than the tracker itself. A nil lookup
// (e.g. the standalone health-mcp stdio binary, which has no hookd process to
// ask) leaves IsProcessing false for every row.
type TurnStateLookup func(sessionID, agentName string) bool

// HandleToolCall dispatches a tools/call request to the appropriate handler.
func HandleToolCall(mainStore store.Store, gaugeStore gauge.GaugeStore, turnLookup TurnStateLookup, rawParams json.RawMessage) (Result, *CallError) {
	var p callParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return Result{}, validationErr("invalid params")
	}

	strArg := func(key string) string {
		v, _ := p.Arguments[key].(string)
		return strings.TrimSpace(v)
	}

	ctx := context.Background()

	resolveSessionID(&p.Meta)
	callerTeam := resolveCallerTeam(ctx, mainStore, p.Meta.SessionID, p.Meta.AgentType)

	switch p.Name {
	case "health_listAgents":
		return handleListAgents(ctx, mainStore, gaugeStore, turnLookup, p.Arguments, p.Meta.SessionID, callerTeam)
	case "health_getAgentSnapshot":
		return handleGetAgentSnapshot(ctx, mainStore, gaugeStore, turnLookup, strArg("roster_id"))
	case "health_getTeamSummary":
		return handleGetTeamSummary(ctx, mainStore, gaugeStore, strArg("team_name"), callerTeam)
	case "health_getPressureAlerts":
		return handleGetPressureAlerts(ctx, mainStore, gaugeStore, turnLookup, p.Meta.SessionID, callerTeam)
	default:
		return Result{}, &CallError{Code: -32601, Message: "unknown tool: " + p.Name}
	}
}

// --- response views ---

type agentHealthView struct {
	RosterID       *string `json:"roster_id"`
	Host           string  `json:"host"`
	Username       string  `json:"username,omitempty"`
	SessionID      string  `json:"session_id"`
	AgentName      string  `json:"agent_name"`
	Runtime        string  `json:"runtime"`
	Model          string  `json:"model"`
	TeamName       string  `json:"team_name,omitempty"`
	Relationship   string  `json:"relationship,omitempty"`
	ParentRef      *string `json:"parent_ref,omitempty"`
	Liveness       string  `json:"liveness,omitempty"`
	ContextFillPct float64 `json:"context_fill_pct"`
	SessionCostUSD float64 `json:"session_cost_usd"`
	// SessionTotalCostUSD is the full session's spend from token_ledger
	// (every agent_name, including ones already swept from
	// agent_health_gauge) — only ever non-zero on the lead's row
	// (AgentName==""). See gauge.GaugeRow.SessionTotalCostUSD.
	SessionTotalCostUSD float64 `json:"session_total_cost_usd,omitempty"`
	PressureLevel       string  `json:"pressure_level"`
	CollectorStatus     string  `json:"collector_status"`
	TokensInTotal       int64   `json:"tokens_in_total"`
	TokensOutTotal      int64   `json:"tokens_out_total"`
	ToolCallsTotal      int64   `json:"tool_calls_total"`
	LastActivityTs      *string `json:"last_activity_ts,omitempty"`
	LastActivityDisplay string  `json:"last_activity_display,omitempty"`
	// LastActivityTag is the tag (READ/EDIT/GOAL/THNK/DONE/...) that colors
	// LastActivityDisplay, sourced from gauge.GaugeRow.LastActivityTool —
	// that column is misleadingly named "tool" but has held the tag value
	// since server.go's activityFromData/UpdateActivity wiring, never a tool
	// name. Lets ctop color an idle agent's fallback activity text by its
	// canonical tag instead of dim grey.
	LastActivityTag string `json:"last_activity_tag,omitempty"`
	CurrentFocus    string `json:"current_focus,omitempty"`
	// CompositionJSON lets every list row (not just a per-agent snapshot
	// fetch) render a composition-colored context bar — ctop's SegmentedBar
	// used to be selected-row-only because this field was snapshot-only.
	CompositionJSON *string `json:"composition_json,omitempty"`
	IsProcessing    bool    `json:"is_processing"`
}

type agentSnapshotView struct {
	agentHealthView
	ContextWindowTokens   int64   `json:"context_window_tokens"`
	ContextTokensUsed     int64   `json:"context_tokens_used"`
	ContextTokensFree     int64   `json:"context_tokens_free"`
	LongContextActive     bool    `json:"long_context_active"`
	ContextResetSuspected bool    `json:"context_reset_suspected"`
	CompositionJSON       *string `json:"composition_json,omitempty"`
	ToolCallCountsJSON    *string `json:"tool_call_counts_json,omitempty"`
	StatuslineJSON        *string `json:"statusline_json,omitempty"`
	FidelityNotes         *string `json:"fidelity_notes,omitempty"`
}

type teamSummaryView struct {
	TeamName       string  `json:"team_name"`
	AgentCount     int     `json:"agent_count"`
	TotalTokensIn  int64   `json:"total_tokens_in"`
	TotalTokensOut int64   `json:"total_tokens_out"`
	AvgContextFill float64 `json:"avg_context_fill_pct"`
	WarningCount   int     `json:"warning_count"`
	CriticalCount  int     `json:"critical_count"`
}

func buildHealthView(g gauge.GaugeRow, rosterEntry *store.RosterEntry, session *store.Session, turnLookup TurnStateLookup) agentHealthView {
	v := agentHealthView{
		RosterID:            g.RosterID,
		Host:                g.Host,
		SessionID:           g.SessionID,
		AgentName:           g.AgentName,
		Runtime:             g.Runtime,
		Model:               g.Model,
		ContextFillPct:      g.ContextFillPct,
		SessionCostUSD:      g.SessionCostUSD,
		SessionTotalCostUSD: g.SessionTotalCostUSD,
		PressureLevel:       g.PressureLevel,
		CollectorStatus:     g.CollectorStatus,
		TokensInTotal:       g.TokensInTotal,
		TokensOutTotal:      g.TokensOutTotal,
		ToolCallsTotal:      g.ToolCallsTotal,
		LastActivityDisplay: g.LastActivityDisplay,
		LastActivityTag:     g.LastActivityTool,
		CompositionJSON:     g.CompositionJSON,
	}

	if g.LastActivityTs != nil {
		ts := g.LastActivityTs.UTC().Format("2006-01-02T15:04:05Z")
		v.LastActivityTs = &ts
	}

	if rosterEntry != nil {
		v.TeamName = rosterEntry.TeamName
		v.Relationship = rosterEntry.Relationship
		v.ParentRef = rosterEntry.ParentRef
		v.Liveness = mcproster.ComputeLiveness(*rosterEntry, session)
	}

	if session != nil && session.Focus != "" {
		v.CurrentFocus = session.Focus
	}
	if session != nil {
		v.Username = session.Username
	}

	if turnLookup != nil {
		v.IsProcessing = turnLookup(g.SessionID, g.AgentName)
	}

	return v
}

// enrichGaugeRow looks up roster and session data for a gauge row.
func enrichGaugeRow(ctx context.Context, mainStore store.Store, g gauge.GaugeRow) (*store.RosterEntry, *store.Session) {
	var rosterEntry *store.RosterEntry
	var session *store.Session

	if g.RosterID != nil && *g.RosterID != "" {
		entry, err := mainStore.GetRosterEntry(ctx, *g.RosterID)
		if err == nil {
			rosterEntry = &entry
		}
	}

	if g.SessionID != "" {
		sess, err := mainStore.GetSession(ctx, store.SessionKey{
			SessionID: g.SessionID,
			AgentName: g.AgentName,
		})
		if err == nil {
			session = &sess
		}
	}

	return rosterEntry, session
}

// defaultLivenessOK returns true if the computed liveness should be included
// in the default scope (live, idle only — no unbound since gauge rows only
// exist once real telemetry arrives).
func defaultLivenessOK(liveness string) bool {
	return liveness == mcproster.LivenessLive || liveness == mcproster.LivenessIdle
}

func parseLivenessFilter(args map[string]interface{}) map[string]bool {
	raw, ok := args["liveness"]
	if !ok {
		return nil
	}
	result := make(map[string]bool)
	switch v := raw.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v != "" {
			result[v] = true
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					result[s] = true
				}
			}
		}
	}
	return result
}

// --- tool handlers ---

// callerAllowsRow reports whether the caller may see a gauge row: same team
// if the caller belongs to one, else just the caller's own session (solo
// mode / before /teamster:bootstrap). If the caller's identity could not be
// resolved at all (no session_id), rows pass through unscoped.
func callerAllowsRow(g gauge.GaugeRow, rosterEntry *store.RosterEntry, callerSessionID, callerTeam string) bool {
	if callerTeam != "" {
		return rosterEntry != nil && rosterEntry.TeamName == callerTeam
	}
	if callerSessionID != "" {
		return g.SessionID == callerSessionID
	}
	return true
}

func handleListAgents(ctx context.Context, mainStore store.Store, gaugeStore gauge.GaugeStore, turnLookup TurnStateLookup, args map[string]interface{}, callerSessionID, callerTeam string) (Result, *CallError) {
	filter := gauge.GaugeFilter{}
	if v, ok := args["host"].(string); ok {
		filter.Host = strings.TrimSpace(v)
	}
	if v, ok := args["runtime"].(string); ok {
		filter.Runtime = strings.TrimSpace(v)
	}

	livenessFilter := parseLivenessFilter(args)

	rows, err := gaugeStore.List(ctx, filter)
	if err != nil {
		return Result{}, internalErr(err.Error())
	}

	var views []agentHealthView
	for _, g := range rows {
		rosterEntry, session := enrichGaugeRow(ctx, mainStore, g)
		if !callerAllowsRow(g, rosterEntry, callerSessionID, callerTeam) {
			continue
		}
		v := buildHealthView(g, rosterEntry, session, turnLookup)

		if len(livenessFilter) > 0 {
			if v.Liveness != "" && !livenessFilter[v.Liveness] {
				continue
			}
		} else if v.Liveness != "" && !defaultLivenessOK(v.Liveness) {
			continue
		}

		views = append(views, v)
	}

	return JSONResult(views), nil
}

func handleGetAgentSnapshot(ctx context.Context, mainStore store.Store, gaugeStore gauge.GaugeStore, turnLookup TurnStateLookup, rosterID string) (Result, *CallError) {
	if rosterID == "" {
		return Result{}, validationErr("roster_id is required")
	}

	rows, err := gaugeStore.List(ctx, gauge.GaugeFilter{RosterID: rosterID})
	if err != nil {
		return Result{}, internalErr(err.Error())
	}
	if len(rows) == 0 {
		return Result{}, notFoundErr("no gauge data for roster_id: " + rosterID)
	}

	g := rows[0]
	rosterEntry, session := enrichGaugeRow(ctx, mainStore, g)
	base := buildHealthView(g, rosterEntry, session, turnLookup)

	snapshot := agentSnapshotView{
		agentHealthView:       base,
		ContextWindowTokens:   g.ContextWindowTokens,
		ContextTokensUsed:     g.ContextTokensUsed,
		ContextTokensFree:     g.ContextTokensFree,
		LongContextActive:     g.LongContextActive,
		ContextResetSuspected: g.ContextResetSuspected,
		CompositionJSON:       g.CompositionJSON,
		ToolCallCountsJSON:    g.ToolCallCountsJSON,
		StatuslineJSON:        g.StatuslineJSON,
		FidelityNotes:         g.FidelityNotes,
	}

	return JSONResult(snapshot), nil
}

func handleGetTeamSummary(ctx context.Context, mainStore store.Store, gaugeStore gauge.GaugeStore, teamName, callerTeam string) (Result, *CallError) {
	if callerTeam != "" {
		if teamName == "" {
			teamName = callerTeam
		} else if teamName != callerTeam {
			return Result{}, &CallError{Code: -32000, Message: "team_name must match caller's own team", Reason: "FORBIDDEN"}
		}
	}
	if teamName == "" {
		return Result{}, validationErr("team_name is required")
	}

	entries, err := mainStore.ListRosterEntries(ctx, store.RosterFilter{})
	if err != nil {
		return Result{}, internalErr(err.Error())
	}

	var rosterIDs []string
	for _, e := range entries {
		if e.TeamName == teamName {
			rosterIDs = append(rosterIDs, e.RosterID)
		}
	}

	if len(rosterIDs) == 0 {
		return JSONResult(teamSummaryView{TeamName: teamName}), nil
	}

	allRows, err := gaugeStore.List(ctx, gauge.GaugeFilter{})
	if err != nil {
		return Result{}, internalErr(err.Error())
	}

	rosterSet := make(map[string]bool, len(rosterIDs))
	for _, id := range rosterIDs {
		rosterSet[id] = true
	}

	summary := teamSummaryView{TeamName: teamName}
	var fillSum float64
	for _, g := range allRows {
		if g.RosterID == nil || !rosterSet[*g.RosterID] {
			continue
		}
		summary.AgentCount++
		summary.TotalTokensIn += g.TokensInTotal
		summary.TotalTokensOut += g.TokensOutTotal
		fillSum += g.ContextFillPct
		switch g.PressureLevel {
		case "warning":
			summary.WarningCount++
		case "critical":
			summary.CriticalCount++
		}
	}
	if summary.AgentCount > 0 {
		summary.AvgContextFill = fillSum / float64(summary.AgentCount)
	}

	return JSONResult(summary), nil
}

func handleGetPressureAlerts(ctx context.Context, mainStore store.Store, gaugeStore gauge.GaugeStore, turnLookup TurnStateLookup, callerSessionID, callerTeam string) (Result, *CallError) {
	rows, err := gaugeStore.List(ctx, gauge.GaugeFilter{})
	if err != nil {
		return Result{}, internalErr(err.Error())
	}

	var alerts []agentHealthView
	for _, g := range rows {
		if g.PressureLevel == "ok" || g.PressureLevel == "" {
			continue
		}
		rosterEntry, session := enrichGaugeRow(ctx, mainStore, g)
		if !callerAllowsRow(g, rosterEntry, callerSessionID, callerTeam) {
			continue
		}
		alerts = append(alerts, buildHealthView(g, rosterEntry, session, turnLookup))
	}

	return JSONResult(alerts), nil
}

// --- helpers for store errors ---

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// ToolDefs is the MCP tools/list payload for the health server.
var ToolDefs = []map[string]interface{}{
	{
		"name":        "health_listAgents",
		"description": "List agent health gauges with optional filters. Default scope: live/idle agents only.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"host":    map[string]interface{}{"type": "string", "description": "Filter by host"},
				"runtime": map[string]interface{}{"type": "string", "description": "Filter by runtime (claude_code, codex)"},
				"liveness": map[string]interface{}{
					"description": "Filter by liveness tier(s). String or array of strings.",
					"oneOf": []map[string]interface{}{
						{"type": "string"},
						{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
			},
		},
	},
	{
		"name":        "health_getAgentSnapshot",
		"description": "Get full health snapshot for a single agent by roster_id. Includes context window details, composition, and tool call counts.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"roster_id": map[string]interface{}{"type": "string"},
			},
			"required": []string{"roster_id"},
		},
	},
	{
		"name":        "health_getTeamSummary",
		"description": "Get aggregate health summary for a team: agent count, total tokens, average context fill, pressure alerts.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"team_name": map[string]interface{}{"type": "string"},
			},
			"required": []string{"team_name"},
		},
	},
	{
		"name":        "health_getPressureAlerts",
		"description": "List all agents with pressure_level != 'ok'. Quick 'who needs attention' view.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
}
