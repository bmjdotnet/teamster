package hook

import (
	"strings"
	"testing"
)

// mkEvent builds a minimal PreToolUse event map with tool_name and tool_input.
func mkEvent(hookEvent, toolName string, toolInput interface{}) map[string]interface{} {
	return map[string]interface{}{
		"hook_event_name": hookEvent,
		"tool_name":       toolName,
		"tool_input":      toolInput,
	}
}

func TestEnrichRecord_Tags(t *testing.T) {
	tests := []struct {
		name        string
		data        map[string]interface{}
		wantTag     string
		wantDisplay string
		wantFocus   string
		wantBashCmd string
		wantThought string
		wantDone    string
		wantTeam    string
	}{
		{
			name:        "Read → READ tag",
			data:        mkEvent("PreToolUse", "Read", map[string]interface{}{"file_path": "/etc/hosts"}),
			wantTag:     "READ",
			wantDisplay: "Reading __hosts__",
		},
		{
			name:        "Glob → READ tag",
			data:        mkEvent("PreToolUse", "Glob", map[string]interface{}{"pattern": "*.go"}),
			wantTag:     "READ",
			wantDisplay: "Finding __*.go__",
		},
		{
			name:        "Grep → READ tag",
			data:        mkEvent("PreToolUse", "Grep", map[string]interface{}{"pattern": "func main", "path": "src/"}),
			wantTag:     "READ",
			wantDisplay: "Searching for __func main__ in __src/__",
		},
		{
			name:        "Edit → EDIT tag",
			data:        mkEvent("PreToolUse", "Edit", map[string]interface{}{"file_path": "/tmp/foo.go"}),
			wantTag:     "EDIT",
			wantDisplay: "Editing __foo.go__",
		},
		{
			name:        "Write → EDIT tag",
			data:        mkEvent("PreToolUse", "Write", map[string]interface{}{"file_path": "/tmp/bar.go"}),
			wantTag:     "EDIT",
			wantDisplay: "Editing __bar.go__",
		},
		{
			name: "Bash with description → ACT tag and bash_cmd",
			data: mkEvent("PreToolUse", "Bash", map[string]interface{}{
				"command":     "go build ./...",
				"description": "Build all packages",
			}),
			wantTag:     " ACT",
			wantDisplay: "Build all packages",
			wantBashCmd: "go build ./...",
		},
		{
			name: "Bash without description → bash_cmd only, no tag",
			data: mkEvent("PreToolUse", "Bash", map[string]interface{}{
				"command": "ls -la",
			}),
			wantTag:     "",
			wantBashCmd: "ls -la",
		},
		{
			name: "mcp__activity__reportActivity → THNK",
			data: mkEvent("PreToolUse", "mcp__activity__reportActivity", map[string]interface{}{
				"type":    "reading",
				"message": "inspect auth flow",
			}),
			wantThought: "inspect auth flow",
		},
		{
			name: "mcp__activity__setOverallIntent → focus field",
			data: mkEvent("PreToolUse", "mcp__activity__setOverallIntent", map[string]interface{}{
				"message": "fix the login bug",
			}),
			wantFocus: "fix the login bug",
		},
		{
			name: "mcp__activity__completeActivity → _done field",
			data: mkEvent("PreToolUse", "mcp__activity__completeActivity", map[string]interface{}{
				"message": "auth bug fixed",
			}),
			wantDone: "auth bug fixed",
		},
		{
			name: "mcp__wms__wms_updateStatus → TASK tag",
			data: mkEvent("PreToolUse", "mcp__wms__wms_updateStatus", map[string]interface{}{
				"entityType": "workunit",
				"entityID":   "wu-1",
				"status":     "done",
			}),
			wantTag:     "TASK",
			wantDisplay: "Updating workunit __wu-1__ → __done__",
		},
		{
			name: "mcp__wms__wms_setFocus → _focus field (not _tool_tag)",
			data: mkEvent("PreToolUse", "mcp__wms__wms_setFocus", map[string]interface{}{
				"entityType": "workunit",
				"entityID":   "wu-1",
				"focus":      "write the HTTP handler",
			}),
			wantFocus: "workunit wu-1: write the HTTP handler",
		},
		{
			name: "mcp__wms__wms_addDependency → TASK tag",
			data: mkEvent("PreToolUse", "mcp__wms__wms_addDependency", map[string]interface{}{
				"blockerID": "wu-1",
				"blockedID": "wu-2",
			}),
			wantTag:     "TASK",
			wantDisplay: "Adding dependency: wu-1 → wu-2",
		},
		{
			name: "Agent with name → TEAM tag",
			data: mkEvent("PreToolUse", "Agent", map[string]interface{}{
				"name":        "store",
				"description": "Handle the sqlite layer",
			}),
			wantTag:     "TEAM",
			wantDisplay: "Spawning @store: Handle the sqlite layer",
		},
		{
			name: "SendMessage → COMM tag",
			data: mkEvent("PreToolUse", "SendMessage", map[string]interface{}{
				"to":      "store",
				"summary": "fix the scan function",
			}),
			wantTag:     "COMM",
			wantDisplay: "@store: fix the scan function",
		},
		{
			name: "WebSearch → WEB tag",
			data: mkEvent("PreToolUse", "WebSearch", map[string]interface{}{
				"query": "go json-rpc 2.0",
			}),
			wantTag:     " WEB",
			wantDisplay: "Searching web for __go json-rpc 2.0__",
		},
		{
			name: "WebFetch → WEB tag",
			data: mkEvent("PreToolUse", "WebFetch", map[string]interface{}{
				"url": "https://pkg.go.dev/encoding/json",
			}),
			wantTag:     " WEB",
			wantDisplay: "Fetching __pkg.go.dev/encoding/json__",
		},
		{
			name: "AskUserQuestion → ASK tag",
			data: mkEvent("PreToolUse", "AskUserQuestion", map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{"question": "Which port should hookd use?"},
				},
			}),
			wantTag:     " ASK",
			wantDisplay: "Asking: __Which port should hookd use?__",
		},
		{
			name:        "EnterPlanMode → PLAN tag",
			data:        mkEvent("PreToolUse", "EnterPlanMode", map[string]interface{}{}),
			wantTag:     "PLAN",
			wantDisplay: "Entering plan mode",
		},
		{
			name:        "ExitPlanMode → PLAN tag",
			data:        mkEvent("PreToolUse", "ExitPlanMode", map[string]interface{}{}),
			wantTag:     "PLAN",
			wantDisplay: "Exiting plan mode",
		},
		{
			name:        "Generic MCP tool → server(method) display",
			data:        mkEvent("PreToolUse", "mcp__playwright__browser_navigate", map[string]interface{}{}),
			wantTag:     "TOOL",
			wantDisplay: "playwright(__browser_navigate__)",
		},
		{
			name: "Unknown tool → TOOL fallback",
			data: mkEvent("PreToolUse", "SomeFutureTool", map[string]interface{}{
				"description": "do something new",
			}),
			wantTag:     "TOOL",
			wantDisplay: "do something new",
		},
		{
			name: "Stop event with stop_response → DONE tag",
			data: map[string]interface{}{
				"hook_event_name": "Stop",
				"stop_response":   "Fixed the auth bug. The tests now pass.",
			},
			wantTag:     "DONE",
			wantDisplay: "Fixed the auth bug.",
		},
		{
			name: "Stop event with last_assistant_message fallback → DONE tag",
			data: map[string]interface{}{
				"hook_event_name":        "Stop",
				"last_assistant_message": "All tasks complete. Build is green.",
			},
			wantTag:     "DONE",
			wantDisplay: "All tasks complete.",
		},
		{
			name: "Stop event with empty stop_response → no tag",
			data: map[string]interface{}{
				"hook_event_name": "Stop",
				"stop_response":   "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			EnrichRecord(tt.data)

			got := func(key string) string {
				v, _ := tt.data[key].(string)
				return v
			}

			if tag := got("_tool_tag"); tag != tt.wantTag {
				t.Errorf("_tool_tag = %q, want %q", tag, tt.wantTag)
			}
			if tt.wantDisplay != "" {
				if d := got("_tool_display"); d != tt.wantDisplay {
					t.Errorf("_tool_display = %q, want %q", d, tt.wantDisplay)
				}
			}
			if tt.wantFocus != "" {
				if f := got("_focus"); f != tt.wantFocus {
					t.Errorf("_focus = %q, want %q", f, tt.wantFocus)
				}
			}
			if tt.wantBashCmd != "" {
				if c := got("_bash_cmd"); c != tt.wantBashCmd {
					t.Errorf("_bash_cmd = %q, want %q", c, tt.wantBashCmd)
				}
			}
			if tt.wantThought != "" {
				if th := got("_thought"); th != tt.wantThought {
					t.Errorf("_thought = %q, want %q", th, tt.wantThought)
				}
			}
			if tt.wantDone != "" {
				if d := got("_done"); d != tt.wantDone {
					t.Errorf("_done = %q, want %q", d, tt.wantDone)
				}
			}
			if tt.wantTeam != "" {
				if tm := got("_team"); tm != tt.wantTeam {
					t.Errorf("_team = %q, want %q", tm, tt.wantTeam)
				}
			}
		})
	}
}

