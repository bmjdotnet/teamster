package health_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	mcphealth "github.com/bmjdotnet/teamster/internal/mcp/health"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

func openMainStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// memGaugeStore is a trivial in-memory gauge.GaugeStore for testing.
type memGaugeStore struct {
	mu   sync.Mutex
	rows map[gauge.GaugeKey]gauge.GaugeRow
}

func newMemGaugeStore() *memGaugeStore {
	return &memGaugeStore{rows: make(map[gauge.GaugeKey]gauge.GaugeRow)}
}

func (m *memGaugeStore) Upsert(_ context.Context, row gauge.GaugeRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := gauge.GaugeKey{Host: row.Host, SessionID: row.SessionID, AgentName: row.AgentName}
	m.rows[key] = row
	return nil
}

func (m *memGaugeStore) Get(_ context.Context, key gauge.GaugeKey) (gauge.GaugeRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[key]
	return row, ok, nil
}

func (m *memGaugeStore) List(_ context.Context, filter gauge.GaugeFilter) ([]gauge.GaugeRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []gauge.GaugeRow
	for _, row := range m.rows {
		if filter.Host != "" && row.Host != filter.Host {
			continue
		}
		if filter.Runtime != "" && row.Runtime != filter.Runtime {
			continue
		}
		if filter.RosterID != "" && (row.RosterID == nil || *row.RosterID != filter.RosterID) {
			continue
		}
		if filter.MinUpdatedAt != nil && row.UpdatedAt.Before(*filter.MinUpdatedAt) {
			continue
		}
		result = append(result, row)
	}
	return result, nil
}

func (m *memGaugeStore) SweepOffline(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, row := range m.rows {
		if row.UpdatedAt.Before(cutoff) {
			delete(m.rows, k)
			n++
		}
	}
	return n, nil
}

func (m *memGaugeStore) UpdateActivity(_ context.Context, key gauge.GaugeKey, display, tool string, ts time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[key]
	row.LastActivityDisplay = display
	row.LastActivityTool = tool
	row.LastActivityTs = &ts
	m.rows[key] = row
	return nil
}

// call dispatches a tools/call with no _meta (anonymous caller). Points HOME
// at an isolated empty temp dir first — this host's own CLAUDE.md notes it
// constantly has a real ~/.claude/current-session-id from an active Claude
// session, which would otherwise leak into resolveSessionID's fallback and
// make caller-team-scoping non-deterministic in tests. See
// internal/mcp/wms/identity_test.go for the same pattern.
func call(t *testing.T, mainStore store.Store, gaugeStore gauge.GaugeStore, toolName string, args map[string]interface{}) (mcphealth.Result, *mcphealth.CallError) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	params := map[string]interface{}{
		"name":      toolName,
		"arguments": args,
	}
	raw, _ := json.Marshal(params)
	return mcphealth.HandleToolCall(mainStore, gaugeStore, nil, raw)
}

// callAs dispatches a tools/call with an explicit _meta identity, so tests
// can exercise caller-team-scoping deterministically.
func callAs(t *testing.T, mainStore store.Store, gaugeStore gauge.GaugeStore, sessionID, agentType, toolName string, args map[string]interface{}) (mcphealth.Result, *mcphealth.CallError) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	params := map[string]interface{}{
		"name":      toolName,
		"arguments": args,
		"_meta": map[string]interface{}{
			"session_id": sessionID,
			"agent_type": agentType,
		},
	}
	raw, _ := json.Marshal(params)
	return mcphealth.HandleToolCall(mainStore, gaugeStore, nil, raw)
}

func seedGaugeRow(t *testing.T, gs gauge.GaugeStore, host, sessID, agentName, runtime, model, pressure string, rosterID *string, fillPct float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	row := gauge.GaugeRow{
		Host:            host,
		SessionID:       sessID,
		AgentName:       agentName,
		RosterID:        rosterID,
		Runtime:         runtime,
		Model:           model,
		PressureLevel:   pressure,
		CollectorStatus: "fresh",
		ContextFillPct:  fillPct,
		UpdatedAt:       now,
		LastActivityTs:  &now,
	}
	if err := gs.Upsert(ctx, row); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
}

