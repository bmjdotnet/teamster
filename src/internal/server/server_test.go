package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Bug #10 regression: hookd must dispatch on the MCP wire-form tool name
// (mcp__wms__wms_createOutcome), not the bare internal form. Before the fix,
// the bridge gauge series populated session_id + host but emitted with empty
// outcome_id / workunit_id labels even when an interactive Claude session
// created WMS entities via the wms MCP.
func TestDispatchObservability_MCPWMS_PopulatesBridgeLabels(t *testing.T) {
	cases := []struct {
		name      string
		toolName  string
		idValue   string
		labelName string
		wantValue string
	}{
		{"createOutcome", "mcp__wms__wms_createOutcome", "O1", "outcome_id", "O1"},
		{"createWorkUnit", "mcp__wms__wms_createWorkUnit", "U1", "workunit_id", "U1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			tracker := observability.NewSessionTracker(
				"testhost",
				5*time.Minute,
				30*time.Second,
				nil,
			)
			reg.MustRegister(observability.NewBridgeCollector(tracker))

			s := &Server{
				cfg:      config.Config{Host: "testhost"},
				sessions: tracker,
				metrics:  observability.NewMetrics(prometheus.NewRegistry()),
			}

			// Session must exist before the WMS create event so the setField
			// lookup hits — matches the real (UserPromptSubmit | Bash) → wms_*
			// ordering on the wire.
			s.dispatchObservability(hook.HookEvent{
				HookEventName: "PreToolUse",
				SessionID:     "s1",
				ToolName:      "Bash",
				ToolInput:     map[string]interface{}{"command": "true"},
			}, nil)

			s.dispatchObservability(hook.HookEvent{
				HookEventName: "PreToolUse",
				SessionID:     "s1",
				ToolName:      tc.toolName,
				ToolInput:     map[string]interface{}{"id": tc.idValue},
			}, nil)

			mfs, err := reg.Gather()
			if err != nil {
				t.Fatalf("Gather: %v", err)
			}
			var found bool
			for _, mf := range mfs {
				if mf.GetName() != "teamster_session_active" {
					continue
				}
				for _, m := range mf.Metric {
					var got, sid string
					for _, lp := range m.Label {
						switch lp.GetName() {
						case "session_id":
							sid = lp.GetValue()
						case tc.labelName:
							got = lp.GetValue()
						}
					}
					if sid != "s1" {
						continue
					}
					found = true
					if got != tc.wantValue {
						t.Errorf("teamster_session_active{%s=...}: got %q, want %q",
							tc.labelName, got, tc.wantValue)
					}
				}
			}
			if !found {
				t.Fatalf("teamster_session_active series for session_id=s1 not emitted; gathered: %s",
					mfNames(mfs))
			}
		})
	}
}

// Negative coverage: the bare internal tool name (no mcp__wms__ prefix) must
// NOT trigger label population — the bug pre-fix relied on this code path and
// silently produced empty labels. Pins the wire-form requirement so a future
// regression that swaps back to the bare names fails here.
func TestDispatchObservability_BareToolName_IsIgnored(t *testing.T) {
	tracker := observability.NewSessionTracker("testhost", 5*time.Minute, 30*time.Second, nil)
	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		sessions: tracker,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
	}

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "s1",
		ToolName:      "Bash",
		ToolInput:     map[string]interface{}{"command": "true"},
	}, nil)
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "s1",
		ToolName:      "wms_createOutcome", // bare form — Claude Code does not emit this
		ToolInput:     map[string]interface{}{"id": "O1"},
	}, nil)

	snap, ok := tracker.GetSnapshot("s1", "")
	if !ok {
		t.Fatal("session entry missing")
	}
	if snap.OutcomeID != "" {
		t.Errorf("OutcomeID populated by bare-name event: got %q, want %q", snap.OutcomeID, "")
	}
}

