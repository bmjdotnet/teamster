package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeSettingsModel writes a ~/.claude/settings.json with the given "model"
// value under a temp HOME, and points os.UserHomeDir() (via $HOME) at it for
// the duration of the test.
func writeSettingsModel(t *testing.T, model string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"model":"` + model + `"}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	t.Setenv("HOME", home)
}

// TestProcessFileAppliesLongContextSuffixToMainSession is the core regression
// for wu-1m-suffix: Claude Code's API never echoes a "[1m]" long-context-beta
// annotation back into a transcript's message.model field — that annotation
// only exists in ~/.claude/settings.json's "model" value. The main session
// file (agentName == "") must have "[1m]" appended to token_ledger.model when
// the configured model (minus suffix) matches the transcript's model, so
// health-collector's contextWindowForModel (cmd/health-collector/main.go) can
// detect the 1M context window.
func TestProcessFileAppliesLongContextSuffixToMainSession(t *testing.T) {
	writeSettingsModel(t, "claude-fable-5[1m]")

	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	writeJSONL(t, path, []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-fable-5", 100, 10, 0, 500, "text"),
	})

	if err := s.processFile(context.Background(), path, ""); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	if got := cap.rows[0].Model; got != "claude-fable-5[1m]" {
		t.Errorf("model = %q, want %q", got, "claude-fable-5[1m]")
	}
}

// TestProcessFileLongContextSuffixNotAppliedToSubagent proves the suffix is
// scoped to the main (lead) session file only. A subagent may be dispatched on
// a completely different model than the lead's configured "[1m]" model, so
// blindly applying the lead's setting to every subagent row would reproduce
// the same last-wins misattribution documented in teamster-context-bug.md,
// just for a different field.
func TestProcessFileLongContextSuffixNotAppliedToSubagent(t *testing.T) {
	writeSettingsModel(t, "claude-fable-5[1m]")

	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	writeJSONL(t, path, []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-fable-5", 100, 10, 0, 500, "text"),
	})

	if err := s.processFile(context.Background(), path, "@teammate"); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	if got := cap.rows[0].Model; got != "claude-fable-5" {
		t.Errorf("model = %q, want %q (no suffix on subagent rows)", got, "claude-fable-5")
	}
}

// TestProcessFileLongContextSuffixRequiresBaseMatch proves the suffix is only
// applied when the transcript's own model matches the configured model's
// base name — a lead session running a model other than the one currently
// configured in settings.json (e.g. after a live /model switch) must not be
// mislabeled as long-context.
func TestProcessFileLongContextSuffixRequiresBaseMatch(t *testing.T) {
	writeSettingsModel(t, "claude-fable-5[1m]")

	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	writeJSONL(t, path, []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10, 0, 500, "text"),
	})

	if err := s.processFile(context.Background(), path, ""); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	if got := cap.rows[0].Model; got != "claude-opus-4-8" {
		t.Errorf("model = %q, want %q (unrelated model untouched)", got, "claude-opus-4-8")
	}
}

// TestProcessFileNoSuffixWithoutLongContextConfig proves the ordinary case
// (no "[1m]" in settings.json) is untouched — this is the common path for
// every session not running the 1M-context beta.
func TestProcessFileNoSuffixWithoutLongContextConfig(t *testing.T) {
	writeSettingsModel(t, "claude-opus-4-8")

	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	writeJSONL(t, path, []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10, 0, 500, "text"),
	})

	if err := s.processFile(context.Background(), path, ""); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	if got := cap.rows[0].Model; got != "claude-opus-4-8" {
		t.Errorf("model = %q, want %q", got, "claude-opus-4-8")
	}
}
