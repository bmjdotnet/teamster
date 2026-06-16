package llm

import (
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/transcript"
)

func TestBuildPrompt_IncludesTranscript(t *testing.T) {
	req := SynthesisRequest{
		SessionID: "sess-123",
		Transcript: []transcript.TranscriptLine{
			{Role: "user", Content: "fix the auth bug", Timestamp: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)},
			{Role: "assistant", Content: "I'll look at the auth module", Timestamp: time.Date(2026, 6, 10, 12, 0, 30, 0, time.UTC)},
		},
		TagVocab: "product: Teamster, SecondBrain\nwork-type: feature, bug, refactor\n",
	}

	prompt := buildPrompt(req)

	if !strings.Contains(prompt, "sess-123") {
		t.Fatal("prompt missing session_id")
	}
	if !strings.Contains(prompt, "fix the auth bug") {
		t.Fatal("prompt missing transcript content")
	}
	if !strings.Contains(prompt, "Teamster") {
		t.Fatal("prompt missing tag vocabulary")
	}
	if !strings.Contains(prompt, "outcome_id") {
		t.Fatal("prompt missing response format instructions")
	}
}

func TestBuildPrompt_TruncatesLongContent(t *testing.T) {
	long := strings.Repeat("x", 1000)
	req := SynthesisRequest{
		SessionID: "s1",
		Transcript: []transcript.TranscriptLine{
			{Role: "user", Content: long, Timestamp: time.Now()},
		},
	}
	prompt := buildPrompt(req)
	if strings.Contains(prompt, long) {
		t.Fatal("prompt should truncate long content")
	}
	if !strings.Contains(prompt, "...") {
		t.Fatal("prompt should include ellipsis for truncated content")
	}
}

func TestParseResponse_ValidJSON(t *testing.T) {
	input := `{
		"outcome_id": "out-fix-auth",
		"title": "Fix auth bug",
		"description": "Fixed the authentication module",
		"product": "Teamster",
		"work_type": "bug",
		"feature_or_bug": "bug",
		"feature_bug_slug": "auth-failure",
		"component": "auth",
		"priority": "p1",
		"confidence": "high",
		"evidence_excerpt": "user asked to fix the auth bug"
	}`

	resp, err := parseResponse(input)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.OutcomeID != "out-fix-auth" {
		t.Fatalf("outcome_id=%q, want out-fix-auth", resp.OutcomeID)
	}
	if resp.Title != "Fix auth bug" {
		t.Fatalf("title=%q, want 'Fix auth bug'", resp.Title)
	}
	if resp.WorkType != "bug" {
		t.Fatalf("work_type=%q, want bug", resp.WorkType)
	}
	if resp.Confidence != "high" {
		t.Fatalf("confidence=%q, want high", resp.Confidence)
	}
}

func TestParseResponse_MarkdownFenced(t *testing.T) {
	input := "```json\n{\"outcome_id\":\"out-x\",\"title\":\"X\",\"description\":\"d\",\"confidence\":\"high\"}\n```"
	resp, err := parseResponse(input)
	if err != nil {
		t.Fatalf("parseResponse with fencing: %v", err)
	}
	if resp.OutcomeID != "out-x" {
		t.Fatalf("outcome_id=%q, want out-x", resp.OutcomeID)
	}
}

func TestParseResponse_MissingRequiredFields(t *testing.T) {
	input := `{"outcome_id":"","title":"","description":"test"}`
	_, err := parseResponse(input)
	if err == nil {
		t.Fatal("expected error for missing outcome_id/title")
	}
}

func TestParseResponse_InvalidJSON(t *testing.T) {
	_, err := parseResponse("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate short=%q, want 'short'", got)
	}
	if got := truncate("longer string", 5); got != "longe..." {
		t.Fatalf("truncate long=%q, want 'longe...'", got)
	}
}
