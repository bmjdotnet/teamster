package codexhook

import (
	"strings"
	"testing"
)

func TestEnrich_SetsHostAndModel(t *testing.T) {
	data := map[string]interface{}{
		"hook_event_name": "SessionStart",
		"session_id":      "019f3e04-9e68-7e12-b4c1-1737bfbb5034",
		"model":           "gpt-5.5",
	}
	Enrich(data, "plex")
	if data["_host"] != "plex" {
		t.Errorf("_host = %v, want plex", data["_host"])
	}
	if data["_model"] != "gpt-5.5" {
		t.Errorf("_model = %v, want gpt-5.5", data["_model"])
	}
}

func TestEnrich_DoesNotOverwriteExistingHostOrModel(t *testing.T) {
	data := map[string]interface{}{
		"_host":  "already-set",
		"model":  "gpt-5.5",
		"_model": "already-set-model",
	}
	Enrich(data, "plex")
	if data["_host"] != "already-set" {
		t.Errorf("_host was overwritten: %v", data["_host"])
	}
	if data["_model"] != "already-set-model" {
		t.Errorf("_model was overwritten: %v", data["_model"])
	}
}

func TestEnrich_RedactsBashCommandInToolInput(t *testing.T) {
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]interface{}{
			"command": "mysql -u root -pSuperSecret123 -e 'show tables'",
		},
	}
	Enrich(data, "plex")
	ti, ok := data["tool_input"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_input is not a map: %T", data["tool_input"])
	}
	cmd, _ := ti["command"].(string)
	if strings.Contains(cmd, "SuperSecret123") {
		t.Errorf("expected the mysql password to be redacted, got: %q", cmd)
	}
}

func TestEnrich_RedactsBareStringToolInput(t *testing.T) {
	data := map[string]interface{}{
		"tool_input": "curl -u admin:hunter2 https://example.com",
	}
	Enrich(data, "plex")
	s, _ := data["tool_input"].(string)
	if strings.Contains(s, "hunter2") {
		t.Errorf("expected the curl password to be redacted, got: %q", s)
	}
}

func TestEnrich_LeavesMCPToolInputUnredactedWhenNoCommandKey(t *testing.T) {
	// MCP tool calls (e.g. mcp__wms__wms_listOutcomes) never carry a
	// "command" key — Enrich must leave the rest of the arguments alone.
	data := map[string]interface{}{
		"tool_name": "mcp__wms__wms_listOutcomes",
		"tool_input": map[string]interface{}{
			"status": "open",
		},
	}
	Enrich(data, "plex")
	ti, _ := data["tool_input"].(map[string]interface{})
	if ti["status"] != "open" {
		t.Errorf("expected non-command MCP args to be untouched, got: %v", ti)
	}
}

func TestEnrich_CapsOversizedStringFields(t *testing.T) {
	big := strings.Repeat("x", 50000)
	data := map[string]interface{}{
		"tool_response":          big,
		"stop_response":          big,
		"last_assistant_message": big,
		"tool_input":             big,
	}
	Enrich(data, "plex")
	if got := len(data["tool_response"].(string)); got != maxToolResponseBytes {
		t.Errorf("tool_response len = %d, want %d", got, maxToolResponseBytes)
	}
	if got := len(data["stop_response"].(string)); got != maxStopResponseBytes {
		t.Errorf("stop_response len = %d, want %d", got, maxStopResponseBytes)
	}
	if got := len(data["last_assistant_message"].(string)); got != maxLastAssistantMsgBytes {
		t.Errorf("last_assistant_message len = %d, want %d", got, maxLastAssistantMsgBytes)
	}
	if got := len(data["tool_input"].(string)); got != maxToolInputStringBytes {
		t.Errorf("tool_input len = %d, want %d", got, maxToolInputStringBytes)
	}
}

func TestEnrich_LeavesObjectShapedToolResponseUncapped(t *testing.T) {
	// Matches both existing clients' shared limitation — see the package
	// doc comment. This test documents the behavior, not endorses it as a
	// permanent gap.
	envelope := map[string]interface{}{"content": []interface{}{
		map[string]interface{}{"type": "text", "text": strings.Repeat("y", 50000)},
	}}
	data := map[string]interface{}{"tool_response": envelope}
	Enrich(data, "plex")
	if _, ok := data["tool_response"].(map[string]interface{}); !ok {
		t.Fatalf("expected tool_response to remain an object, got %T", data["tool_response"])
	}
}
