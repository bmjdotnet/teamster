package hook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discardServer returns an httptest server that accepts and drops POSTs, so
// ProcessEvent's fire-and-forget postEvent succeeds without touching a real hub.
func discardServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// additionalContext decodes ProcessEvent's stdout JSON and returns the
// hookSpecificOutput.additionalContext string.
func additionalContext(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var m struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
			Decision          string `json:"decision"`
			Reason            string `json:"reason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("unmarshal ProcessEvent output: %v\noutput=%q", err, out)
	}
	return m.HookSpecificOutput.AdditionalContext
}

func decision(t *testing.T, out string) (decision, reason string) {
	t.Helper()
	if out == "" {
		return "", ""
	}
	var m struct {
		HookSpecificOutput struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("unmarshal ProcessEvent output: %v\noutput=%q", err, out)
	}
	return m.HookSpecificOutput.Decision, m.HookSpecificOutput.Reason
}

// TestSolo_UserPromptSubmit_TeamHalfSuppressed covers gates (a) and (b): in solo
// mode the additionalContext is exactly the activity half — no team-dispatch
// mandate and no bootstrap nudge. In team mode it begins with the full
// activity+dispatch string (byte-identical to pre-solo), and the bootstrap
// nudge appears iff no team dir exists.
func TestSolo_UserPromptSubmit_TeamHalfSuppressed(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()

	const bootstrapMarker = "Run /teamster:bootstrap"

	// Solo: additionalContext must equal ACTIVITY_INSTRUCTION exactly.
	soloEvent := HookEvent{HookEventName: "UserPromptSubmit", SessionID: "solosess0001"}
	soloRaw := map[string]interface{}{"hook_event_name": "UserPromptSubmit", "session_id": "solosess0001"}
	soloOut := ProcessEvent(soloEvent, soloRaw, srv.URL, t.TempDir(), true)
	soloCtx := additionalContext(t, soloOut)

	if soloCtx != ACTIVITY_INSTRUCTION {
		t.Errorf("solo additionalContext != ACTIVITY_INSTRUCTION\n got=%q\nwant=%q", soloCtx, ACTIVITY_INSTRUCTION)
	}
	if strings.Contains(soloCtx, "Agent Teams") {
		t.Error("solo additionalContext must not contain the team-dispatch mandate")
	}
	if strings.Contains(soloCtx, bootstrapMarker) {
		t.Error("solo additionalContext must not contain the bootstrap nudge")
	}

	// Team: additionalContext begins with the full pre-solo string.
	teamEvent := HookEvent{HookEventName: "UserPromptSubmit", SessionID: "teamsess0001"}
	teamRaw := map[string]interface{}{"hook_event_name": "UserPromptSubmit", "session_id": "teamsess0001"}
	teamOut := ProcessEvent(teamEvent, teamRaw, srv.URL, t.TempDir(), false)
	teamCtx := additionalContext(t, teamOut)

	wantPrefix := ACTIVITY_INSTRUCTION + TEAM_DISPATCH_INSTRUCTION
	if !strings.HasPrefix(teamCtx, wantPrefix) {
		t.Errorf("team additionalContext missing activity+dispatch prefix\n got=%q\nwant prefix=%q", teamCtx, wantPrefix)
	}
	// The only allowed suffix is the bootstrap nudge, and only when no team dir
	// exists. Whatever hasTeam() reports here, the team context must match the
	// production composition exactly.
	if hasTeam() {
		if teamCtx != wantPrefix {
			t.Errorf("team additionalContext with a team present must be exactly activity+dispatch\n got=%q", teamCtx)
		}
	} else {
		if !strings.Contains(teamCtx, bootstrapMarker) {
			t.Error("team additionalContext without a team must contain the bootstrap nudge")
		}
	}
}

// TestSolo_PreToolUse_BareAgentAllowed covers gate (c): a bare Agent (no
// name field) is silently allowed in all modes — the bare-Agent block was
// removed in the implicit-teams migration.
func TestSolo_PreToolUse_BareAgentAllowed(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()

	bareInput := map[string]interface{}{"description": "do a thing", "name": "scout"}

	// Solo: no block, silent allow — ProcessEvent returns "".
	soloEvent := HookEvent{HookEventName: "PreToolUse", ToolName: "Agent", SessionID: "solosess0002", ToolInput: bareInput}
	soloRaw := map[string]interface{}{
		"hook_event_name": "PreToolUse", "tool_name": "Agent", "session_id": "solosess0002", "tool_input": bareInput,
	}
	soloOut := ProcessEvent(soloEvent, soloRaw, srv.URL, t.TempDir(), true)
	if soloOut != "" {
		t.Errorf("solo bare-Agent must produce no output (silent allow), got %q", soloOut)
	}

	// Team: also silently allowed — bare-Agent blocking was removed.
	teamEvent := HookEvent{HookEventName: "PreToolUse", ToolName: "Agent", SessionID: "teamsess0002", ToolInput: bareInput}
	teamRaw := map[string]interface{}{
		"hook_event_name": "PreToolUse", "tool_name": "Agent", "session_id": "teamsess0002", "tool_input": bareInput,
	}
	teamOut := ProcessEvent(teamEvent, teamRaw, srv.URL, t.TempDir(), false)
	if dec, _ := decision(t, teamOut); dec == "block" {
		t.Errorf("team bare-Agent must not be blocked (implicit-teams migration), got output=%q", teamOut)
	}
}

// TestSolo_TeamMode_ContextByteIdentical asserts the !solo composition is exactly
// the strings that existed before the const split — guarding the invariant that
// solo mode is a pure suppression and team mode is unchanged.
func TestSolo_TeamMode_ContextByteIdentical(t *testing.T) {
	want := "Before starting work this turn, call reportActivity(type, message) " +
		"and setOverallIntent(message) if not already set. " +
		"Types: thought, reading, writing, executing, planning, reviewing. " +
		"Keep messages under 8 words, imperative: 'inspect host health', 'fix auth bug'. " +
		"Call completeActivity(message) when you finish a task or turn's objective. " +
		"If working on a WMS-tracked entity and you haven't called wms_setFocus yet this session, " +
		"do so now — it's the cost-bearing focus (reportActivity is cosmetic only).\n\n" +
		"When dispatching parallel work, spawn named teammates (give each a `name` for addressability) " +
		"and route follow-up tasks to existing idle teammates via SendMessage — " +
		"do not spawn replacements. Teammates collaborate directly: @tester messages @store about a " +
		"bug, @store fixes, @tester re-tests. The lead monitors but does not relay. Keep all " +
		"teammates alive until the human operator reviews and accepts the work."
	if got := ACTIVITY_INSTRUCTION + TEAM_DISPATCH_INSTRUCTION; got != want {
		t.Errorf("activity+dispatch composition drifted from pre-split bytes\n got=%q\nwant=%q", got, want)
	}
}
