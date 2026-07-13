package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTranscriptLines(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
}

// asstTurn builds the lines for one assistant turn: an early partial
// streaming snapshot (text only) followed by the final line carrying the
// complete cumulative content array — mirroring Claude Code's real
// per-content-block-line writes, where only the LAST line of a turn holds
// the full block set.
func asstTurn(msgID, reqID string, outputTokens int64, textFinal string, toolName, toolInput, toolID string) []map[string]any {
	partial := map[string]any{
		"type": "assistant",
		"uuid": msgID + "-partial",
		"message": map[string]any{
			"id":      msgID,
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": textFinal[:min(len(textFinal), 3)]}},
			"usage":   map[string]any{"output_tokens": 1},
		},
		"requestId": reqID,
	}
	final := map[string]any{
		"type": "assistant",
		"uuid": msgID + "-final",
		"message": map[string]any{
			"id":   msgID,
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": textFinal},
				{"type": "tool_use", "id": toolID, "name": toolName, "input": map[string]any{"raw": toolInput}},
			},
			"usage": map[string]any{"output_tokens": outputTokens},
		},
		"requestId": reqID,
	}
	return []map[string]any{partial, final}
}

func userToolResult(toolID, resultText string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": toolID, "content": resultText},
			},
		},
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestWalkDedupsPartialSnapshotsAndSumsToolResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	var lines []map[string]any
	lines = append(lines, asstTurn("msg_A", "req_A", 100, "hello world this is the final text", "Read", "file.go", "tool_1")...)
	lines = append(lines, userToolResult("tool_1", "file contents here, moderately long output from the read"))

	writeTranscriptLines(t, path, lines)

	s := &compositionState{transcriptPath: path}
	if err := s.walk(); err != nil {
		t.Fatalf("walk: %v", err)
	}

	wantText := int64(len("hello world this is the final text"))
	if s.textBytes != wantText {
		t.Errorf("textBytes = %d, want %d (partial snapshot must not be double-counted)", s.textBytes, wantText)
	}

	// tool_use bytes come from json.Marshal of {"raw":"file.go"} as written in
	// the fixture's input map — just assert it's non-zero and plausible.
	if s.toolUseBytes == 0 {
		t.Errorf("toolUseBytes = 0, want > 0")
	}

	wantReading := int64(len("file contents here, moderately long output from the read"))
	if s.readingBytes != wantReading {
		t.Errorf("readingBytes = %d, want %d", s.readingBytes, wantReading)
	}

	if s.outputTokens != 100 {
		t.Errorf("outputTokens = %d, want 100 (max across the turn's snapshots)", s.outputTokens)
	}
}

func TestWalkResumesFromCursorWithoutDoubleCounting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	writeTranscriptLines(t, path, asstTurn("msg_A", "req_A", 50, "first turn text", "Bash", "ls", "tool_1"))

	s := &compositionState{transcriptPath: path}
	if err := s.walk(); err != nil {
		t.Fatalf("walk pass1: %v", err)
	}
	firstText := s.textBytes
	firstTokens := s.outputTokens

	// Append a second, distinct turn.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, l := range asstTurn("msg_B", "req_B", 60, "second turn text here", "Grep", "pattern", "tool_2") {
		if err := enc.Encode(l); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	f.Close()

	if err := s.walk(); err != nil {
		t.Fatalf("walk pass2: %v", err)
	}

	if s.textBytes <= firstText {
		t.Errorf("textBytes did not grow after second turn: %d -> %d", firstText, s.textBytes)
	}
	wantTotalText := int64(len("first turn text")) + int64(len("second turn text here"))
	if s.textBytes != wantTotalText {
		t.Errorf("textBytes = %d, want %d (first turn re-counted? cursor not advancing correctly)", s.textBytes, wantTotalText)
	}
	if s.outputTokens != firstTokens+60 {
		t.Errorf("outputTokens = %d, want %d", s.outputTokens, firstTokens+60)
	}
}

func TestPercentagesSumToOne(t *testing.T) {
	s := &compositionState{
		textBytes:    350,
		toolUseBytes: 250,
		outputTokens: 200, // ×4 = 800 output-byte budget
		readingBytes: 100,
	}
	comp := s.percentages()
	if comp == nil {
		t.Fatal("percentages() = nil, want a value")
	}
	sum := comp.TextPct + comp.ToolUsePct + comp.ThinkingPct + comp.ReadingPct
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("percentages sum = %v, want ~1.0 (%+v)", sum, comp)
	}
	if comp.ThinkingPct <= 0 {
		t.Errorf("expected a positive thinking residual, got %+v", comp)
	}
}

