// Composition estimator: walks an agent's own Claude Code transcript JSONL
// to estimate what fraction of its session so far was spent on assistant
// text, tool_use requests, thinking, and tool_result ("reading") content.
// Estimate-fidelity by construction — Claude's thinking blocks are stored
// redacted (`"thinking":""`), so thinking_pct is always a residual derived
// from output_tokens×4 minus the measured text/tool_use bytes, never a
// direct measurement. Populated only for sessions with a locally-readable
// transcript (hub-local claude_code sessions); remote sessions, Codex
// sessions (different file layout entirely), and sessions with no transcript
// found are left with a nil composition — no error, just no data.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// compositionJSON is the wire shape written to gauge.GaugeRow.CompositionJSON.
// The four percentages always sum to ~1.0 by construction (see percentages()):
// text/tool_use/reading are real byte-length measurements, thinking is the
// residual of the output token budget those bytes don't account for.
type compositionJSON struct {
	TextPct     float64 `json:"text_pct"`
	ToolUsePct  float64 `json:"tool_use_pct"`
	ThinkingPct float64 `json:"thinking_pct"`
	ReadingPct  float64 `json:"reading_pct"`
}

// compositionState is one agent's running, cumulative byte/token accumulator,
// carried across collector ticks so the transcript is never re-walked from
// the start — only new bytes since cursorOffset are scanned each tick,
// mirroring cmd/token-scraper's cursor pattern.
type compositionState struct {
	transcriptPath string
	cursorOffset   int64
	textBytes      int64
	toolUseBytes   int64
	outputTokens   int64 // sum of each closed turn's max output_tokens
	readingBytes   int64 // tool_result bytes, all tool names combined

	// pendingToolNames maps an in-flight tool_use block's id to its tool
	// name, so a later tool_result block (in the following user-role
	// message) can be attributed. Registered as soon as a tool_use block is
	// seen (not deferred to turn-close) since the matching tool_result can
	// arrive in the very next line. Entries are removed once consumed;
	// unconsumed entries (a tool_use with no result yet, e.g. still
	// executing) are harmless — they're just not double-counted.
	pendingToolNames map[string]string
}

