package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assistantLine(model string) string {
	return `{"type":"assistant","message":{"model":"` + model + `","usage":{"input_tokens":1,"output_tokens":1}}}`
}

// TestGetModelFromTranscript_ReflectsModelSwitch is the regression test for
// the bug this function fixes: a mid-session /model switch must be picked up
// on the very next event, not just at Stop.
func TestGetModelFromTranscript_ReflectsModelSwitch(t *testing.T) {
	p := writeTranscript(t, []string{
		assistantLine("claude-opus-4-6[1m]"),
		`{"type":"user","message":{"content":"switch please"}}`,
		assistantLine("claude-fable-5[1m]"),
	})
	if got := getModelFromTranscript(p); got != "claude-fable-5[1m]" {
		t.Fatalf("got %q, want claude-fable-5[1m]", got)
	}
}

func TestGetModelFromMetaSidecar(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent-a1.meta.json")
	if err := os.WriteFile(p, []byte(`{"agentType":"general-purpose","model":"haiku"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := getModelFromMetaSidecar(p); got != "haiku" {
		t.Fatalf("got %q, want haiku", got)
	}
}

func TestGetModelFromMetaSidecar_MissingFile(t *testing.T) {
	if got := getModelFromMetaSidecar("/nonexistent/agent-a1.meta.json"); got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestGetModelFromMetaSidecar_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent-a1.meta.json")
	if err := os.WriteFile(p, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := getModelFromMetaSidecar(p); got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

// TestProcessEvent_SubagentStart_DerivesModelFromChildTranscript is the
// regression test for the bug this WP fixes: event.AgentTranscriptPath is
// left empty by Claude Code, so the child transcript model must be derived
// from the parent's TranscriptPath + AgentID instead.
func TestProcessEvent_SubagentStart_DerivesModelFromChildTranscript(t *testing.T) {
	sessionDir := t.TempDir()
	parentTranscript := filepath.Join(sessionDir, "session1.jsonl")
	if err := os.WriteFile(parentTranscript, []byte(assistantLine("claude-opus-4-6[1m]")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subagentsDir := filepath.Join(sessionDir, "session1", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	childTranscript := filepath.Join(subagentsDir, "agent-a1.jsonl")
	if err := os.WriteFile(childTranscript, []byte(assistantLine("claude-sonnet-5")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	event := HookEvent{
		HookEventName:  "SubagentStart",
		SessionID:      "session1",
		AgentID:        "a1",
		TranscriptPath: parentTranscript,
	}
	rawData := map[string]interface{}{}
	ProcessEvent(event, rawData, unreachableServerURL(t), t.TempDir(), false)

	if got := rawData["_model"]; got != "claude-sonnet-5" {
		t.Fatalf("_model = %v, want claude-sonnet-5 (child transcript model, not parent's)", got)
	}
}

// TestProcessEvent_SubagentStart_FallsBackToMetaSidecar covers the case
// where SubagentStart fires before the child has produced any assistant
// turn — the .meta.json sidecar's launch-time model alias is used instead.
func TestProcessEvent_SubagentStart_FallsBackToMetaSidecar(t *testing.T) {
	sessionDir := t.TempDir()
	parentTranscript := filepath.Join(sessionDir, "session1.jsonl")
	if err := os.WriteFile(parentTranscript, []byte(assistantLine("claude-opus-4-6[1m]")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subagentsDir := filepath.Join(sessionDir, "session1", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(subagentsDir, "agent-a1.meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"agentType":"general-purpose","model":"haiku"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// No agent-a1.jsonl written: the child transcript doesn't exist yet.

	event := HookEvent{
		HookEventName:  "SubagentStart",
		SessionID:      "session1",
		AgentID:        "a1",
		TranscriptPath: parentTranscript,
	}
	rawData := map[string]interface{}{}
	ProcessEvent(event, rawData, unreachableServerURL(t), t.TempDir(), false)

	if got := rawData["_model"]; got != "haiku" {
		t.Fatalf("_model = %v, want haiku (from meta sidecar fallback)", got)
	}
}

// unreachableServerURL returns a URL ProcessEvent's postEvent will fail to
// reach quickly (loopback, closed port) — tests only care about rawData
// mutation, not the POST itself.
func unreachableServerURL(t *testing.T) string {
	t.Helper()
	return "http://127.0.0.1:1/event"
}

func TestGetModelFromTranscript_MissingFile(t *testing.T) {
	if got := getModelFromTranscript("/nonexistent/transcript.jsonl"); got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestGetModelFromTranscript_LargeFileOnlyReadsTail(t *testing.T) {
	lines := []string{assistantLine("claude-opus-4-6[1m]")}
	// Pad well past transcriptTailSize with plain user lines so the real
	// model line only survives if we correctly seek to the tail.
	padding := strings.Repeat("x", 200)
	for i := 0; i < 2000; i++ {
		lines = append(lines, `{"type":"user","message":{"content":"`+padding+`"}}`)
	}
	lines = append(lines, assistantLine("claude-fable-5[1m]"))
	p := writeTranscript(t, lines)

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= transcriptTailSize {
		t.Fatalf("test fixture too small (%d bytes) to exercise tail-read path", info.Size())
	}
	if got := getModelFromTranscript(p); got != "claude-fable-5[1m]" {
		t.Fatalf("got %q, want claude-fable-5[1m]", got)
	}
}