func seedRosterEntry(t *testing.T, ms store.Store, rosterID, sessID, agentName, teamName, relationship string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	sid := sessID
	entry := store.RosterEntry{
		RosterID:     rosterID,
		SessionID:    &sid,
		AgentName:    agentName,
		Host:         "host-a",
		Runtime:      "claude_code",
		Relationship: relationship,
		TeamName:     teamName,
		CreatedAt:    now,
		BoundAt:      &now,
	}
	if err := ms.CreateRosterEntry(ctx, entry); err != nil {
		t.Fatalf("seed roster: %v", err)
	}
	// Also create a session so liveness can be computed.
	sess := store.Session{
		SessionID: sessID,
		AgentName: agentName,
		Host:      "host-a",
		LastSeen:  now,
		Status:    store.SessionStatusActive,
	}
	_ = ms.UpsertSession(ctx, sess)
}

func TestListAgentsReturnsGaugeRows(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rid := "r-1"
	seedRosterEntry(t, ms, "r-1", "sess-1", "@scout", "ops", "teammate")
	seedGaugeRow(t, gs, "host-a", "sess-1", "@scout", "claude_code", "opus", "ok", &rid, 25.0)

	result, callErr := call(t, ms, gs, "health_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_listAgents: %v", callErr)
	}

	var views []struct {
		RosterID       *string `json:"roster_id"`
		AgentName      string  `json:"agent_name"`
		Liveness       string  `json:"liveness"`
		ContextFillPct float64 `json:"context_fill_pct"`
		TeamName       string  `json:"team_name"`
	}
	parseResult(t, result, &views)

	if len(views) == 0 {
		t.Fatal("expected at least 1 agent")
	}
	v := views[0]
	if v.RosterID == nil || *v.RosterID != "r-1" {
		t.Fatalf("roster_id = %v, want r-1", v.RosterID)
	}
	if v.AgentName != "@scout" {
		t.Fatalf("agent_name = %s, want @scout", v.AgentName)
	}
	if v.TeamName != "ops" {
		t.Fatalf("team_name = %s, want ops", v.TeamName)
	}
	if v.ContextFillPct != 25.0 {
		t.Fatalf("context_fill_pct = %.1f, want 25.0", v.ContextFillPct)
	}
}

// TestListAgentsSurfacesLastActivityTag confirms the health API exposes
// last_activity_tag from gauge.GaugeRow.LastActivityTool — see that field's
// doc comment: it's misleadingly named "tool" but has held the display's
// color tag (READ/EDIT/GOAL/...) since server.go's activityFromData wiring.
// This is what lets ctop color an idle agent's fallback activity text by its
// canonical tag instead of dim grey.
func TestListAgentsSurfacesLastActivityTag(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()
	t.Setenv("HOME", t.TempDir())

	ctx := context.Background()
	now := time.Now().UTC()
	if err := gs.Upsert(ctx, gauge.GaugeRow{
		Host:                "host-a",
		SessionID:           "sess-1",
		AgentName:           "@scout",
		Runtime:             "claude_code",
		CollectorStatus:     "fresh",
		UpdatedAt:           now,
		LastActivityTs:      &now,
		LastActivityDisplay: "editing __foo.go__",
		LastActivityTool:    "EDIT",
	}); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}

	result, callErr := call(t, ms, gs, "health_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_listAgents: %v", callErr)
	}

	var views []struct {
		AgentName           string `json:"agent_name"`
		LastActivityDisplay string `json:"last_activity_display"`
		LastActivityTag     string `json:"last_activity_tag"`
	}
	parseResult(t, result, &views)

	if len(views) == 0 {
		t.Fatal("expected at least 1 agent")
	}
	v := views[0]
	if v.LastActivityDisplay != "editing __foo.go__" {
		t.Fatalf("last_activity_display = %q, want %q", v.LastActivityDisplay, "editing __foo.go__")
	}
	if v.LastActivityTag != "EDIT" {
		t.Fatalf("last_activity_tag = %q, want %q", v.LastActivityTag, "EDIT")
	}
}

