// Package codexhook implements the Codex CLI hook client's payload shaping —
// everything cmd/codex-hook does to a raw Codex hook payload before POSTing
// it to hookd, factored out so it is unit-testable without a live Codex
// binary.
//
// This client is deliberately much thinner than cmd/teamster's Claude Code
// hook client (internal/hook.ProcessEvent): Codex v1 is solo-only (no Agent
// Teams, so no dedup files or session-mode markers are needed), and the
// WMS-attribution fix for Codex sessions lives entirely in wms-mcp (WP1, via
// the MCP _meta turn-metadata channel on every tools/call) — this client's
// payload never needs to carry or derive a session identity beyond what
// Codex's own hook JSON already provides in "session_id".
//
// What this package must NEVER do (WP1's fail-safe requirement, carried here
// as a design constraint even though this package has no file-IO of its
// own): nothing in this client ever writes ~/.claude/current-session-id or
// any file the Claude-Code-specific WMS-attribution fallback reads. Doing so
// from a Codex process would silently steal attribution from whatever Claude
// Code session happens to be running concurrently on the same host — the
// exact bug WP1 fixes.
package codexhook

import "github.com/bmjdotnet/teamster/internal/redact"

// Field caps mirror hookd's own 1MB POST body limit
// (internal/server.maxBodySize) and the existing Python hook client's own
// caps (skel/lib/hook/teamster.py) — a PostToolUse payload for a large MCP
// result (e.g. wms_listOutcomes returning many rows) can otherwise exceed it
// and get rejected outright. Only string-valued fields are capped, matching
// both existing clients: an object-shaped tool_response (the normal MCP
// result envelope) is not size-bounded here either — a pre-existing gap
// shared with the Claude Code clients, not something new to this one.
const (
	maxToolResponseBytes     = 1024
	maxStopResponseBytes     = 1024
	maxLastAssistantMsgBytes = 1024
	maxToolInputStringBytes  = 32768
)

// Enrich mutates a raw Codex hook payload in place, in preparation for
// POSTing it to hookd:
//
//   - Sets "_host" to host (the caller's already-resolved TEAMSTER_HOST or
//     hostname fallback), unless already present.
//   - Copies Codex's own "model" field to "_model", unless already present.
//     Codex sends "model" on every hook event; Claude Code's Go client
//     instead derives "_model" by reading Claude Code's own settings.json
//     (getModel() in internal/hook), since Claude's hook payload doesn't
//     carry a model field — Codex needs no equivalent lookup, the value is
//     already on the payload.
//   - Redacts any shell command text in tool_input before this payload ever
//     leaves the process, mirroring both existing hook clients' client-side
//     redaction (defense in depth: hookd's own EnrichRecord redacts again at
//     ingest via the same tool_name="Bash" path Codex's hook payload already
//     uses, but scrubbing here means a secret never touches the wire at all,
//     even for a hub-local POST).
//   - Caps oversized string fields to hookd's body limit.
func Enrich(data map[string]interface{}, host string) {
	if _, exists := data["_host"]; !exists {
		data["_host"] = host
	}
	if model, ok := data["model"].(string); ok && model != "" {
		if _, exists := data["_model"]; !exists {
			data["_model"] = model
		}
	}
	redactToolInput(data)
	capStringField(data, "tool_response", maxToolResponseBytes)
	capStringField(data, "stop_response", maxStopResponseBytes)
	capStringField(data, "last_assistant_message", maxLastAssistantMsgBytes)
	capStringField(data, "tool_input", maxToolInputStringBytes)
}

// redactToolInput mirrors teamster.py's _redact_event: tool_input is either
// an object (the common MCP/exec case — only its "command" key, if any, is
// a redaction target) or a bare string (redact the whole thing).
func redactToolInput(data map[string]interface{}) {
	switch ti := data["tool_input"].(type) {
	case map[string]interface{}:
		if cmd, ok := ti["command"].(string); ok && cmd != "" {
			ti["command"] = redact.Redact(cmd)
		}
	case string:
		if ti != "" {
			data["tool_input"] = redact.Redact(ti)
		}
	}
}

// capStringField truncates data[key] to max bytes, only when it is a string
// (an object-shaped field, e.g. an MCP tool_response envelope, is left
// alone — see the package doc comment's note on this shared limitation).
func capStringField(data map[string]interface{}, key string, max int) {
	if s, ok := data[key].(string); ok && len(s) > max {
		data[key] = s[:max]
	}
}
