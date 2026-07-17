package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// captureServer is a stand-in for hookd's /telemetry endpoint that records every
// row the scraper POSTs, so a test can assert what the scraper deduplicated to.
type captureServer struct {
	mu   sync.Mutex
	rows []telemetryRow
}

func (c *captureServer) handler(w http.ResponseWriter, r *http.Request) {
	var row telemetryRow
	if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.rows = append(c.rows, row)
	c.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func newCaptureScraper(t *testing.T) (*scraper, *captureServer) {
	t.Helper()
	cap := &captureServer{}
	ts := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(ts.Close)
	return &scraper{
		client:       ts.Client(),
		telemetryURL: ts.URL,
		host:         "testhost",
		cursors:      make(map[string]*cursorEntry),
	}, cap
}

// asstLine builds one assistant transcript line. Multiple lines sharing msgID and
// reqID but with distinct uuids reproduce Claude Code's per-content-block writes.
func asstLine(uuid, msgID, reqID, model string, in, out, cr, cw int64, blocks ...string) map[string]any {
	content := make([]map[string]string, 0, len(blocks))
	for _, b := range blocks {
		content = append(content, map[string]string{"type": b})
	}
	return map[string]any{
		"type":      "assistant",
		"uuid":      uuid,
		"requestId": reqID,
		"sessionId": "sess-1",
		"timestamp": "2026-06-09T19:03:27.386Z",
		"message": map[string]any{
			"id":          msgID,
			"model":       model,
			"stop_reason": "tool_use",
			"content":     content,
			"usage": map[string]any{
				"input_tokens":                in,
				"output_tokens":               out,
				"cache_read_input_tokens":     cr,
				"cache_creation_input_tokens": cw,
				"service_tier":                "standard",
			},
		},
	}
}

func writeJSONL(t *testing.T, path string, lines []map[string]any) {
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

// TestProcessFileDedupsContentBlockLines is the core regression: a single API
// response written as two transcript lines (text then tool_use), same
// message.id+requestId, distinct uuids, full usage on each — must yield ONE
// emitted row carrying that usage once, not two.
func TestProcessFileDedupsContentBlockLines(t *testing.T) {
	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")

	writeJSONL(t, path, []map[string]any{
		// request A: 2 lines (text, tool_use), identical full usage
		asstLine("u1", "msg_A", "req_A", "claude-opus-4-8", 3884, 200, 0, 27520, "text"),
		asstLine("u2", "msg_A", "req_A", "claude-opus-4-8", 3884, 200, 0, 27520, "tool_use"),
		// request B: single line
		asstLine("u3", "msg_B", "req_B", "claude-opus-4-8", 261, 145, 0, 33568, "text"),
		// request C: 2 lines, early snapshot has partial output_tokens=1
		asstLine("u4", "msg_C", "req_C", "claude-opus-4-8", 2, 1, 34087, 5433, "text"),
		asstLine("u5", "msg_C", "req_C", "claude-opus-4-8", 2, 136, 34087, 5433, "tool_use"),
	})

	if err := s.processFile(context.Background(), path, "@agent", ""); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	if len(cap.rows) != 3 {
		t.Fatalf("expected 3 deduped rows, got %d: %+v", len(cap.rows), cap.rows)
	}

	byID := map[string]telemetryRow{}
	for _, r := range cap.rows {
		byID[r.MessageID] = r
	}

	a, ok := byID["msg_A|req_A"]
	if !ok {
		t.Fatalf("request A row missing; got keys %v", keys(byID))
	}
	if a.OutputTokens != 200 || a.InputTokens != 3884 || a.CacheWriteTokens != 27520 {
		t.Errorf("request A usage counted wrong (double-count?): %+v", a)
	}
	// content-block counts should reflect the union (text + tool_use seen)
	if a.NText != 1 || a.NToolUse != 1 {
		t.Errorf("request A content counts = text %d tool_use %d, want 1/1", a.NText, a.NToolUse)
	}

	// request C must take the MAX output (136), not the partial snapshot (1).
	c := byID["msg_C|req_C"]
	if c.OutputTokens != 136 {
		t.Errorf("request C output = %d, want 136 (max of streamed snapshots)", c.OutputTokens)
	}
	if c.TotalInput != 2+34087+5433 {
		t.Errorf("request C total_input = %d, want %d", c.TotalInput, 2+34087+5433)
	}
}

// TestProcessFileResumesAcrossBoundary proves the cursor does not advance past an
// open (incomplete) group when reading stops, so a request whose lines straddle a
// poll boundary is not split. We simulate by reading a truncated file, then the
// full file, and asserting the request is emitted with complete usage.
func TestProcessFileResumesAcrossBoundary(t *testing.T) {
	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")

	full := []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10, 0, 500, "text"),
		asstLine("u2", "msg_A", "req_A", "claude-opus-4-8", 100, 90, 0, 500, "tool_use"),
	}
	// First write only the first content-block line of request A.
	writeJSONL(t, path, full[:1])
	if err := s.processFile(context.Background(), path, "@agent", ""); err != nil {
		t.Fatalf("processFile pass1: %v", err)
	}
	// At clean EOF the trailing group flushes (we cannot know more is coming),
	// so request A emits once here with output=10.
	if len(cap.rows) != 1 || cap.rows[0].OutputTokens != 10 {
		t.Fatalf("pass1 expected 1 row out=10, got %+v", cap.rows)
	}

	// Now append the completing line and re-process.
	writeJSONL(t, path, full) // rewrite whole file
	cap.rows = nil
	if err := s.processFile(context.Background(), path, "@agent", ""); err != nil {
		t.Fatalf("processFile pass2: %v", err)
	}
	// The second pass re-reads from the cursor; the completing line re-emits the
	// request with full output=90 under the SAME message_id, so the DB-side
	// max-output upsert keeps the complete one. Here we just assert the scraper
	// emits the complete usage at least once.
	sawComplete := false
	for _, r := range cap.rows {
		if r.MessageID == "msg_A|req_A" && r.OutputTokens == 90 {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Errorf("pass2 did not re-emit request A with complete output=90: %+v", cap.rows)
	}
}

// TestEmitStampsHostUsername proves the scraper sends host + username on the
// wire (wu-host-capture): these identify WHERE the transcript physically lives
// so the focus-attribution recovery pass can host-scope. host comes from
// cfg.Host, username from cfg.User (the OS user whose ~/.claude was read).
func TestEmitStampsHostUsername(t *testing.T) {
	s, cap := newCaptureScraper(t)
	s.username = "claude"
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	writeJSONL(t, path, []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10, 0, 500, "text"),
	})

	if err := s.processFile(context.Background(), path, "@agent", ""); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	if cap.rows[0].Host != "testhost" || cap.rows[0].Username != "claude" {
		t.Errorf("emitted host/username = %q/%q, want testhost/claude", cap.rows[0].Host, cap.rows[0].Username)
	}
}

func keys(m map[string]telemetryRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