func TestPercentagesClampsThinkingWhenHeuristicUndershoots(t *testing.T) {
	// text+tool_use bytes exceed output_tokens×4 (a dense-text turn) — the
	// residual would go negative without the clamp.
	s := &compositionState{
		textBytes:    900,
		toolUseBytes: 200,
		outputTokens: 100, // ×4 = 400, less than 900+200
		readingBytes: 50,
	}
	comp := s.percentages()
	if comp == nil {
		t.Fatal("percentages() = nil")
	}
	if comp.ThinkingPct < 0 {
		t.Errorf("ThinkingPct = %v, want >= 0 (clamped)", comp.ThinkingPct)
	}
	sum := comp.TextPct + comp.ToolUsePct + comp.ThinkingPct + comp.ReadingPct
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("percentages sum = %v, want ~1.0 even after clamping (%+v)", sum, comp)
	}
}

func TestPercentagesNilWhenEmpty(t *testing.T) {
	s := &compositionState{}
	if got := s.percentages(); got != nil {
		t.Errorf("percentages() on empty state = %+v, want nil", got)
	}
}

func TestToolResultBytesShapes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"plain string", `"hello world"`, len("hello world")},
		{"text block array", `[{"type":"text","text":"abc"}]`, 3},
		{"mixed array counts only text", `[{"type":"text","text":"abcd"},{"type":"image","source":"x"}]`, 4},
		{"empty", ``, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolResultBytes(json.RawMessage(tt.raw))
			if got != tt.want {
				t.Errorf("toolResultBytes(%s) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFindTranscriptPathLead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projDir := filepath.Join(home, ".claude", "projects", "-mnt-ai-gh")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.Join(projDir, "sess-123.jsonl")
	if err := os.WriteFile(sessPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findTranscriptPath("sess-123", "")
	if err != nil {
		t.Fatalf("findTranscriptPath: %v", err)
	}
	if got != sessPath {
		t.Errorf("findTranscriptPath = %q, want %q", got, sessPath)
	}
}

func TestFindTranscriptPathSubagentMatchesByMetaAgentType(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	subDir := filepath.Join(home, ".claude", "projects", "-mnt-ai-gh", "sess-lead", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two candidates: one for "engine", one for "store" — must match the
	// right one via .meta.json, not by guessing the filename.
	enginePath := filepath.Join(subDir, "agent-axyz123.jsonl")
	storePath := filepath.Join(subDir, "agent-aabc999.jsonl")
	os.WriteFile(enginePath, []byte("{}\n"), 0o644)
	os.WriteFile(storePath, []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(subDir, "agent-axyz123.meta.json"), []byte(`{"agentType":"engine"}`), 0o644)
	os.WriteFile(filepath.Join(subDir, "agent-aabc999.meta.json"), []byte(`{"agentType":"store"}`), 0o644)

	got, err := findTranscriptPath("sess-lead", "@engine")
	if err != nil {
		t.Fatalf("findTranscriptPath: %v", err)
	}
	if got != enginePath {
		t.Errorf("findTranscriptPath(@engine) = %q, want %q", got, enginePath)
	}
}

func TestFindTranscriptPathNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := findTranscriptPath("no-such-session", ""); err == nil {
		t.Error("expected an error for a session with no local transcript (e.g. remote or Codex session)")
	}
}

func TestCompositionTrackerUpdateReturnsNilWithoutTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tracker := newCompositionTracker()
	if got := tracker.Update("no-such-session", ""); got != nil {
		t.Errorf("Update() = %v, want nil (no transcript found)", *got)
	}
}

func TestCompositionTrackerUpdateProducesJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projDir := filepath.Join(home, ".claude", "projects", "-mnt-ai-gh")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.Join(projDir, "sess-123.jsonl")
	writeTranscriptLines(t, sessPath, asstTurn("msg_A", "req_A", 100, "some assistant text here", "Bash", "ls -la", "tool_1"))

	tracker := newCompositionTracker()
	got := tracker.Update("sess-123", "")
	if got == nil {
		t.Fatal("Update() = nil, want a composition JSON string")
	}
	var comp compositionJSON
	if err := json.Unmarshal([]byte(*got), &comp); err != nil {
		t.Fatalf("unmarshal composition JSON: %v (raw=%s)", err, *got)
	}
	if comp.TextPct <= 0 {
		t.Errorf("comp.TextPct = %v, want > 0", comp.TextPct)
	}
}