func TestEnrichRecord_Identity(t *testing.T) {
	t.Run("agent_type populated → _agent_name set", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/tmp/x"},
			"agent_type":      "store",
		}
		EnrichRecord(data)
		if v, _ := data["_agent_name"].(string); v != "@store" {
			t.Errorf("_agent_name = %q, want %q", v, "@store")
		}
	})

	t.Run("host field copied to _host", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/tmp/x"},
			"host":            "host-a",
		}
		EnrichRecord(data)
		if v, _ := data["_host"].(string); v != "host-a" {
			t.Errorf("_host = %q, want %q", v, "host-a")
		}
	})

	t.Run("absent agent_type → no _agent_name", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/tmp/x"},
		}
		EnrichRecord(data)
		if _, exists := data["_agent_name"]; exists {
			t.Errorf("_agent_name should not be set when agent_type is absent")
		}
	})
}

func TestEnrichRecord_Idempotency(t *testing.T) {
	t.Run("fully pre-enriched payload unchanged", func(t *testing.T) {
		// Simulates a hub-local Go client that already enriched before POSTing.
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/etc/hosts"},
			"_tool_tag":       "CUSTOM",
			"_tool_display":   "custom display",
			"_host":           "host-b",
			"_agent_name":     "@custom",
		}
		EnrichRecord(data)
		if v, _ := data["_tool_tag"].(string); v != "CUSTOM" {
			t.Errorf("_tool_tag changed: got %q, want %q", v, "CUSTOM")
		}
		if v, _ := data["_tool_display"].(string); v != "custom display" {
			t.Errorf("_tool_display changed: got %q, want %q", v, "custom display")
		}
		if v, _ := data["_host"].(string); v != "host-b" {
			t.Errorf("_host changed: got %q, want %q", v, "host-b")
		}
		if v, _ := data["_agent_name"].(string); v != "@custom" {
			t.Errorf("_agent_name changed: got %q, want %q", v, "@custom")
		}
	})

	t.Run("_thought pre-set → no tool enrichment applied", func(t *testing.T) {
		// If _thought is present, alreadyEnriched=true and tool enrichment is skipped.
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/etc/hosts"},
			"_thought":        "existing thought",
		}
		EnrichRecord(data)
		if v, _ := data["_thought"].(string); v != "existing thought" {
			t.Errorf("_thought changed: got %q, want %q", v, "existing thought")
		}
		// _tool_tag should NOT be set because enrichment was skipped
		if _, exists := data["_tool_tag"]; exists {
			t.Errorf("_tool_tag should not be set when payload is already enriched")
		}
	})

	t.Run("_host absent but _agent_name present → _host still populated from host field", func(t *testing.T) {
		// Identity enrichment runs before alreadyEnriched check.
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/etc/hosts"},
			"_tool_tag":       "READ",
			"_tool_display":   "existing display",
			"host":            "host-a",
		}
		EnrichRecord(data)
		if v, _ := data["_host"].(string); v != "host-a" {
			t.Errorf("_host = %q, want %q", v, "host-a")
		}
		// Tool tag was pre-set — should be unchanged
		if v, _ := data["_tool_tag"].(string); v != "READ" {
			t.Errorf("_tool_tag changed: got %q, want %q", v, "READ")
		}
	})
}

