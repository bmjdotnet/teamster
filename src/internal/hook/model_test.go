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
