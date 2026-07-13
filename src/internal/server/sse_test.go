package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
)

func newSSETestServer(t *testing.T, jsonlLines ...string) *Server {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "hookd-*.jsonl")
	if err != nil {
		t.Fatalf("create temp log: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	for _, line := range jsonlLines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("write jsonl fixture: %v", err)
		}
	}

	s := &Server{
		cfg:     config.Config{Host: "testhost", LogFile: f.Name()},
		logFile: f,
		metrics: observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker(
			"testhost", 5*time.Minute, 30*time.Second, nil,
		),
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)
	return s
}

// requestWithCanceledContext builds a GET request whose context is already
// canceled, so handleSSE's history burst runs but the live-subscription
// select loop exits immediately on <-ctx.Done() instead of blocking forever
// — the history branch is what's under test here, not the live-publish path
// (see TestHandleSSE_LivePublish_FormatSelectsPayload for that).
func requestWithCanceledContext(target string) *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
}

func TestHandleSSE_DefaultFormat_HistoryIsHTML(t *testing.T) {
	line := `{"ts":"2026-07-11T15:04:22Z","tag":"EXEC","display":"go test ./...","agent_name":"@scout","session_id":"sess-1"}`
	s := newSSETestServer(t, line)

	req := requestWithCanceledContext("/events/stream?history=1")
	rec := httptest.NewRecorder()
	s.handleSSE(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("expected an SSE data frame, got: %q", body)
	}
	// Default format must be the htmx-ready HTML snippet, not raw JSON — a
	// raw JSONL line always starts with '{' right after "data: ".
	if strings.HasPrefix(body, "data: {") {
		t.Fatalf("default format leaked raw JSON instead of HTML: %q", body)
	}
	if !strings.Contains(body, "go test") {
		t.Fatalf("expected rendered display text in HTML output, got: %q", body)
	}
}

func TestHandleSSE_FormatJSON_HistoryIsRawJSONL(t *testing.T) {
	line := `{"ts":"2026-07-11T15:04:22Z","tag":"EXEC","display":"go test ./...","agent_name":"@scout","session_id":"sess-1"}`
	s := newSSETestServer(t, line)

	req := requestWithCanceledContext("/events/stream?history=1&format=json")
	rec := httptest.NewRecorder()
	s.handleSSE(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {") {
		t.Fatalf("format=json history must emit raw JSON, got: %q", body)
	}

	// Extract the single "data: <json>\n\n" frame and confirm it parses as
	// the exact fixture record — this is the "must parse as JSONL" guarantee
	// ctop's SSE client depends on.
	payload := strings.TrimPrefix(strings.TrimSpace(body), "data: ")
	var rec2 map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &rec2); err != nil {
		t.Fatalf("format=json frame did not parse as JSON: %v (payload: %s)", err, payload)
	}
	if rec2["agent_name"] != "@scout" {
		t.Fatalf("agent_name = %v, want @scout", rec2["agent_name"])
	}
}

// TestHandleSSE_LivePublish_FormatSelectsPayload exercises the actual bug
// surface of this change: ssePayload must carry both representations through
// the live bus, and handleSSE must pick the right one per ?format=, without
// altering the default (html) behavior other subscribers rely on.
func TestHandleSSE_LivePublish_FormatSelectsPayload(t *testing.T) {
	s := newSSETestServer(t)

	htmlWant := "<div class=\"event\">rendered</div>"
	rawWant := `{"agent_name":"@scout","tag":"EXEC"}`

	run := func(target string) string {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			s.handleSSE(rec, req)
			close(done)
		}()

		waitForSubscriberCount(t, &s.bus, 1, 2*time.Second)
		s.bus.publish(ssePayload{html: []byte(htmlWant), raw: []byte(rawWant)})

		// Give the select loop one scheduling window to consume the publish
		// before we cancel — cancel racing the read is fine either way for
		// this assertion (worst case: zero frames, which the checks below
		// would catch), but in practice this reliably lets the frame land.
		time.Sleep(20 * time.Millisecond)
		cancel()
		<-done
		return rec.Body.String()
	}

	htmlBody := run("/events/stream")
	if !strings.Contains(htmlBody, htmlWant) {
		t.Fatalf("default format: expected html payload in body, got: %q", htmlBody)
	}
	if strings.Contains(htmlBody, rawWant) {
		t.Fatalf("default format: raw payload leaked into html body: %q", htmlBody)
	}

	jsonBody := run("/events/stream?format=json")
	if !strings.Contains(jsonBody, rawWant) {
		t.Fatalf("format=json: expected raw payload in body, got: %q", jsonBody)
	}
	if strings.Contains(jsonBody, htmlWant) {
		t.Fatalf("format=json: html payload leaked into json body: %q", jsonBody)
	}
}

func waitForSubscriberCount(t *testing.T, b *eventBus, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b.mu.RLock()
		count := len(b.subscribers)
		b.mu.RUnlock()
		if count >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s)", n)
}
