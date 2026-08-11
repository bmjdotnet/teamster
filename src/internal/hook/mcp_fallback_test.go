package hook

import "testing"

// TestProcessEvent_UnhandledMCPToolLeavesEnrichmentToHookd is a regression
// test for a bug found live on chunk: mcp__roster__registerPeer stayed
// [TOOL] in the feed forever because ProcessEvent's generic default case
// unconditionally wrote _tool_tag="TOOL" client-side. hookd's EnrichRecord
// treats any non-empty _tool_tag as alreadyEnriched and never calls the
// interceptor registry, so the client-side write permanently shadowed the
// registry's real mcp__roster__ rules for every hub-local session (the Go
// hook client, unlike the Python remote client, pre-enriches before
// POSTing). ProcessEvent must leave mcp__ tools it doesn't specifically
// recognize unenriched so hookd's registry gets first refusal.
func TestProcessEvent_UnhandledMCPToolLeavesEnrichmentToHookd(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()

	event := HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "regressn0001",
		ToolName:      "mcp__roster__registerPeer",
		ToolInput:     map[string]interface{}{"team_name": "feed-forge"},
	}
	raw := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "regressn0001",
		"tool_name":       "mcp__roster__registerPeer",
		"tool_input":      map[string]interface{}{"team_name": "feed-forge"},
	}
	ProcessEvent(event, raw, srv.URL, t.TempDir(), true)

	if tag, exists := raw["_tool_tag"]; exists {
		t.Errorf("_tool_tag = %q, want unset — an unhandled mcp__ tool must be left for hookd's registry", tag)
	}
	if display, exists := raw["_tool_display"]; exists {
		t.Errorf("_tool_display = %q, want unset — an unhandled mcp__ tool must be left for hookd's registry", display)
	}
}

// TestProcessEvent_UnknownBuiltinToolStillGetsToolFallback confirms the fix
// is scoped to mcp__ tools only — a genuinely unknown non-mcp tool name (a
// future built-in Claude Code tool the registry has no way to enrich, since
// intercept.Registry.Match only ever matches mcp__* names) must still get
// the client-side TOOL fallback tag/display, unchanged from before the fix.
func TestProcessEvent_UnknownBuiltinToolStillGetsToolFallback(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()

	event := HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "regressn0002",
		ToolName:      "SomeFutureBuiltinTool",
		ToolInput:     map[string]interface{}{"description": "do something new"},
	}
	raw := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "regressn0002",
		"tool_name":       "SomeFutureBuiltinTool",
		"tool_input":      map[string]interface{}{"description": "do something new"},
	}
	ProcessEvent(event, raw, srv.URL, t.TempDir(), true)

	if tag, _ := raw["_tool_tag"].(string); tag != "TOOL" {
		t.Errorf("_tool_tag = %q, want TOOL", tag)
	}
	if display, _ := raw["_tool_display"].(string); display != "do something new" {
		t.Errorf("_tool_display = %q, want %q", display, "do something new")
	}
}
