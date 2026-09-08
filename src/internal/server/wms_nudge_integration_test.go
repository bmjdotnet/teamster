package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "hookd-*.jsonl")
	if err != nil {
		t.Fatalf("create temp log: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	s := &Server{
		cfg:     config.Config{Host: "testhost"},
		logFile: f,
		metrics: observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker(
			"testhost", 5*time.Minute, 30*time.Second, nil,
		),
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)
	return s
}

func postEvent(t *testing.T, s *Server, event map[string]interface{}) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(event)
	req := httptest.NewRequest("POST", "/event", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleEvent(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /event status = %d, want 200", rec.Code)
	}
	respBody, _ := io.ReadAll(rec.Body)
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

func TestOrphanDispatch_AppearsInAdditionalContext(t *testing.T) {
	s := newTestServer(t)

	resp := postEvent(t, s, map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-orphan",
		"tool_name":       "SendMessage",
	})

	ctx, _ := resp["additionalContext"].(string)
	if !strings.Contains(ctx, "orphan dispatch") {
		t.Fatalf("additionalContext = %q, want orphan dispatch warning", ctx)
	}
}

func TestWMSWarningQueue_DeliveredOnNextPreToolUse(t *testing.T) {
	s := newTestServer(t)

	s.wmsWarnings.queue("sess-closeout", "@scout",
		"[WMS] workunit wu-123 closed without required tags: component, work-type — add them with wms_tagEntity")

	resp := postEvent(t, s, map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-closeout",
		"agent_type":      "scout",
		"tool_name":       "Bash",
	})

	ctx, _ := resp["additionalContext"].(string)
	if !strings.Contains(ctx, "wu-123") {
		t.Fatalf("additionalContext = %q, want queued close-out warning", ctx)
	}

	resp2 := postEvent(t, s, map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-closeout",
		"agent_type":      "scout",
		"tool_name":       "Bash",
	})
	ctx2, _ := resp2["additionalContext"].(string)
	if strings.Contains(ctx2, "wu-123") {
		t.Fatalf("warning should be one-shot, but appeared again: %q", ctx2)
	}
}

func TestWMSWarningQueue_DeliveredOnUserPromptSubmit(t *testing.T) {
	s := newTestServer(t)

	s.wmsWarnings.queue("sess-prompt", "", "[WMS] workunit wu-456 missing tags")

	resp := postEvent(t, s, map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      "sess-prompt",
	})

	ctx, _ := resp["additionalContext"].(string)
	if !strings.Contains(ctx, "wu-456") {
		t.Fatalf("additionalContext = %q, want queued warning on UserPromptSubmit", ctx)
	}
}