// TestListAgentsSurfacesIsProcessing confirms is_processing is sourced from
// the caller-supplied TurnStateLookup (hookd's in-memory turnStateTracker in
// production) rather than anything stored in the gauge row or roster entry.
func TestListAgentsSurfacesIsProcessing(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()
	t.Setenv("HOME", t.TempDir())

	rid := "r-1"
	seedRosterEntry(t, ms, rid, "sess-1", "@scout", "ops", "teammate")
	seedGaugeRow(t, gs, "host-a", "sess-1", "@scout", "claude_code", "opus", "ok", &rid, 25.0)

	turnLookup := func(sessionID, agentName string) bool {
		return sessionID == "sess-1" && agentName == "@scout"
	}

	params := map[string]interface{}{
		"name":      "health_listAgents",
		"arguments": map[string]interface{}{},
	}
	raw, _ := json.Marshal(params)
	result, callErr := mcphealth.HandleToolCall(ms, gs, turnLookup, raw)
	if callErr != nil {
		t.Fatalf("health_listAgents: %v", callErr)
	}

	var views []struct {
		AgentName    string `json:"agent_name"`
		IsProcessing bool   `json:"is_processing"`
	}
	parseResult(t, result, &views)

	if len(views) != 1 || !views[0].IsProcessing {
		t.Fatalf("views = %+v, want 1 view with is_processing=true", views)
	}
}

// TestListAgentsSurfacesSessionCost confirms session_cost_usd (statusLine's
// cost.total_cost_usd, written via POST /context) flows through to the
// agent-facing view — not just stored, but visible to callers.
func TestListAgentsSurfacesSessionCost(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rid := "r-cost"
	seedRosterEntry(t, ms, rid, "sess-cost", "@lead", "ops", "lead")
	now := time.Now().UTC()
	if err := gs.Upsert(context.Background(), gauge.GaugeRow{
		Host:            "host-a",
		SessionID:       "sess-cost",
		AgentName:       "@lead",
		RosterID:        &rid,
		Runtime:         "claude_code",
		Model:           "opus",
		PressureLevel:   "ok",
		CollectorStatus: "fresh",
		SessionCostUSD:  12.34,
		UpdatedAt:       now,
		LastActivityTs:  &now,
	}); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}

	result, callErr := call(t, ms, gs, "health_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_listAgents: %v", callErr)
	}

	var views []struct {
		AgentName      string  `json:"agent_name"`
		SessionCostUSD float64 `json:"session_cost_usd"`
	}
	parseResult(t, result, &views)

	if len(views) != 1 || views[0].SessionCostUSD != 12.34 {
		t.Fatalf("views = %+v, want 1 view with session_cost_usd=12.34", views)
	}
}

func TestGetAgentSnapshot(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rid := "r-snap"
	seedRosterEntry(t, ms, rid, "sess-snap", "@lead", "team-x", "lead")
	ctx := context.Background()
	now := time.Now().UTC()
	comp := `{"system":10000,"user":5000}`
	if err := gs.Upsert(ctx, gauge.GaugeRow{
		Host:                "host-a",
		SessionID:           "sess-snap",
		AgentName:           "@lead",
		RosterID:            &rid,
		Runtime:             "claude_code",
		Model:               "opus",
		ContextWindowTokens: 200000,
		ContextTokensUsed:   80000,
		ContextTokensFree:   120000,
		ContextFillPct:      40.0,
		LongContextActive:   true,
		PressureLevel:       "warning",
		CollectorStatus:     "fresh",
		CompositionJSON:     &comp,
		TokensInTotal:       50000,
		TokensOutTotal:      30000,
		UpdatedAt:           now,
	}); err != nil {
		t.Fatal(err)
	}

	result, callErr := call(t, ms, gs, "health_getAgentSnapshot", map[string]interface{}{
		"roster_id": rid,
	})
	if callErr != nil {
		t.Fatalf("health_getAgentSnapshot: %v", callErr)
	}

	var snap struct {
		ContextWindowTokens int64   `json:"context_window_tokens"`
		ContextTokensUsed   int64   `json:"context_tokens_used"`
		ContextFillPct      float64 `json:"context_fill_pct"`
		LongContextActive   bool    `json:"long_context_active"`
		PressureLevel       string  `json:"pressure_level"`
		CompositionJSON     *string `json:"composition_json"`
	}
	parseResult(t, result, &snap)

	if snap.ContextWindowTokens != 200000 {
		t.Fatalf("context_window_tokens = %d, want 200000", snap.ContextWindowTokens)
	}
	if snap.ContextTokensUsed != 80000 {
		t.Fatalf("context_tokens_used = %d, want 80000", snap.ContextTokensUsed)
	}
	if !snap.LongContextActive {
		t.Fatal("expected long_context_active=true")
	}
	if snap.PressureLevel != "warning" {
		t.Fatalf("pressure_level = %s, want warning", snap.PressureLevel)
	}
	if snap.CompositionJSON == nil || *snap.CompositionJSON != comp {
		t.Fatal("composition_json mismatch")
	}
}

