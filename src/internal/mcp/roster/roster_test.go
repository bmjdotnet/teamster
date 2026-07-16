package roster_test

import (
	"context"
	"encoding/json"
	"testing"

	mcproster "github.com/bmjdotnet/teamster/internal/mcp/roster"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// call dispatches a tools/call with no _meta (anonymous caller). Points HOME
// at an isolated empty temp dir first — this host's own CLAUDE.md notes it
// constantly has a real ~/.claude/current-session-id from an active Claude
// session, which would otherwise leak into resolveSessionID's fallback and
// make caller-team-scoping non-deterministic in tests. See
// internal/mcp/wms/identity_test.go for the same pattern.
func call(t *testing.T, s store.Store, toolName string, args map[string]interface{}) (mcproster.Result, *mcproster.CallError) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	params := map[string]interface{}{
		"name":      toolName,
		"arguments": args,
	}
	raw, _ := json.Marshal(params)
	return mcproster.HandleToolCall(s, raw)
}

// callAs dispatches a tools/call with an explicit _meta identity, so tests
// can exercise caller-team-scoping deterministically.
func callAs(t *testing.T, s store.Store, sessionID, agentType, toolName string, args map[string]interface{}) (mcproster.Result, *mcproster.CallError) {
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
	return mcproster.HandleToolCall(s, raw)
}

func TestRegisterAndVerify(t *testing.T) {
	s := openTestStore(t)

	// Register a peer.
	result, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@scout",
		"runtime":      "claude_code",
		"model":        "sonnet",
		"host":         "hub01",
		"relationship": "teammate",
		"session_id":   "sess-1",
	})
	if callErr != nil {
		t.Fatalf("registerPeer: %v", callErr)
	}

	var reg struct {
		RosterID    string `json:"roster_id"`
		BearerToken string `json:"bearer_token"`
	}
	parseResult(t, result, &reg)
	if reg.RosterID == "" || reg.BearerToken == "" {
		t.Fatal("expected non-empty roster_id and bearer_token")
	}

	// Verify the token.
	result, callErr = call(t, s, "verifyToken", map[string]interface{}{
		"token": reg.BearerToken,
	})
	if callErr != nil {
		t.Fatalf("verifyToken: %v", callErr)
	}

	var verify struct {
		Valid        bool    `json:"valid"`
		RosterID     string  `json:"roster_id"`
		SessionID    *string `json:"session_id"`
		AgentName    string  `json:"agent_name"`
		Relationship string  `json:"relationship"`
	}
	parseResult(t, result, &verify)
	if !verify.Valid {
		t.Fatal("expected valid=true")
	}
	if verify.RosterID != reg.RosterID {
		t.Fatalf("roster_id mismatch: %s vs %s", verify.RosterID, reg.RosterID)
	}
	if verify.SessionID == nil || *verify.SessionID != "sess-1" {
		t.Fatalf("session_id = %v, want sess-1", verify.SessionID)
	}
}

func TestVerifyGarbageToken(t *testing.T) {
	s := openTestStore(t)

	result, callErr := call(t, s, "verifyToken", map[string]interface{}{
		"token": "garbage-token-value",
	})
	if callErr != nil {
		t.Fatalf("unexpected call error: %v", callErr)
	}

	var verify struct {
		Valid bool `json:"valid"`
	}
	parseResult(t, result, &verify)
	if verify.Valid {
		t.Fatal("expected valid=false for garbage token")
	}
}

func TestResolveID(t *testing.T) {
	s := openTestStore(t)

	result, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@worker",
		"runtime":      "codex",
		"relationship": "peer",
		"session_id":   "sess-resolve",
	})
	if callErr != nil {
		t.Fatalf("registerPeer: %v", callErr)
	}
	var reg struct {
		RosterID string `json:"roster_id"`
	}
	parseResult(t, result, &reg)

	result, callErr = call(t, s, "roster_resolveId", map[string]interface{}{
		"session_id": "sess-resolve",
		"agent_name": "@worker",
	})
	if callErr != nil {
		t.Fatalf("roster_resolveId: %v", callErr)
	}
	var resolve struct {
		RosterID string `json:"roster_id"`
	}
	parseResult(t, result, &resolve)
	if resolve.RosterID != reg.RosterID {
		t.Fatalf("resolved roster_id = %s, want %s", resolve.RosterID, reg.RosterID)
	}
}

