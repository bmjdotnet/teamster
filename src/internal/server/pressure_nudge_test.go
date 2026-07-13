package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
)

func TestPressureNudgeSetAndConsume(t *testing.T) {
	var c pressureNudgeCache

	c.set("sess1", "@agent", "context is 78% full")

	msg := c.consume("sess1", "@agent")
	if msg != "context is 78% full" {
		t.Fatalf("expected queued message, got %q", msg)
	}

	// One-shot: a second consume before another set returns "".
	if msg := c.consume("sess1", "@agent"); msg != "" {
		t.Fatalf("expected empty on second consume, got %q", msg)
	}
}

func TestPressureNudgeRateLimit(t *testing.T) {
	var c pressureNudgeCache

	c.set("sess1", "@agent", "first warning")
	c.set("sess1", "@agent", "second warning — should be dropped")

	if msg := c.consume("sess1", "@agent"); msg != "first warning" {
		t.Fatalf("rate limit should have dropped the second set, got %q", msg)
	}

	// Manually age the entry past the rate-limit window (no real sleep) and
	// confirm a new set is accepted afterward.
	c.mu.Lock()
	c.state[cacheKey("sess1", "agent")].createdAt = time.Now().Add(-pressureNudgeRateLimit - time.Second)
	c.mu.Unlock()

	c.set("sess1", "@agent", "third warning — after window")
	if msg := c.consume("sess1", "@agent"); msg != "third warning — after window" {
		t.Fatalf("expected new nudge after rate-limit window elapsed, got %q", msg)
	}
}

func TestPressureNudgeClearAgent(t *testing.T) {
	var c pressureNudgeCache

	c.set("sess1", "@agent", "for agent")
	c.set("sess1", "@other", "for other")

	c.clearAgent("sess1", "@agent")

	if msg := c.consume("sess1", "@agent"); msg != "" {
		t.Fatalf("expected no pending nudge after clearAgent, got %q", msg)
	}
	if msg := c.consume("sess1", "@other"); msg != "for other" {
		t.Fatalf("clearAgent should not affect other agents, got %q", msg)
	}
}

// TestNudgeEndToEnd_DeliveredOnNextPreToolUse exercises the full wire: POST
// /nudge queues a message, then the target agent's next PreToolUse event
// receives it as additionalContext — the same path health-collector's
// nudgeDelivery adapter and a Claude Code session drive in production.
func TestNudgeEndToEnd_DeliveredOnNextPreToolUse(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "hookd-*.jsonl")
	if err != nil {
		t.Fatalf("create temp log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:     config.Config{Host: "testhost"},
		logFile: f,
		metrics: observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker(
			"testhost", 5*time.Minute, 30*time.Second, nil,
		),
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	nudgeBody, _ := json.Marshal(map[string]interface{}{
		"session_id": "sess-pressure",
		"agent_name": "@scout",
		"message":    "critical context pressure: 92% of context window used",
	})
	nudgeReq := httptest.NewRequest(http.MethodPost, "/nudge", bytes.NewReader(nudgeBody))
	nudgeRec := httptest.NewRecorder()
	s.handleNudge(nudgeRec, nudgeReq)
	if nudgeRec.Code != http.StatusOK {
		t.Fatalf("POST /nudge status = %d, want 200", nudgeRec.Code)
	}

	eventBody, _ := json.Marshal(map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-pressure",
		"agent_type":      "scout", // agentNameFor prepends "@" -> "@scout"
		"tool_name":       "Bash",
	})
	eventReq := httptest.NewRequest(http.MethodPost, "/event", bytes.NewReader(eventBody))
	eventRec := httptest.NewRecorder()
	s.handleEvent(eventRec, eventReq)
	if eventRec.Code != http.StatusOK {
		t.Fatalf("POST /event status = %d, want 200", eventRec.Code)
	}

	body, _ := io.ReadAll(eventRec.Body)
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	ctx, _ := resp["additionalContext"].(string)
	if ctx != "critical context pressure: 92% of context window used" {
		t.Fatalf("additionalContext = %q, want the queued pressure message", ctx)
	}

	// One-shot: a second PreToolUse for the same agent must not repeat it.
	eventReq2 := httptest.NewRequest(http.MethodPost, "/event", bytes.NewReader(eventBody))
	eventRec2 := httptest.NewRecorder()
	s.handleEvent(eventRec2, eventReq2)
	body2, _ := io.ReadAll(eventRec2.Body)
	var resp2 map[string]interface{}
	if err := json.Unmarshal(body2, &resp2); err != nil {
		t.Fatalf("unmarshal second response: %v", err)
	}
	if ctx2, ok := resp2["additionalContext"].(string); ok && ctx2 != "" {
		t.Fatalf("expected no additionalContext on second PreToolUse, got %q", ctx2)
	}
}