// rawLine is the minimal transcript JSONL line shape composition.go needs.
// Deliberately local rather than reusing internal/transcript.Line: this
// package needs tool_use block ids and tool_result content, which that
// shared type doesn't carry (it's scoped to the token-usage/focus fields
// token-scraper and the recovery pass need).
type rawLine struct {
	Type      string     `json:"type"`
	UUID      string     `json:"uuid"`
	RequestID string     `json:"requestId"`
	Message   rawMessage `json:"message"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use block's own id
	Name      string          `json:"name"`        // tool_use block's tool name
	Input     json.RawMessage `json:"input"`       // tool_use block's input
	ToolUseID string          `json:"tool_use_id"` // tool_result's back-reference
	Content   json.RawMessage `json:"content"`     // tool_result's content
}

// decodeBlocks parses a message.content field into content blocks. Returns
// nil for a plain string content (no blocks to measure) or on any parse
// failure — composition is best-effort, never fatal.
func decodeBlocks(raw json.RawMessage) []rawBlock {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var blocks []rawBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// toolResultBytes measures a tool_result block's content field: the byte
// length of the underlying text, not the JSON-escaped wire form. Falls back
// to the raw field's byte length for shapes it doesn't specifically parse
// (e.g. image content) — still a reasonable size proxy for estimate-fidelity
// purposes.
func toolResultBytes(raw json.RawMessage) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return len(s)
		}
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) == nil {
			total := 0
			for _, b := range blocks {
				if b.Type == "text" {
					total += len(b.Text)
				}
			}
			return total
		}
	}
	return len(raw)
}

// walk reads any new bytes since cursorOffset and folds them into the
// running accumulators. Turns are deduped by message.id|requestId — Claude
// Code writes one JSONL line per content block, each carrying the FULL
// cumulative content array up to that point, so only the LAST line seen for
// a given turn key holds the complete, authoritative block set (see P2
// spec §1.3); earlier lines of the same turn are partial streaming
// snapshots and must not also be counted.
//
// A turn's byte counts are folded in as soon as a new turn key appears or
// at clean EOF — same "trailing group is done" assumption token-scraper
// makes (composition is estimate-fidelity, so the rare case of a poll
// landing mid-stream costs at most one turn's momentary undercount, which
// evens out over the session).
func (s *compositionState) walk() error {
	if s.pendingToolNames == nil {
		s.pendingToolNames = make(map[string]string)
	}

	f, err := os.Open(s.transcriptPath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < s.cursorOffset {
		s.cursorOffset = 0 // truncated/rotated
	}
	if fi.Size() == s.cursorOffset {
		return nil
	}
	if s.cursorOffset > 0 {
		if _, err := f.Seek(s.cursorOffset, 0); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	pos := s.cursorOffset
	var curKey string
	var curBlocks []rawBlock
	var curOutputTokens int64

	flush := func() {
		if curKey == "" {
			return
		}
		for _, b := range curBlocks {
			switch b.Type {
			case "text":
				s.textBytes += int64(len(b.Text))
			case "tool_use":
				s.toolUseBytes += int64(len(b.Input))
			}
		}
		s.outputTokens += curOutputTokens
		curKey = ""
		curBlocks = nil
		curOutputTokens = 0
	}

	for scanner.Scan() {
		raw := scanner.Bytes()
		pos += int64(len(raw)) + 1

		var line rawLine
		if json.Unmarshal(raw, &line) != nil {
			continue
		}

		switch {
		case line.Type == "assistant" || line.Message.Role == "assistant":
			blocks := decodeBlocks(line.Message.Content)
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					s.pendingToolNames[b.ID] = b.Name
				}
			}
			key := line.Message.ID + "|" + line.RequestID
			if key == "" {
				key = line.UUID
			}
			if key != curKey {
				flush()
				curKey = key
			}
			curBlocks = blocks // last-wins: latest snapshot is authoritative
			if line.Message.Usage.OutputTokens > curOutputTokens {
				curOutputTokens = line.Message.Usage.OutputTokens
			}

		case line.Type == "user" || line.Message.Role == "user":
			flush() // a user-role line only follows a fully-completed assistant turn
			for _, b := range decodeBlocks(line.Message.Content) {
				if b.Type != "tool_result" || b.ToolUseID == "" {
					continue
				}
				if _, known := s.pendingToolNames[b.ToolUseID]; known {
					s.readingBytes += int64(toolResultBytes(b.Content))
					delete(s.pendingToolNames, b.ToolUseID)
				}
			}
		}
	}
	flush()

	s.cursorOffset = pos
	return scanner.Err()
}

// percentages derives the four composition shares from cumulative totals.
// Returns nil when there is nothing to report yet (empty/not-yet-processed
// transcript). thinkingBytes is clamped to 0 when the ×4 output-token
// heuristic undershoots the measured text+tool_use bytes (a dense-text turn
// can do this) — total is then recomputed from the actual parts so the four
// percentages always sum to 1.0 rather than silently drifting.
func (s *compositionState) percentages() *compositionJSON {
	outputBytesEstimate := float64(s.outputTokens) * 4
	thinkingBytes := outputBytesEstimate - float64(s.textBytes) - float64(s.toolUseBytes)
	if thinkingBytes < 0 {
		thinkingBytes = 0
	}

	total := float64(s.textBytes) + float64(s.toolUseBytes) + thinkingBytes + float64(s.readingBytes)
	if total <= 0 {
		return nil
	}

	return &compositionJSON{
		TextPct:     round3(float64(s.textBytes) / total),
		ToolUsePct:  round3(float64(s.toolUseBytes) / total),
		ThinkingPct: round3(thinkingBytes / total),
		ReadingPct:  round3(float64(s.readingBytes) / total),
	}
}

func round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}

// findTranscriptPath locates the on-disk transcript for a (sessionID,
// agentName) pair, or an error if none is found locally — the correct,
// silent outcome for a remote session (transcript lives on a different
// host), a Codex session (lives under ~/.codex/sessions, different glob
// entirely, never matched here), or a session whose transcript has been
// cleaned up.
//
// agentName == "" is the lead: its transcript is exactly
// ~/.claude/projects/<project>/<sessionID>.jsonl. A non-empty agentName is a
// teammate — on hub/Linux, teammates share the LEAD's session_id (see
// teamster-context-bug.md), so sessionID here is the lead's id and the
// teammate's own transcript is a sibling under
// <sessionID's dir>/subagents/agent-*.jsonl. Matching is done via each
// candidate's sibling .meta.json "agentType" field (same mechanism
// cmd/token-scraper's agentNameFor uses) rather than constructing a filename
// from agentName — that's an internal naming convention this package
// shouldn't depend on staying stable.
func findTranscriptPath(sessionID, agentName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if agentName == "" {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
		if len(matches) == 0 {
			return "", fmt.Errorf("no transcript found for session %s", sessionID)
		}
		return newestFile(matches), nil
	}

	wantType := strings.TrimPrefix(agentName, "@")
	pattern := filepath.Join(home, ".claude", "projects", "*", sessionID, "subagents", "agent-*.jsonl")
	candidates, _ := filepath.Glob(pattern)

	var matches []string
	for _, c := range candidates {
		metaPath := strings.TrimSuffix(c, ".jsonl") + ".meta.json"
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta struct {
			AgentType string `json:"agentType"`
		}
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		if meta.AgentType == wantType {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no subagent transcript found for session %s agent %s", sessionID, agentName)
	}
	return newestFile(matches), nil
}

// newestFile returns the most recently modified path — the pragmatic choice
// when an agent name has been reused across multiple historical dispatches
// (each with its own hash-suffixed subagent file).
func newestFile(paths []string) string {
	best := paths[0]
	var bestMod time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(bestMod) {
			bestMod = fi.ModTime()
			best = p
		}
	}
	return best
}

// compositionTracker holds one compositionState per (sessionID, agentName)
// for the collector process's lifetime. In-memory only — a restart re-walks
// from scratch, which is fine (cumulative percentages reconverge quickly;
// there's no long-term persistence requirement for an estimate).
type compositionTracker struct {
	states map[string]*compositionState
}

func newCompositionTracker() *compositionTracker {
	return &compositionTracker{states: make(map[string]*compositionState)}
}

// Update walks new transcript bytes for (sessionID, agentName) and returns
// the current composition_json string, or nil if no transcript is available
// or there's nothing to report yet.
func (t *compositionTracker) Update(sessionID, agentName string) *string {
	key := sessionID + "|" + agentName
	st, ok := t.states[key]
	if !ok {
		st = &compositionState{}
		t.states[key] = st
	}

	if st.transcriptPath == "" {
		path, err := findTranscriptPath(sessionID, agentName)
		if err != nil {
			return nil
		}
		st.transcriptPath = path
	}

	if err := st.walk(); err != nil {
		return nil
	}

	comp := st.percentages()
	if comp == nil {
		return nil
	}
	data, err := json.Marshal(comp)
	if err != nil {
		return nil
	}
	s := string(data)
	return &s
}