func TestEnrichRecord_EdgeCases(t *testing.T) {
	t.Run("empty map no panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("EnrichRecord panicked on empty map: %v", r)
			}
		}()
		EnrichRecord(map[string]interface{}{})
	})

	t.Run("nil tool_input no panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("EnrichRecord panicked on nil tool_input: %v", r)
			}
		}()
		EnrichRecord(map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Read",
			"tool_input":      nil,
		})
	})

	t.Run("PostToolUse TaskCreate captures task ID from response", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "TaskCreate",
			"tool_input":      map[string]interface{}{"subject": "Write the handler"},
			"tool_response":   "Created task #42",
		}
		EnrichRecord(data)
		tag, _ := data["_tool_tag"].(string)
		display, _ := data["_tool_display"].(string)
		if tag != "TASK" {
			t.Errorf("_tool_tag = %q, want TASK", tag)
		}
		if display == "" {
			t.Errorf("_tool_display should be non-empty")
		}
	})

	t.Run("PreToolUse TaskCreate gets TASK tag via GetToolTarget", func(t *testing.T) {
		// GetToolTarget("TaskCreate", ...) returns a plain result with display text;
		// TOOL_TAGS["TaskCreate"]="TASK" so it gets the TASK tag on PreToolUse.
		// The PostToolUse path additionally captures the assigned task ID from the response.
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "TaskCreate",
			"tool_input":      map[string]interface{}{"subject": "Write the handler"},
		}
		EnrichRecord(data)
		if tag, _ := data["_tool_tag"].(string); tag != "TASK" {
			t.Errorf("_tool_tag = %q, want TASK", tag)
		}
	})

	t.Run("ToolSearch skipped entirely", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "ToolSearch",
			"tool_input":      map[string]interface{}{"query": "select:Read"},
		}
		EnrichRecord(data)
		if _, exists := data["_tool_tag"]; exists {
			t.Errorf("_tool_tag should not be set for ToolSearch")
		}
	})

	t.Run("Stop with long message truncates at sentence boundary not at 80 chars", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "Stop",
			"stop_response":   "Short sentence. This is a much longer sentence that would exceed the old limit if included.",
		}
		EnrichRecord(data)
		display, _ := data["_tool_display"].(string)
		if display != "Short sentence." {
			t.Errorf("_tool_display = %q, want %q", display, "Short sentence.")
		}
	})

	t.Run("Stop with filename in message does not chop at dot in filename", func(t *testing.T) {
		data := map[string]interface{}{
			"hook_event_name": "Stop",
			"stop_response":   "the foo.md document has been updated. Check it now.",
		}
		EnrichRecord(data)
		display, _ := data["_tool_display"].(string)
		if display != "the foo.md document has been updated." {
			t.Errorf("_tool_display = %q, want full first sentence (no mid-filename chop)", display)
		}
	})

	t.Run("Stop with message >200 chars and boundary at ~240 carries full sentence", func(t *testing.T) {
		// Sentence boundary at char 240: producer must not chop at old 80-char cap.
		// The loop keeps the terminator, so want = 238 a's + ".".
		prefix := strings.Repeat("a", 238) + ". trailing text that should be excluded"
		data := map[string]interface{}{
			"hook_event_name": "Stop",
			"stop_response":   prefix,
		}
		EnrichRecord(data)
		display, _ := data["_tool_display"].(string)
		want := strings.Repeat("a", 238) + "."
		if display != want {
			t.Errorf("display len=%d, want len=%d (full 238-char sentence + terminator)", len(display), len(want))
		}
	})

	t.Run("TaskCreate PostToolUse with 150-char subject carries full subject", func(t *testing.T) {
		longSubject := strings.Repeat("b", 150)
		data := map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "TaskCreate",
			"tool_input":      map[string]interface{}{"subject": longSubject},
			"tool_response":   "Created task #7",
		}
		EnrichRecord(data)
		display, _ := data["_tool_display"].(string)
		if !strings.Contains(display, longSubject) {
			t.Errorf("display %q does not contain full 150-char subject", display)
		}
	})

	t.Run("PostToolUse non-TaskCreate tool no enrichment", func(t *testing.T) {
		// PostToolUse for regular tools is only handled for TaskCreate; others are no-ops.
		data := map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "Read",
			"tool_input":      map[string]interface{}{"file_path": "/etc/hosts"},
		}
		EnrichRecord(data)
		if _, exists := data["_tool_tag"]; exists {
			t.Errorf("_tool_tag should not be set for PostToolUse Read")
		}
	})
}

func TestTrimLargeInput(t *testing.T) {
	t.Run("string over limit is capped", func(t *testing.T) {
		big := strings.Repeat("x", 64<<10)
		data := map[string]interface{}{"tool_input": big}
		trimLargeInput(data)
		got := data["tool_input"].(string)
		if len(got) != 32<<10 {
			t.Errorf("len = %d, want %d", len(got), 32<<10)
		}
	})

	t.Run("string under limit untouched", func(t *testing.T) {
		small := "echo hello"
		data := map[string]interface{}{"tool_input": small}
		trimLargeInput(data)
		if data["tool_input"] != small {
			t.Error("small string was modified")
		}
	})

	t.Run("map input untouched", func(t *testing.T) {
		m := map[string]interface{}{"command": "ls"}
		data := map[string]interface{}{"tool_input": m}
		trimLargeInput(data)
		if _, ok := data["tool_input"].(map[string]interface{}); !ok {
			t.Error("map was replaced")
		}
	})
}
