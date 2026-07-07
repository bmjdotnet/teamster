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
// id is the file's OWN thread UUID — same as the filename and threads.id in
// state_5.sqlite (surface-map.md §2.1).
//
// Codex 0.142.x's thread_spawn (subagent) feature: a subagent runs as its own
// thread with its own rollout file, whose session_meta carries session_id ==
// the PARENT thread's id and parent_thread_id == the same parent id (both
// empty/absent for a top-level, non-subagent file). Confirmed live
// (chunk-test2 evidence, 06-rollout-subagent-meta.txt): a top-level file has
// id == session_id and no parent_thread_id; a subagent file has id != session_id,
// with session_id carrying the parent's id. 0.137.0's session_meta has NO
// session_id field at all (this package's own redteam fixture) — SessionID
// must fall back to ID when absent, which also makes non-subagent files on
// 0.142.x (session_id == id) behave identically either way.
//
// AgentRole/AgentNickname are only present on subagent files (e.g. "explorer"/
// "Mencius") — same fields as state_5.sqlite's threads table. Verified live
// that wms-mcp's existing identity handling already opens focus intervals for
// subagent work under agent_name "@"+role (e.g. "@explorer") — matching that
// exactly (role, not nickname) is what lets rollup's temporal_join attribute
// subagent spend precisely instead of falling back to the lead's own focus.
type sessionMetaPayload struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	ParentThreadID string `json:"parent_thread_id"`
	Cwd            string `json:"cwd"`
	Originator     string `json:"originator"` // "codex-tui" | "codex_exec"
	CliVersion     string `json:"cli_version"`
	AgentRole      string `json:"agent_role"`
	AgentNickname  string `json:"agent_nickname"`
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

// tokenUsage's fields are NOT disjoint buckets: cached_input_tokens is a
// subset of input_tokens, and reasoning_output_tokens is a subset of
// output_tokens (both are informational breakdowns, not additional tokens).
// Verified against live evidence (surface-map.md, and this package's own
// resumed-rollout fixture): total_tokens == input_tokens + output_tokens
// always, with cached_input/reasoning_output never adding to that sum. See
// emitLedgerRow's derivation and its sanity check.
type tokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
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
