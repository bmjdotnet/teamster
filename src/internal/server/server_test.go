package server

import (
	"bytes"
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

func TestResolveSubagentName_LastSpawnWins(t *testing.T) {
	s := &Server{}

	// First spawn: name "alpha".
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "alpha"},
	}, map[string]interface{}{})

	// Second spawn (same type): name "beta".
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Agent",
		ToolInput:     map[string]interface{}{"name": "beta"},
	}, map[string]interface{}{})

	eventData := map[string]interface{}{"_agent_name": "@general-purpose"}
	s.resolveSubagentName(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "sess1",
		ToolName:      "Read",
		AgentType:     "general-purpose",
	}, eventData)

	got := eventData["_agent_name"]
	if got != "@beta" {
		t.Errorf("_agent_name = %q, want %q (last spawn wins)", got, "@beta")
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
	s.bus.subscribers = make(map[uint64]chan []byte)

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