func TestListAgentsDefaultScope(t *testing.T) {
	s := openTestStore(t)

	// Register a bound peer (will be live/idle/stale depending on timing).
	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@live",
		"runtime":      "claude_code",
		"relationship": "teammate",
		"session_id":   "sess-live",
	})

	// Register an unbound peer.
	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@unbound",
		"runtime":      "codex",
		"relationship": "peer",
	})

	result, callErr := call(t, s, "roster_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("roster_listAgents: %v", callErr)
	}

	var views []struct {
		AgentName string `json:"agent_name"`
		Liveness  string `json:"liveness"`
	}
	parseResult(t, result, &views)

	if len(views) < 2 {
		t.Fatalf("expected at least 2 agents, got %d", len(views))
	}

	// Both should be in the default scope (unbound is included).
	names := make(map[string]string)
	for _, v := range views {
		names[v.AgentName] = v.Liveness
	}
	if _, ok := names["@unbound"]; !ok {
		t.Fatal("unbound agent should be in default scope")
	}
}

func TestListAgentsLivenessFilter(t *testing.T) {
	s := openTestStore(t)

	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@peer",
		"runtime":      "codex",
		"relationship": "peer",
	})

	// Filter for only "unbound".
	result, callErr := call(t, s, "roster_listAgents", map[string]interface{}{
		"liveness": "unbound",
	})
	if callErr != nil {
		t.Fatalf("roster_listAgents: %v", callErr)
	}

	var views []struct {
		Liveness string `json:"liveness"`
	}
	parseResult(t, result, &views)
	for _, v := range views {
		if v.Liveness != "unbound" {
			t.Fatalf("expected only unbound, got %s", v.Liveness)
		}
	}

	// Filter for array of liveness values.
	result, callErr = call(t, s, "roster_listAgents", map[string]interface{}{
		"liveness": []interface{}{"unbound", "stale"},
	})
	if callErr != nil {
		t.Fatalf("roster_listAgents array filter: %v", callErr)
	}
	parseResult(t, result, &views)
	for _, v := range views {
		if v.Liveness != "unbound" && v.Liveness != "stale" {
			t.Fatalf("expected unbound or stale, got %s", v.Liveness)
		}
	}
}

func TestBindSession(t *testing.T) {
	s := openTestStore(t)

	result, _ := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@binder",
		"runtime":      "codex",
		"relationship": "peer",
	})
	var reg struct {
		RosterID string `json:"roster_id"`
	}
	parseResult(t, result, &reg)

	result, callErr := call(t, s, "roster_bindSession", map[string]interface{}{
		"roster_id":  reg.RosterID,
		"session_id": "codex-sess-1",
	})
	if callErr != nil {
		t.Fatalf("roster_bindSession: %v", callErr)
	}

	var bind struct {
		RosterID string `json:"roster_id"`
		Bound    bool   `json:"bound"`
	}
	parseResult(t, result, &bind)
	if !bind.Bound {
		t.Fatal("expected bound=true")
	}

	// Rebind with different session_id should fail.
	_, callErr = call(t, s, "roster_bindSession", map[string]interface{}{
		"roster_id":  reg.RosterID,
		"session_id": "different-sess",
	})
	if callErr == nil {
		t.Fatal("expected error on rebind with different session_id")
	}
	if callErr.Reason != "CONFLICT" {
		t.Fatalf("expected CONFLICT reason, got %s", callErr.Reason)
	}
}

func TestGetRosterEntry(t *testing.T) {
	s := openTestStore(t)

	result, _ := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@entry",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-entry",
		"bus_team":     "team-alpha",
	})
	var reg struct {
		RosterID string `json:"roster_id"`
	}
	parseResult(t, result, &reg)

	// Get by roster_id.
	result, callErr := call(t, s, "getRosterEntry", map[string]interface{}{
		"roster_id": reg.RosterID,
	})
	if callErr != nil {
		t.Fatalf("getRosterEntry by id: %v", callErr)
	}
	var view struct {
		RosterID string `json:"roster_id"`
	}
	parseResult(t, result, &view)
	if view.RosterID != reg.RosterID {
		t.Fatalf("roster_id = %s, want %s", view.RosterID, reg.RosterID)
	}

	// Get by bus_team.
	result, callErr = call(t, s, "getRosterEntry", map[string]interface{}{
		"bus_team": "team-alpha",
	})
	if callErr != nil {
		t.Fatalf("getRosterEntry by bus_team: %v", callErr)
	}
	var views []struct {
		BusTeam string `json:"bus_team"`
	}
	parseResult(t, result, &views)
	if len(views) == 0 {
		t.Fatal("expected at least 1 entry for bus_team")
	}
}