func TestGetAgentSnapshotNotFound(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	_, callErr := call(t, ms, gs, "health_getAgentSnapshot", map[string]interface{}{
		"roster_id": "nonexistent",
	})
	if callErr == nil {
		t.Fatal("expected NOT_FOUND")
	}
	if callErr.Reason != "NOT_FOUND" {
		t.Fatalf("reason = %s, want NOT_FOUND", callErr.Reason)
	}
}

func TestGetPressureAlerts(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	r1 := "r-ok"
	r2 := "r-warn"
	r3 := "r-crit"
	seedGaugeRow(t, gs, "host-a", "s1", "", "claude_code", "opus", "ok", &r1, 10.0)
	seedGaugeRow(t, gs, "host-a", "s2", "@peer", "codex", "o3-pro", "warning", &r2, 75.0)
	seedGaugeRow(t, gs, "host-b", "s3", "", "claude_code", "opus", "critical", &r3, 95.0)

	result, callErr := call(t, ms, gs, "health_getPressureAlerts", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_getPressureAlerts: %v", callErr)
	}

	var alerts []struct {
		PressureLevel string `json:"pressure_level"`
		AgentName     string `json:"agent_name"`
	}
	parseResult(t, result, &alerts)

	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(alerts))
	}

	levels := make(map[string]bool)
	for _, a := range alerts {
		levels[a.PressureLevel] = true
	}
	if !levels["warning"] || !levels["critical"] {
		t.Fatalf("expected warning and critical, got %v", levels)
	}
}

func TestGetTeamSummary(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	r1 := "r-t1"
	r2 := "r-t2"
	seedRosterEntry(t, ms, "r-t1", "s-t1", "@a", "alpha", "teammate")
	seedRosterEntry(t, ms, "r-t2", "s-t2", "@b", "alpha", "teammate")
	seedGaugeRow(t, gs, "host-a", "s-t1", "@a", "claude_code", "opus", "ok", &r1, 30.0)
	seedGaugeRow(t, gs, "host-a", "s-t2", "@b", "claude_code", "sonnet", "warning", &r2, 70.0)

	result, callErr := call(t, ms, gs, "health_getTeamSummary", map[string]interface{}{
		"team_name": "alpha",
	})
	if callErr != nil {
		t.Fatalf("health_getTeamSummary: %v", callErr)
	}

	var summary struct {
		TeamName       string  `json:"team_name"`
		AgentCount     int     `json:"agent_count"`
		AvgContextFill float64 `json:"avg_context_fill_pct"`
		WarningCount   int     `json:"warning_count"`
	}
	parseResult(t, result, &summary)

	if summary.TeamName != "alpha" {
		t.Fatalf("team_name = %s, want alpha", summary.TeamName)
	}
	if summary.AgentCount != 2 {
		t.Fatalf("agent_count = %d, want 2", summary.AgentCount)
	}
	if summary.AvgContextFill != 50.0 {
		t.Fatalf("avg_context_fill = %.1f, want 50.0", summary.AvgContextFill)
	}
	if summary.WarningCount != 1 {
		t.Fatalf("warning_count = %d, want 1", summary.WarningCount)
	}
}

func TestUnknownTool(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	_, callErr := call(t, ms, gs, "nonexistent_tool", map[string]interface{}{})
	if callErr == nil {
		t.Fatal("expected error")
	}
	if callErr.Code != -32601 {
		t.Fatalf("expected code -32601, got %d", callErr.Code)
	}
}

