// Package activity implements the activity MCP tool handlers (reportActivity,
// setOverallIntent, completeActivity). It is transport-agnostic: no imports
// from internal/server or cmd/hookd.
package activity

import (
	"encoding/json"
	"fmt"
	"strings"
)

var validActivityTypes = map[string]bool{
	"thought": true, "reading": true, "writing": true,
	"executing": true, "planning": true, "reviewing": true,
}

// ToolDefs is the MCP tools/list payload for this server.
var ToolDefs = []map[string]interface{}{
	{
		"name":        "reportActivity",
		"description": "Report what you're doing RIGHT NOW.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"type":    map[string]interface{}{"type": "string"},
				"message": map[string]interface{}{"type": "string"},
			},
			"required": []string{"type", "message"},
		},
	},
	{
		"name":        "setOverallIntent",
		"description": "Declare your overall mission for this session.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{"type": "string"},
			},
			"required": []string{"message"},
		},
	},
	{
		"name":        "completeActivity",
		"description": "Declare what you accomplished.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{"type": "string"},
			},
			"required": []string{"message"},
		},
	},
	{
		"name": "setMode",
		"description": "Declare this session's collaboration mode: \"solo\" (single " +
			"agent, no team) or \"team\" (Agent Teams). The hook records it for this " +
			"session; \"team\" is the default when unset.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"mode": map[string]interface{}{"type": "string", "enum": []string{"solo", "team"}},
			},
			"required": []string{"mode"},
		},
	},
}

// CallParams holds the parsed tools/call params.
type CallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
	// Meta captures identity from params._meta (host, session_id, agent_type).
	Meta Meta `json:"_meta"`
}

// Meta carries request identity injected by the MCP client.
type Meta struct {
	Host      string `json:"host"`
	SessionID string `json:"session_id"`
	AgentType string `json:"agent_type"`
}

// Result is the MCP tools/call success result.
type Result struct {
	Content []map[string]interface{} `json:"content"`
}

// HandleToolCall dispatches a tools/call request and returns the result text
// or an error. The caller is responsible for encoding the JSON-RPC envelope.
func HandleToolCall(rawParams json.RawMessage) (string, *CallError) {
	var p CallParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return "", &CallError{Code: -32602, Message: "invalid params"}
	}

	strArg := func(key string) string {
		v, _ := p.Arguments[key].(string)
		return strings.TrimSpace(v)
	}

	switch p.Name {
	case "reportActivity":
		actType := strArg("type")
		msg := strArg("message")
		if !validActivityTypes[actType] {
			return "", &CallError{Code: -32602, Message: "type must be one of: thought, reading, writing, executing, planning, reviewing"}
		}
		return fmt.Sprintf("Activity recorded: %s %s", actType, msg), nil

	case "setOverallIntent":
		return "Overall intent set: " + strArg("message"), nil

	case "completeActivity":
		return "Activity completed: " + strArg("message"), nil

	case "setMode":
		// No-op confirmation; the hook does the real work (writes the session
		// mode marker from the PreToolUse payload, which carries the authoritative
		// session_id). Validate the value so a typo doesn't silently no-op.
		mode := strArg("mode")
		if mode != "solo" && mode != "team" {
			return "", &CallError{Code: -32602, Message: `mode must be "solo" or "team"`}
		}
		return "Mode set: " + mode, nil

	default:
		return "", &CallError{Code: -32601, Message: "unknown tool: " + p.Name}
	}
}

// CallError represents a JSON-RPC error for a tools/call.
type CallError struct {
	Code    int
	Message string
}

func (e *CallError) Error() string { return e.Message }

// TextResult wraps a text string in the MCP content envelope.
func TextResult(text string) Result {
	return Result{
		Content: []map[string]interface{}{
			{"type": "text", "text": text},
		},
	}
}
