// Package transcript is the shared reader for Claude Code session JSONL
// transcripts (~/.claude/projects/<project>/<sessionId>.jsonl and their
// subagents/agent-*.jsonl siblings).
//
// It owns two things both the token-scraper and the focus-attribution recovery
// pass depend on, so they parse the same bytes the same way:
//
//   - DedupKey / the line parse: the composite ledger key message.id|requestId
//     (spec §2.4) that becomes token_ledger.message_id. A join on the bare
//     message.id matches 0%; on the composite key it matches 100%.
//   - The per-thread intended-focus timeline: every wms_setFocus an agent issued,
//     as an ordered (timestamp, entityType, entityID) step-function keyed by
//     agent thread (lead = "", teammate = "@Name"). This is the signal the
//     recovery pass uses to re-attribute unallocated cost (spec §5.2).
package transcript

import (
	"encoding/json"
	"time"
)

// setFocusToolNames lists every wire-format name a setFocus tool_use block may
// carry across Claude Code versions and MCP routing variants.
var setFocusToolNames = map[string]bool{
	"mcp__wms__wms_setFocus": true,
	"wms_setFocus":           true,
	"setFocus":               true,
}

// isSetFocusTool returns true if name is any known wire-format variant.
func isSetFocusTool(name string) bool {
	return setFocusToolNames[name]
}

// Line is the minimal shape decoded from one transcript JSONL line. It is the
// canonical parse shared by the scraper (token usage) and the recovery pass
// (setFocus intent). Fields the scraper does not need (the tool_use name/input)
// coexist with the usage fields so a single decode serves both consumers.
type Line struct {
	Type      string    `json:"type"`
	UUID      string    `json:"uuid"`
	RequestID string    `json:"requestId"`
	SessionID string    `json:"sessionId"`
	Timestamp time.Time `json:"timestamp"`
	Message   Message   `json:"message"`
}

// Message is the assistant message envelope within a transcript Line.
type Message struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    FlexContent    `json:"content"`
	Usage      Usage          `json:"usage"`
}

// FlexContent handles transcript message content that can be either a JSON
// string (common for user messages) or an array of ContentBlock (assistant
// messages, some user messages).
type FlexContent struct {
	Text   string         // populated when JSON content is a bare string
	Blocks []ContentBlock // populated when JSON content is an array
}

func (fc *FlexContent) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		return json.Unmarshal(data, &fc.Text)
	}
	return json.Unmarshal(data, &fc.Blocks)
}

// ContentBlock is one block of an assistant message. Type is one of "text",
// "tool_use", "thinking". Text is populated for text blocks; Name/Input are
// populated for tool_use blocks.
type ContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// Usage mirrors the API usage object Claude Code records per assistant line.
type Usage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	ServiceTier              string `json:"service_tier"`
	Speed                    string `json:"speed"`
	CacheCreation            struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
}

// DedupKey is the shared composite ledger key: message.id|requestId (spec §2.4).
// It is the join key into token_ledger.message_id — a join on the bare message.id
// matches 0% of rows, on the composite key 100%. messageID == "" falls back to a
// caller-supplied uuid form via LineDedupKey; this pure helper assumes a present
// messageID and never falls back (recovery drives off the DB's message_id, which
// is always the composite form for real assistant rows).
func DedupKey(messageID, requestID string) string {
	return messageID + "|" + requestID
}

// LineDedupKey identifies a single API request from a parsed Line. Claude Code
// repeats message.id and requestId across the per-content-block transcript lines
// of one response, so the pair uniquely names the request. Older/synthetic lines
// that lack message.id fall back to the top-level uuid (each is then its own
// group). This is the form the scraper keys its dedup on.
func LineDedupKey(line Line) string {
	if line.Message.ID == "" {
		return line.UUID
	}
	return DedupKey(line.Message.ID, line.RequestID)
}