func TestRegisterPeerValidation(t *testing.T) {
	s := openTestStore(t)

	// Missing agent_name.
	_, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"runtime":      "codex",
		"relationship": "peer",
	})
	if callErr == nil {
		t.Fatal("expected validation error for missing agent_name")
	}
	if callErr.Reason != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %s", callErr.Reason)
	}

	// Missing runtime.
	_, callErr = call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@test",
		"relationship": "peer",
	})
	if callErr == nil {
		t.Fatal("expected validation error for missing runtime")
	}

	// Bad parent_ref.
	_, callErr = call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@test",
		"runtime":      "codex",
		"relationship": "peer",
		"parent_ref":   "nonexistent-id",
	})
	if callErr == nil {
		t.Fatal("expected error for bad parent_ref")
	}
	if callErr.Reason != "PARENT_NOT_FOUND" {
		t.Fatalf("expected PARENT_NOT_FOUND, got %s", callErr.Reason)
	}
}

func TestUnknownTool(t *testing.T) {
	s := openTestStore(t)

	_, callErr := call(t, s, "nonexistent_tool", map[string]interface{}{})
	if callErr == nil {
		t.Fatal("expected error for unknown tool")
	}
	if callErr.Code != -32601 {
		t.Fatalf("expected code -32601, got %d", callErr.Code)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	s := openTestStore(t)

	_, callErr := call(t, s, "roster_getAgent", map[string]interface{}{
		"roster_id": "nonexistent",
	})
	if callErr == nil {
		t.Fatal("expected error for nonexistent roster_id")
	}
	if callErr.Reason != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND, got %s", callErr.Reason)
	}
}

func TestListAgentsScopedToCallerTeam(t *testing.T) {
	s := openTestStore(t)

	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@alpha-lead",
		"runtime":      "claude_code",
		"relationship": "lead",
		"team_name":    "alpha",
		"session_id":   "sess-alpha",
	})
	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@alpha-mate",
		"runtime":      "claude_code",
		"relationship": "teammate",
		"team_name":    "alpha",
		"session_id":   "sess-alpha",
	})
	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@beta-lead",
		"runtime":      "claude_code",
		"relationship": "lead",
		"team_name":    "beta",
		"session_id":   "sess-beta",
	})

	// Caller is @alpha-lead on sess-alpha — should see only team alpha.
	result, callErr := callAs(t, s, "sess-alpha", "@alpha-lead", "roster_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("roster_listAgents: %v", callErr)
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

func TestListAgentsScopedToCallerSessionWhenNoTeam(t *testing.T) {
	s := openTestStore(t)

	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@solo",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-solo",
	})
	call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@other",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-other",
	})

	// Caller has no team_name (never called registerPeer with one) — falls
	// back to session-scoping, so only its own session's agent is visible.
	result, callErr := callAs(t, s, "sess-solo", "@solo", "roster_listAgents", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("roster_listAgents: %v", callErr)
	}

	var views []struct {
		AgentName string `json:"agent_name"`
	}
	parseResult(t, result, &views)

	if len(views) != 1 || views[0].AgentName != "@solo" {
		t.Fatalf("expected only @solo in solo scope, got %+v", views)
	}
}

// TestRegisterPeerPropagatesTeamNameToSession confirms registerPeer's
// team_name propagates onto the underlying sessions row (sessions.team_name
// is unscoped by agent_name — see store.SetSessionTeam) so that auto-
// registered teammates joining later inherit it (internal/server's
// dispatchObservability early-upsert reads the lead's session row).
func TestRegisterPeerPropagatesTeamNameToSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed the lead's session row as hookd's auto-registration would have
	// already created it before /teamster:bootstrap calls registerPeer.
	// SetSessionTeam is UPDATE-only (see store.SetSessionTeam), so there
	// must be an existing row for propagation to have anything to update.
	if err := s.CreateSession(ctx, store.Session{
		SessionID: "sess-1",
		AgentName: "",
		Host:      "host-a",
		Status:    store.SessionStatusActive,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	_, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@lead",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-1",
		"team_name":    "ops",
	})
	if callErr != nil {
		t.Fatalf("registerPeer: %v", callErr)
	}

	sess, err := s.GetSession(ctx, store.SessionKey{SessionID: "sess-1", AgentName: ""})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.TeamName != "ops" {
		t.Fatalf("session team_name = %q, want ops", sess.TeamName)
	}
}