// TestDispatchObservability_TeammateInheritsLeadTeamName confirms the
// early-upsert roster-registration path (dispatchObservability's isNew
// branch) copies the lead's session-level team_name onto a teammate's
// auto-registered roster entry — so a teammate that joins after
// /teamster:bootstrap has named the team isn't stranded with an empty
// team_name until it separately calls registerPeer.
func TestDispatchObservability_TeammateInheritsLeadTeamName(t *testing.T) {
	obsStore, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer obsStore.Close()

	tracker := observability.NewSessionTracker("testhost", 5*time.Minute, 30*time.Second, nil)
	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		sessions: tracker,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		obsStore: obsStore,
	}
	ctx := context.Background()

	// Lead's first event: auto-registers the lead's session + roster entry
	// via a detached goroutine. Wait for the session row before naming the
	// team below, or SetSessionTeam races the insert and updates 0 rows.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-1",
	}, nil)
	waitForSessionRow(t, obsStore, "sess-1")

	// Simulate /teamster:bootstrap naming the team: registerPeer writes
	// team_name to the roster entry (not the sessions table).
	leadRosterID, err := obsStore.ResolveRosterID(ctx, "sess-1", "")
	if err != nil {
		t.Fatalf("ResolveRosterID for lead: %v", err)
	}
	leadEntry, err := obsStore.GetRosterEntry(ctx, leadRosterID)
	if err != nil {
		t.Fatalf("GetRosterEntry for lead: %v", err)
	}
	leadEntry.TeamName = "ops"
	if err := obsStore.UpsertRosterEntry(ctx, leadEntry); err != nil {
		t.Fatalf("UpsertRosterEntry with team: %v", err)
	}

	// Teammate's first event: auto-registers and should inherit "ops" from
	// the lead's roster entry.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-1",
		AgentType:     "scout",
	}, nil)

	entry := waitForRosterEntry(t, obsStore, "sess-1", "@scout")
	if entry.TeamName != "ops" {
		t.Fatalf("teammate roster team_name = %q, want ops", entry.TeamName)
	}
}

// waitForSessionRow polls until the lead's (session_id, agent_name="") row
// exists (the early-upsert roster path writes it from a detached goroutine)
// or a 2s deadline elapses.
func waitForSessionRow(t *testing.T, s store.Store, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.GetSession(context.Background(), store.SessionKey{SessionID: sessionID}); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session row for %s never appeared within deadline", sessionID)
}

// waitForRosterEntry polls until a roster entry exists for (sessionID,
// agentName) — the early-upsert roster path writes it from a detached
// goroutine — or a 2s deadline elapses.
func waitForRosterEntry(t *testing.T, s store.Store, sessionID, agentName string) store.RosterEntry {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rosterID, err := s.ResolveRosterID(ctx, sessionID, agentName); err == nil {
			if entry, err := s.GetRosterEntry(ctx, rosterID); err == nil {
				return entry
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("roster entry for (%s, %s) never appeared within deadline", sessionID, agentName)
	return store.RosterEntry{}
}

func TestResolveSubagentName_Basic(t *testing.T) {
	s := &Server{}

	// Lead spawns Agent(name="scraper-research", subagent_type omitted → default).
	spawnData := map[string]interface{}{}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput: map[string]interface{}{
			"name":        "scraper-research",
			"description": "Research token-scraper for remotes",
		},
	}, spawnData)

	// Simulate SubagentStart's own registration popping the queued name —
	// peek() (used by resolveSubagentName below) only reads the sticky
	// resolved map; it never touches the FIFO itself.
	s.subagentNames.pop("sess1", "general-purpose")

	// Subagent event arrives with agent_type="general-purpose".
	eventData := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, eventData)

	got := eventData["_agent_name"]
	if got != "@scraper-research" {
		t.Errorf("_agent_name = %q, want %q", got, "@scraper-research")
	}
}

