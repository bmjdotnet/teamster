// Package llm provides prompt construction and response parsing for the nightly
// attribution sweep. The actual LLM invocation uses claude --print (Claude Code
// headless mode), not the Anthropic API directly.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/transcript"
)

const defaultModel = "claude-sonnet-4-6"

type SynthesisRequest struct {
	SessionID  string
	Transcript []transcript.TranscriptLine
	TagVocab   string
}

type SynthesisResponse struct {
	OutcomeID       string `json:"outcome_id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	Product         string `json:"product"`
	WorkType        string `json:"work_type"`
	Phase           string `json:"phase"`
	FeatureOrBug    string `json:"feature_or_bug"`
	FeatureBugSlug  string `json:"feature_bug_slug"`
	Component       string `json:"component"`
	Priority        string `json:"priority"`
	Confidence      string `json:"confidence"`
	EvidenceExcerpt string `json:"evidence_excerpt"`
}

// SynthesizeOutcome invokes claude --print with the synthesis prompt and parses
// the JSON response. Requires claude CLI on PATH (uses the user's existing
// Claude Code authentication, not ANTHROPIC_API_KEY).
func SynthesizeOutcome(ctx context.Context, req SynthesisRequest) (*SynthesisResponse, error) {
	prompt := buildPrompt(req)

	model := os.Getenv("TEAMSTER_SWEEP_MODEL")
	if model == "" {
		model = defaultModel
	}

	args := []string{"--print", "-p", prompt, "--model", model, "--output-format", "text"}
	cmd := exec.CommandContext(ctx, "claude", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude --print failed: %w (stderr: %s)", err, stderr.String())
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return nil, fmt.Errorf("empty response from claude --print")
	}

	return parseResponse(text)
}

func buildPrompt(req SynthesisRequest) string {
	var sb strings.Builder

	sb.WriteString("You are analyzing a Claude Code session transcript to synthesize a WMS Outcome.\n\n")
	sb.WriteString("## Task\n")
	sb.WriteString("Read the transcript excerpt below and determine:\n")
	sb.WriteString("1. What the session's objective was (the Outcome)\n")
	sb.WriteString("2. How to classify it using the existing tag vocabulary\n\n")

	sb.WriteString("## Existing Tag Vocabulary (REUSE existing values, do not invent near-duplicates)\n")
	if req.TagVocab != "" {
		sb.WriteString(req.TagVocab)
	} else {
		sb.WriteString("(no vocabulary loaded)\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Transcript Excerpt (session_id: ")
	sb.WriteString(req.SessionID)
	sb.WriteString(")\n```\n")
	for _, line := range req.Transcript {
		fmt.Fprintf(&sb, "[%s] %s: %s\n",
			line.Timestamp.Format(time.RFC3339), line.Role,
			truncate(line.Content, 500))
	}
	sb.WriteString("```\n\n")

	sb.WriteString(`## Response Format
Respond with ONLY a JSON object (no markdown fencing, no explanation):
{
  "outcome_id": "out-<short-kebab-slug>",
  "title": "<concise outcome title>",
  "description": "<1-2 sentence description of what the session accomplished>",
  "product": "<existing product tag value, e.g. 'Teamster'>",
  "work_type": "<feature|bug|refactor|infra|research|docs|test>",
  "phase": "<design|build|test|review|rework — the lifecycle phase: design=investigation/planning, build=implementation/fixing/deploying, test=testing/validation, review=auditing/evaluating existing work, rework=redoing previously completed work>",
  "feature_or_bug": "<'feature' or 'bug' — which context key to use>",
  "feature_bug_slug": "<the value for the feature or bug tag>",
  "component": "<component if identifiable, else empty string>",
  "priority": "<p0|p1|p2|p3>",
  "confidence": "<high|medium|low>",
  "evidence_excerpt": "<1-2 lines from the transcript that justify this classification>"
}
`)

	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func parseResponse(text string) (*SynthesisResponse, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		lines := strings.SplitN(text, "\n", 2)
		if len(lines) > 1 {
			text = lines[1]
		}
		if idx := strings.LastIndex(text, "```"); idx >= 0 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
	}

	var resp SynthesisResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		return nil, fmt.Errorf("parse synthesis JSON: %w (raw: %s)", err, truncate(text, 200))
	}

	if resp.OutcomeID == "" || resp.Title == "" {
		return nil, fmt.Errorf("synthesis response missing outcome_id or title")
	}

	return &resp, nil
}