// TestRegisterPeerPropagatesTeamNameToSiblingRosterEntries confirms that
// registering a peer with a team_name updates every other agent_roster entry
// already bound to the same session_id — covering the ordering where a
// teammate auto-registered (empty team_name) before the lead's
// /teamster:bootstrap call named the team.
func TestRegisterPeerPropagatesTeamNameToSiblingRosterEntries(t *testing.T) {
	s := openTestStore(t)

	result, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@scout",
		"runtime":      "claude_code",
		"relationship": "teammate",
		"session_id":   "sess-1",
	})
	if callErr != nil {
		t.Fatalf("registerPeer @scout: %v", callErr)
	}
	var scoutReg struct {
		RosterID string `json:"roster_id"`
	}
	parseResult(t, result, &scoutReg)

	_, callErr = call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@lead",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-1",
		"team_name":    "ops",
	})
	if callErr != nil {
		t.Fatalf("registerPeer lead: %v", callErr)
	}

	scoutEntry, err := s.GetRosterEntry(context.Background(), scoutReg.RosterID)
	if err != nil {
		t.Fatalf("GetRosterEntry: %v", err)
	}
	if scoutEntry.TeamName != "ops" {
		t.Fatalf("@scout roster team_name = %q, want ops (propagated from lead's registerPeer)", scoutEntry.TeamName)
	}
}

// TestRegisterPeerDedupsExistingSessionAgent confirms that calling
// registerPeer for a (session_id, agent_name) pair that hookd already
// auto-registered updates the existing roster entry in place — rather than
// creating a second entry — and still mints a fresh bearer token.
func TestRegisterPeerDedupsExistingSessionAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Simulate hookd's auto-registration of the lead (empty agent_name) on
	// the first hook event, before /teamster:bootstrap runs.
	autoResult, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-dedup",
	})
	if callErr != nil {
		t.Fatalf("registerPeer (auto): %v", callErr)
	}
	var autoReg struct {
		RosterID    string `json:"roster_id"`
		BearerToken string `json:"bearer_token"`
	}
	parseResult(t, autoResult, &autoReg)

	// Bootstrap now calls registerPeer with the same session_id + agent_name
	// to name the team.
	bootResult, callErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "",
		"runtime":      "claude_code",
		"relationship": "lead",
		"session_id":   "sess-dedup",
		"team_name":    "dedup-team",
	})
	if callErr != nil {
		t.Fatalf("registerPeer (bootstrap): %v", callErr)
	}
	var bootReg struct {
		RosterID    string `json:"roster_id"`
		BearerToken string `json:"bearer_token"`
	}
	parseResult(t, bootResult, &bootReg)

	if bootReg.RosterID != autoReg.RosterID {
		t.Fatalf("expected dedup to reuse roster_id %q, got new roster_id %q", autoReg.RosterID, bootReg.RosterID)
	}
	if bootReg.BearerToken == "" || bootReg.BearerToken == autoReg.BearerToken {
		t.Fatalf("expected a fresh non-empty bearer token, got %q (auto was %q)", bootReg.BearerToken, autoReg.BearerToken)
	}

	entries, err := s.ListRosterEntries(ctx, store.RosterFilter{})
	if err != nil {
		t.Fatalf("ListRosterEntries: %v", err)
	}
	var matches int
	for _, e := range entries {
		if e.SessionID != nil && *e.SessionID == "sess-dedup" && e.AgentName == "" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 roster entry for (sess-dedup, \"\"), got %d", matches)
	}

	entry, err := s.GetRosterEntry(ctx, bootReg.RosterID)
	if err != nil {
		t.Fatalf("GetRosterEntry: %v", err)
	}
	if entry.TeamName != "dedup-team" {
		t.Fatalf("entry.TeamName = %q, want dedup-team", entry.TeamName)
	}
}

// --- helpers ---

func parseResult(t *testing.T, result mcproster.Result, dest interface{}) {
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