func TestResolveSubagentName_ExplicitSubagentType(t *testing.T) {
	s := &Server{}

	// Lead spawns Agent(name="code-scout", subagent_type="Explore").
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput: map[string]interface{}{
			"name":          "code-scout",
			"subagent_type": "Explore",
		},
	}, map[string]interface{}{})

	// Simulate SubagentStart's own registration popping the queued name.
	s.subagentNames.pop("sess1", "Explore")

	eventData := map[string]interface{}{"_agent_name": "@Explore"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Bash",
		AgentType:     "Explore",
	}, eventData)

	got := eventData["_agent_name"]
	if got != "@code-scout" {
		t.Errorf("_agent_name = %q, want %q", got, "@code-scout")
	}
}

func TestResolveSubagentName_NoNameNoOverride(t *testing.T) {
	s := &Server{}

	// Agent call without a name — should NOT override.
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput: map[string]interface{}{
			"description": "do something",
		},
	}, map[string]interface{}{})

	eventData := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, eventData)

	got := eventData["_agent_name"]
	if got != "@general-purpose" {
		t.Errorf("_agent_name = %q, want %q (should not override unnamed agent)", got, "@general-purpose")
	}
}

func TestResolveSubagentName_LeadUnaffected(t *testing.T) {
	s := &Server{}

	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput: map[string]interface{}{
			"name": "worker",
		},
	}, map[string]interface{}{})

	// Lead's own events have empty AgentType — should not be touched.
	eventData := map[string]interface{}{}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Bash",
		AgentType:     "",
	}, eventData)

	if _, exists := eventData["_agent_name"]; exists {
		t.Errorf("lead's _agent_name was set unexpectedly: %v", eventData["_agent_name"])
	}
}

func TestResolveSubagentName_SessionIsolation(t *testing.T) {
	s := &Server{}

	// Session A: name "alpha".
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sessA",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "alpha"},
	}, map[string]interface{}{})

	// Session B: no Agent call — should not resolve.
	eventData := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sessB",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, eventData)

	got := eventData["_agent_name"]
	if got != "@general-purpose" {
		t.Errorf("cross-session leak: _agent_name = %q, want %q", got, "@general-purpose")
	}
}

func TestResolveSubagentName_FIFOOrdering(t *testing.T) {
	s := &Server{}

	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "alpha"},
	}, map[string]interface{}{})

	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "beta"},
	}, map[string]interface{}{})

	// Simulate SubagentStart popping "alpha" (the first queued spawn) before
	// its own first tool-call event arrives.
	s.subagentNames.pop("sess1", "general-purpose")

	first := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, first)
	if got := first["_agent_name"]; got != "@alpha" {
		t.Errorf("first resolve = %q, want @alpha (FIFO)", got)
	}

	// Simulate the second concurrent spawn's SubagentStart popping "beta".
	s.subagentNames.pop("sess1", "general-purpose")

	second := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, second)
	if got := second["_agent_name"]; got != "@beta" {
		t.Errorf("second resolve = %q, want @beta (FIFO)", got)
	}

	sticky := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, sticky)
	if got := sticky["_agent_name"]; got != "@beta" {
		t.Errorf("sticky fallback = %q, want @beta (last-popped)", got)
	}
}

func TestResolveSubagentName_ClearSession(t *testing.T) {
	s := &Server{}

	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "worker"},
	}, map[string]interface{}{})

	// Simulate SubagentStart popping+resolving "worker" before the clear —
	// clearSession must purge both the FIFO and the sticky resolved map.
	s.subagentNames.pop("sess1", "general-purpose")

	s.subagentNames.clearSession("sess1")

	eventData := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, eventData)

	got := eventData["_agent_name"]
	if got != "@general-purpose" {
		t.Errorf("_agent_name = %q after clear, want %q", got, "@general-purpose")
	}
}