func TestListAgentsScopedToCallerTeam(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rA1 := "r-a1"
	rA2 := "r-a2"
	rB1 := "r-b1"
	seedRosterEntry(t, ms, rA1, "sess-alpha", "@a1", "alpha", "teammate")
	seedRosterEntry(t, ms, rA2, "sess-alpha", "@a2", "alpha", "teammate")
	seedRosterEntry(t, ms, rB1, "sess-beta", "@b1", "beta", "lead")
	seedGaugeRow(t, gs, "host-a", "sess-alpha", "@a1", "claude_code", "opus", "ok", &rA1, 10.0)
	seedGaugeRow(t, gs, "host-a", "sess-alpha", "@a2", "claude_code", "opus", "ok", &rA2, 20.0)
	seedGaugeRow(t, gs, "host-b", "sess-beta", "@b1", "claude_code", "opus", "ok", &rB1, 30.0)

	result, callErr := callAs(t, ms, gs, "sess-alpha", "@a1", "health_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_listAgents: %v", callErr)
	}

	var views []struct {
		AgentName string `json:"agent_name"`
		TeamName  string `json:"team_name"`
	}
	parseResult(t, result, &views)

	if len(views) != 2 {
		t.Fatalf("expected 2 agents scoped to team alpha, got %d: %+v", len(views), views)
	}
	for _, v := range views {
		if v.TeamName != "alpha" {
			t.Fatalf("leaked non-alpha agent into scoped list: %+v", v)
		}
	}
}

func TestGetTeamSummaryRejectsOtherTeam(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	seedRosterEntry(t, ms, "r-alpha", "sess-alpha", "@a", "alpha", "lead")

	_, callErr := callAs(t, ms, gs, "sess-alpha", "@a", "health_getTeamSummary", map[string]interface{}{
		"team_name": "beta",
	})
	if callErr == nil {
		t.Fatal("expected error requesting another team's summary, got none")
	}
	if callErr.Reason != "FORBIDDEN" {
		t.Fatalf("reason = %s, want FORBIDDEN", callErr.Reason)
	}
}

func TestGetTeamSummaryDefaultsToCallerTeam(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	r1 := "r-t1"
	seedRosterEntry(t, ms, r1, "sess-alpha", "@a", "alpha", "lead")
	seedGaugeRow(t, gs, "host-a", "sess-alpha", "@a", "claude_code", "opus", "ok", &r1, 40.0)

	result, callErr := callAs(t, ms, gs, "sess-alpha", "@a", "health_getTeamSummary", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_getTeamSummary: %v", callErr)
	}

	var summary struct {
		TeamName string `json:"team_name"`
	}
	parseResult(t, result, &summary)
	if summary.TeamName != "alpha" {
		t.Fatalf("team_name = %s, want alpha (defaulted from caller)", summary.TeamName)
	}
}

func TestGetPressureAlertsScopedToCallerTeam(t *testing.T) {
	ms := openMainStore(t)
	gs := newMemGaugeStore()

	rA := "r-a-warn"
	rB := "r-b-crit"
	seedRosterEntry(t, ms, rA, "sess-alpha", "@a", "alpha", "teammate")
	seedRosterEntry(t, ms, rB, "sess-beta", "@b", "beta", "teammate")
	seedGaugeRow(t, gs, "host-a", "sess-alpha", "@a", "claude_code", "opus", "warning", &rA, 75.0)
	seedGaugeRow(t, gs, "host-b", "sess-beta", "@b", "claude_code", "opus", "critical", &rB, 95.0)

	result, callErr := callAs(t, ms, gs, "sess-alpha", "@a", "health_getPressureAlerts", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("health_getPressureAlerts: %v", callErr)
	}

	var alerts []struct {
		AgentName string `json:"agent_name"`
	}
	parseResult(t, result, &alerts)

	if len(alerts) != 1 || alerts[0].AgentName != "@a" {
		t.Fatalf("expected only @a's alert visible, got %+v", alerts)
	}
}

// --- helpers ---

func parseResult(t *testing.T, result mcphealth.Result, dest interface{}) {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("empty result content")
	}
	text, ok := result.Content[0]["text"].(string)
	if !ok {
		t.Fatal("result content[0] has no text field")
	}
	if err := json.Unmarshal([]byte(text), dest); err != nil {
		t.Fatalf("unmarshal result: %v (text: %s)", err, text)
	}
}
