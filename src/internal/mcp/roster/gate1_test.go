package roster_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	rosterutil "github.com/bmjdotnet/teamster/internal/roster"
	"github.com/bmjdotnet/teamster/internal/store"
)

// TestGate1Demo exercises the full Gate 1 acceptance flow end-to-end:
// register lead + teammate + codex, list all three, verify/reject tokens,
// and prove the unbound→bound spawn-time flow.
//
// Registration flows match reality:
//   - Claude lead + teammate use direct store upsert (the dispatchObservability
//     implicit path — no registerPeer MCP call for these).
//   - Codex main session uses direct store upsert (the POST /session path).
//   - Spawn-time peer uses registerPeer MCP tool (the union-hall flow).
//
// All roster queries and token operations go through the MCP handler layer.
func TestGate1Demo(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedSession := func(sessionID, agentName, host, runtime string) {
		t.Helper()
		if err := s.UpsertSession(ctx, store.Session{
			SessionID: sessionID,
			AgentName: agentName,
			Host:      host,
			Status:    store.SessionStatusActive,
			Runtime:   runtime,
		}); err != nil {
			t.Fatalf("seed session %s/%s: %v", sessionID, agentName, err)
		}
	}

	// ---- Step 1: Register a Claude lead (implicit self-registration) ----

	leadRosterID := rosterutil.GenerateRosterID()
	leadSID := "lead-session-1"
	leadBoundAt := now
	if err := s.UpsertRosterEntry(ctx, store.RosterEntry{
		RosterID:     leadRosterID,
		SessionID:    &leadSID,
		AgentName:    "",
		Host:         "hub01",
		Runtime:      "claude_code",
		Relationship: "lead",
		CreatedAt:    now,
		BoundAt:      &leadBoundAt,
	}); err != nil {
		t.Fatalf("step 1 — upsert lead roster: %v", err)
	}
	leadToken, err := rosterutil.MintToken(ctx, s, leadRosterID)
	if err != nil {
		t.Fatalf("step 1 — mint lead token: %v", err)
	}
	seedSession("lead-session-1", "", "hub01", "claude_code")
	t.Logf("step 1 PASS: lead registered roster_id=%s", leadRosterID)

	// ---- Step 2: Register its teammate (implicit self-registration) ----

	tmRosterID := rosterutil.GenerateRosterID()
	tmBoundAt := now
	if err := s.UpsertRosterEntry(ctx, store.RosterEntry{
		RosterID:     tmRosterID,
		SessionID:    &leadSID,
		AgentName:    "@roster",
		Host:         "hub01",
		Runtime:      "claude_code",
		Relationship: "teammate",
		ParentRef:    &leadRosterID,
		CreatedAt:    now,
		BoundAt:      &tmBoundAt,
	}); err != nil {
		t.Fatalf("step 2 — upsert teammate roster: %v", err)
	}
	seedSession("lead-session-1", "@roster", "hub01", "claude_code")

	// Verify parent_ref via MCP getAgent.
	tmDetail, tmDetailErr := call(t, s, "roster_getAgent", map[string]interface{}{
		"roster_id": tmRosterID,
	})
	if tmDetailErr != nil {
		t.Fatalf("step 2 — roster_getAgent: %v", tmDetailErr)
	}
	var tmView struct {
		ParentRef    *string `json:"parent_ref"`
		Relationship string  `json:"relationship"`
	}
	parseResult(t, tmDetail, &tmView)
	if tmView.ParentRef == nil || *tmView.ParentRef != leadRosterID {
		t.Fatalf("step 2 — parent_ref = %v, want %s", tmView.ParentRef, leadRosterID)
	}
	if tmView.Relationship != "teammate" {
		t.Fatalf("step 2 — relationship = %q, want teammate", tmView.Relationship)
	}
	t.Logf("step 2 PASS: teammate registered roster_id=%s parent_ref=%s", tmRosterID, leadRosterID)

	// ---- Step 3: Register a Codex main session (POST /session path) ----

	codexRosterID := rosterutil.GenerateRosterID()
	codexSID := "codex-session-1"
	codexBoundAt := now
	if err := s.UpsertRosterEntry(ctx, store.RosterEntry{
		RosterID:     codexRosterID,
		SessionID:    &codexSID,
		AgentName:    "",
		Host:         "hub01",
		Runtime:      "codex",
		Relationship: "lead",
		CreatedAt:    now,
		BoundAt:      &codexBoundAt,
	}); err != nil {
		t.Fatalf("step 3 — upsert codex roster: %v", err)
	}
	seedSession("codex-session-1", "", "hub01", "codex")
	t.Logf("step 3 PASS: codex registered roster_id=%s", codexRosterID)

	// ---- Step 4: roster_listAgents shows all three ----

	listResult, listErr := call(t, s, "roster_listAgents", map[string]interface{}{})
	if listErr != nil {
		t.Fatalf("step 4 — roster_listAgents: %v", listErr)
	}

	var agents []struct {
		RosterID     string  `json:"roster_id"`
		AgentName    string  `json:"agent_name"`
		Runtime      string  `json:"runtime"`
		Relationship string  `json:"relationship"`
		ParentRef    *string `json:"parent_ref"`
		Liveness     string  `json:"liveness"`
	}
	parseResult(t, listResult, &agents)

	if len(agents) != 3 {
		raw, _ := json.MarshalIndent(agents, "", "  ")
		t.Fatalf("step 4 — expected 3 agents, got %d: %s", len(agents), raw)
	}

	type agentInfo struct {
		Runtime, Relationship, Liveness string
		ParentRef                       *string
	}
	byID := make(map[string]agentInfo)
	for _, a := range agents {
		byID[a.RosterID] = agentInfo{a.Runtime, a.Relationship, a.Liveness, a.ParentRef}
	}

	leadInfo, ok := byID[leadRosterID]
	if !ok {
		t.Fatal("step 4 — lead not found in list")
	}
	if leadInfo.Relationship != "lead" || leadInfo.Runtime != "claude_code" {
		t.Fatalf("step 4 — lead: %+v", leadInfo)
	}
	if leadInfo.Liveness != "live" {
		t.Fatalf("step 4 — lead liveness = %q, want live", leadInfo.Liveness)
	}

	tmInfo, ok := byID[tmRosterID]
	if !ok {
		t.Fatal("step 4 — teammate not found in list")
	}
	if tmInfo.Relationship != "teammate" {
		t.Fatalf("step 4 — teammate relationship = %q", tmInfo.Relationship)
	}
	if tmInfo.ParentRef == nil || *tmInfo.ParentRef != leadRosterID {
		t.Fatalf("step 4 — teammate parent_ref = %v", tmInfo.ParentRef)
	}

	codexInfo, ok := byID[codexRosterID]
	if !ok {
		t.Fatal("step 4 — codex not found in list")
	}
	if codexInfo.Relationship != "lead" || codexInfo.Runtime != "codex" {
		t.Fatalf("step 4 — codex: %+v", codexInfo)
	}
	t.Log("step 4 PASS: all 3 agents listed with correct metadata")

	// ---- Step 5: verifyToken with garbage rejects ----

	garbageResult, garbageErr := call(t, s, "verifyToken", map[string]interface{}{
		"token": "garbage-not-a-real-token",
	})
	if garbageErr != nil {
		t.Fatalf("step 5 — verifyToken: unexpected call error: %v", garbageErr)
	}
	var garbageVerify struct {
		Valid bool `json:"valid"`
	}
	parseResult(t, garbageResult, &garbageVerify)
	if garbageVerify.Valid {
		t.Fatal("step 5 — expected valid=false for garbage token")
	}
	t.Log("step 5 PASS: garbage token rejected")

	// ---- Step 6: verifyToken with real token succeeds ----

	realResult, realErr := call(t, s, "verifyToken", map[string]interface{}{
		"token": leadToken,
	})
	if realErr != nil {
		t.Fatalf("step 6 — verifyToken: %v", realErr)
	}
	var realVerify struct {
		Valid        bool    `json:"valid"`
		RosterID     string  `json:"roster_id"`
		SessionID    *string `json:"session_id"`
		Relationship string  `json:"relationship"`
	}
	parseResult(t, realResult, &realVerify)
	if !realVerify.Valid {
		t.Fatal("step 6 — expected valid=true")
	}
	if realVerify.RosterID != leadRosterID {
		t.Fatalf("step 6 — roster_id = %s, want %s", realVerify.RosterID, leadRosterID)
	}
	if realVerify.SessionID == nil || *realVerify.SessionID != "lead-session-1" {
		t.Fatalf("step 6 — session_id = %v, want lead-session-1", realVerify.SessionID)
	}
	if realVerify.Relationship != "lead" {
		t.Fatalf("step 6 — relationship = %s, want lead", realVerify.Relationship)
	}
	t.Log("step 6 PASS: real token verified with correct identity")

	// ---- Step 7: Unbound→bound flow (WP0.6 acceptance) ----

	// 7a: Register with session_id omitted via registerPeer MCP tool (spawn-time).
	spawnResult, spawnErr := call(t, s, "registerPeer", map[string]interface{}{
		"agent_name":   "@spawned-peer",
		"runtime":      "codex",
		"relationship": "peer",
		"host":         "hub01",
		"parent_ref":   leadRosterID,
	})
	if spawnErr != nil {
		t.Fatalf("step 7a — registerPeer unbound: %v", spawnErr)
	}
	var spawn struct {
		RosterID    string `json:"roster_id"`
		BearerToken string `json:"bearer_token"`
	}
	parseResult(t, spawnResult, &spawn)
	if spawn.RosterID == "" || spawn.BearerToken == "" {
		t.Fatal("step 7a — spawn roster_id or bearer_token empty")
	}

	// 7b: Verify unbound entry shows liveness=unbound.
	unboundList, unboundErr := call(t, s, "roster_listAgents", map[string]interface{}{
		"liveness": "unbound",
	})
	if unboundErr != nil {
		t.Fatalf("step 7b — roster_listAgents unbound: %v", unboundErr)
	}
	var unboundAgents []struct {
		RosterID string `json:"roster_id"`
		Liveness string `json:"liveness"`
	}
	parseResult(t, unboundList, &unboundAgents)
	found := false
	for _, a := range unboundAgents {
		if a.RosterID == spawn.RosterID {
			if a.Liveness != "unbound" {
				t.Fatalf("step 7b — liveness = %q, want unbound", a.Liveness)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("step 7b — spawned peer not in unbound list")
	}

	// 7c: Token works pre-bind (valid=true, session_id=null).
	preBindResult, preBindErr := call(t, s, "verifyToken", map[string]interface{}{
		"token": spawn.BearerToken,
	})
	if preBindErr != nil {
		t.Fatalf("step 7c — verifyToken pre-bind: %v", preBindErr)
	}
	var preBind struct {
		Valid     bool    `json:"valid"`
		SessionID *string `json:"session_id"`
	}
	parseResult(t, preBindResult, &preBind)
	if !preBind.Valid {
		t.Fatal("step 7c — expected valid=true pre-bind")
	}
	if preBind.SessionID != nil {
		t.Fatalf("step 7c — session_id should be null pre-bind, got %v", preBind.SessionID)
	}

	// 7d: Bind session.
	bindResult, bindErr := call(t, s, "roster_bindSession", map[string]interface{}{
		"roster_id":  spawn.RosterID,
		"session_id": "peer-session-99",
	})
	if bindErr != nil {
		t.Fatalf("step 7d — roster_bindSession: %v", bindErr)
	}
	var bindResp struct {
		Bound bool `json:"bound"`
	}
	parseResult(t, bindResult, &bindResp)
	if !bindResp.Bound {
		t.Fatal("step 7d — expected bound=true")
	}

	// 7e: After bind, roster_getAgent shows session_id populated, liveness != unbound.
	seedSession("peer-session-99", "@spawned-peer", "hub01", "codex")

	getResult, getErr := call(t, s, "roster_getAgent", map[string]interface{}{
		"roster_id": spawn.RosterID,
	})
	if getErr != nil {
		t.Fatalf("step 7e — roster_getAgent: %v", getErr)
	}
	var postBind struct {
		SessionID *string `json:"session_id"`
		Liveness  string  `json:"liveness"`
	}
	parseResult(t, getResult, &postBind)
	if postBind.SessionID == nil || *postBind.SessionID != "peer-session-99" {
		t.Fatalf("step 7e — session_id = %v, want peer-session-99", postBind.SessionID)
	}
	if postBind.Liveness == "unbound" {
		t.Fatal("step 7e — liveness should not be unbound after bind")
	}

	// 7f: Token now returns populated session_id.
	postBindVerify, postBindErr := call(t, s, "verifyToken", map[string]interface{}{
		"token": spawn.BearerToken,
	})
	if postBindErr != nil {
		t.Fatalf("step 7f — verifyToken post-bind: %v", postBindErr)
	}
	var postBindToken struct {
		Valid     bool    `json:"valid"`
		SessionID *string `json:"session_id"`
	}
	parseResult(t, postBindVerify, &postBindToken)
	if !postBindToken.Valid {
		t.Fatal("step 7f — expected valid=true post-bind")
	}
	if postBindToken.SessionID == nil || *postBindToken.SessionID != "peer-session-99" {
		t.Fatalf("step 7f — session_id = %v, want peer-session-99", postBindToken.SessionID)
	}

	// 7g: Rejected rebind with different session_id.
	_, rebindErr := call(t, s, "roster_bindSession", map[string]interface{}{
		"roster_id":  spawn.RosterID,
		"session_id": "different-session",
	})
	if rebindErr == nil {
		t.Fatal("step 7g — expected CONFLICT on rebind")
	}
	if rebindErr.Reason != "CONFLICT" {
		t.Fatalf("step 7g — reason = %s, want CONFLICT", rebindErr.Reason)
	}
	t.Log("step 7 PASS: unbound→bound flow complete")
}