func TestResolveSubagentName_ClearAgent(t *testing.T) {
	s := &Server{}

	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "worker"},
	}, map[string]interface{}{})

	// Simulate SubagentStart popping+resolving "worker" before the clear —
	// clearAgent must purge both the FIFO and the sticky resolved map.
	s.subagentNames.pop("sess1", "general-purpose")

	s.subagentNames.clearAgent("sess1", "general-purpose")

	eventData := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, eventData)

	if got := eventData["_agent_name"]; got != "@general-purpose" {
		t.Errorf("_agent_name = %q, want raw %q after clearAgent", got, "@general-purpose")
	}
}

func mfNames(mfs []*dto.MetricFamily) string {
	names := make([]string, 0, len(mfs))
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	return strings.Join(names, ",")
}

// TestHandleEvent_UserPromptSubmit_ReturnsAdditionalContext verifies that a
// UserPromptSubmit POST to /event includes additionalContext in the JSON
// response body. Remote clients (e.g. the Python thin client) read this field
// to inject the activity/team-dispatch nudge; the hub Go client generates its
// own copy locally and ignores hookd's response, so this only affects remotes.
func TestHandleEvent_UserPromptSubmit_ReturnsAdditionalContext(t *testing.T) {
	// Build a minimal Server with a real JSONL log file (handleEvent writes to it).
	f, err := os.CreateTemp(t.TempDir(), "hookd-*.jsonl")
	if err != nil {
		t.Fatalf("create temp log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:     config.Config{Host: "testhost"},
		logFile: f,
		metrics: observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker(
			"testhost", 5*time.Minute, 30*time.Second, nil,
		),
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	payload, _ := json.Marshal(map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      "sess-remote",
	})
	req := httptest.NewRequest(http.MethodPost, "/event", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleEvent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body, _ := io.ReadAll(rec.Body)
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	ctx, ok := resp["additionalContext"].(string)
	if !ok || ctx == "" {
		t.Fatalf("additionalContext missing or empty in UserPromptSubmit response; got: %v", resp)
	}
	// Must contain both the activity instruction and the team-dispatch mandate.
	if !strings.Contains(ctx, "reportActivity") {
		t.Errorf("additionalContext missing activity instruction; got: %q", ctx)
	}
	if !strings.Contains(ctx, "dispatch") {
		t.Errorf("additionalContext missing team-dispatch instruction; got: %q", ctx)
	}
	// The text must be byte-identical to the shared constants so hub Go client
	// and remote Python client can never drift.
	want := hook.ACTIVITY_INSTRUCTION + hook.TEAM_DISPATCH_INSTRUCTION
	if ctx != want {
		t.Errorf("additionalContext = %q\nwant: %q", ctx, want)
	}
}

// TestHandleEvent_PostToolUse_ObjectToolResponse_Codex is the WP-R7-reported
// regression: Codex sends tool_response as a JSON object for MCP tool calls
// (the raw tool_result shape), where Claude Code always sends a string. Before
// the fix, hook.HookEvent.ToolResponse was typed `string`, so the server's
// json.Unmarshal(body, &event) rejected every Codex PostToolUse event outright
// (400 "invalid json") — silently dropped, since codex-hook.py is
// exit-0-always and only logs to a local file the operator has to go looking
// for. PreToolUse was unaffected (no tool_response field on that event), so
// this bug was invisible to WMS focus/attribution but ate every
// PostToolUse-driven signal (feed lines, completeActivity's hook, etc.) for
// Codex sessions.
func TestHandleEvent_PostToolUse_ObjectToolResponse_Codex(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "hookd-*.jsonl")
	if err != nil {
		t.Fatalf("create temp log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:     config.Config{Host: "testhost"},
		logFile: f,
		metrics: observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker(
			"testhost", 5*time.Minute, 30*time.Second, nil,
		),
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	// Shape captured live off a Codex mcp__activity__reportActivity call
	// (see the codex-remote-kit research notes, wp-r7-verification).
	payload, _ := json.Marshal(map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"session_id":      "sess-codex-1",
		"tool_name":       "mcp__activity__reportActivity",
		"tool_response": map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Activity recorded: thought test"},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/event", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleEvent(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, body)
	}
}
