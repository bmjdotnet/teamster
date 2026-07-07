package main

import (
	"encoding/json"
)

// envelope is the outer shape of every line in a Codex rollout JSONL file:
// {"timestamp":"...","type":"session_meta|turn_context|event_msg|response_item","payload":{...}}
type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionMetaPayload is the payload of the first line of every rollout file.
// id is the session UUID — same as the filename and threads.id in
// state_5.sqlite (surface-map.md §2.1).
type sessionMetaPayload struct {
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	Originator string `json:"originator"` // "codex-tui" | "codex_exec"
	CliVersion string `json:"cli_version"`
}

// turnContextPayload carries the model in effect for the turn that follows.
// Emitted once per turn, before that turn's event_msg records.
type turnContextPayload struct {
	Model string `json:"model"`
}

// eventMsgPayload is the payload.payload of an event_msg envelope; its own
// "type" field discriminates the many event_msg shapes Codex emits. Only the
// fields the tailer consumes are declared.
type eventMsgPayload struct {
	Type string `json:"type"`

	// token_count
	Info *tokenCountInfo `json:"info"`

	// mcp_tool_call_end
	CallID     string          `json:"call_id"`
	Invocation *mcpInvocation  `json:"invocation"`
	Result     json.RawMessage `json:"result"`
}

// tokenCountInfo is the payload.info of a token_count event_msg. The ledger
// derivation rule (redteam m4, binding): use LastTokenUsage only — TotalTokenUsage
// is cumulative across the whole session and summing it double-counts.
type tokenCountInfo struct {
	TotalTokenUsage tokenUsage `json:"total_token_usage"`
	LastTokenUsage  tokenUsage `json:"last_token_usage"`
}

type tokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// mcpInvocation is the payload.invocation of an mcp_tool_call_end event_msg.
type mcpInvocation struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// mcpResult is the payload.result of an mcp_tool_call_end event_msg: a
// cancelled/denied call is {"Err":"..."}, a successful one is {"Ok":{...}} —
// same event type, discriminated only by which key is present (surface-map.md
// §2.1, verified against a live cancelled-call specimen). mcpCallOK reports
// which.
type mcpResult struct {
	Ok  json.RawMessage `json:"Ok"`
	Err json.RawMessage `json:"Err"`
}

// mcpCallOK parses a mcp_tool_call_end result and reports whether it was a
// success (Ok) as opposed to a cancellation/denial/failure (Err — a distinct
// shape, not a separate event type). Returns ok=false, matched=false when
// raw is empty or matches neither shape.
func mcpCallOK(raw json.RawMessage) (ok bool, matched bool) {
	if len(raw) == 0 {
		return false, false
	}
	var r mcpResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, false
	}
	if r.Err != nil {
		return false, true
	}
	if r.Ok != nil {
		return true, true
	}
	return false, false
}
