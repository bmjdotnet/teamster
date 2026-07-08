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

	"github.com/bmjdotnet/teamster/internal/store"
)

// captureServer stands in for hookd's /telemetry endpoint, recording every
// row POSTed so a test can assert what the tailer derived.
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

// fakeUpserter records every UpsertSession call so a test can assert the
// tailer's session-ownership behavior without a real store connection.
type fakeUpserter struct {
	mu    sync.Mutex
	calls []store.Session
}

func (f *fakeUpserter) UpsertSession(_ context.Context, s store.Session) error {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	return nil
}

func newTestScraper(t *testing.T) (*scraper, *captureServer, *fakeUpserter) {
	t.Helper()
	cap := &captureServer{}
	ts := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(ts.Close)
	up := &fakeUpserter{}
	return &scraper{
		client:       ts.Client(),
		telemetryURL: ts.URL,
		host:         "testhost",
		username:     "testuser",
		cursors:      make(map[string]*cursorEntry),
		st:           up,
	}, cap, up
}

// TestProcessFile_ResumedRollout is the binding fixture test (redteam
// rollout-after-resume.jsonl): a session that ran one turn, then was resumed
// via `codex exec resume` for a second turn, all appended to the SAME file.
// Verifies: (1) the tailer emits one ledger row per token_count event using
// last_token_usage — never the cumulative total_token_usage, so an unrelated
// resumed continuation must not inflate/double-count the original turn's
// usage; (2) the sessions row is upserted with the identity carried in
// session_meta/turn_context, runtime=codex.
func TestProcessFile_ResumedRollout(t *testing.T) {
	s, cap, up := newTestScraper(t)
	path, err := filepath.Abs("testdata/redteam-rollout-after-resume.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	if len(cap.rows) != 3 {
		t.Fatalf("expected 3 ledger rows (one per token_count event), got %d: %+v", len(cap.rows), cap.rows)
	}

	wantSessionID := "019f3b4a-3808-7fa3-bc1d-e99cdc0f1f4e"
	type want struct{ input, output, cacheRead int64 }
	// input/cacheRead/output are the CORRECTED (non-additive) derivation:
	// cached_input_tokens is a subset of the fixture's raw input_tokens, so
	// input here is (raw input_tokens - cached_input_tokens); output is raw
	// output_tokens as-is (reasoning_output_tokens, 0 in this fixture, is
	// already inside it, never added). Raw last_token_usage values:
	// (23215,2432,36), (23300,22912,5), (23318,22912,5).
	wants := []want{
		{input: 23215 - 2432, output: 36, cacheRead: 2432},
		{input: 23300 - 22912, output: 5, cacheRead: 22912},
		{input: 23318 - 22912, output: 5, cacheRead: 22912},
	}
	for i, row := range cap.rows {
		if row.SessionID != wantSessionID {
			t.Errorf("row %d: session_id = %q, want %q", i, row.SessionID, wantSessionID)
		}
		if row.Runtime != "codex" {
			t.Errorf("row %d: runtime = %q, want codex", i, row.Runtime)
		}
		if row.Model != "gpt-5.5" {
			t.Errorf("row %d: model = %q, want gpt-5.5", i, row.Model)
		}
		w := wants[i]
		if row.InputTokens != w.input || row.OutputTokens != w.output || row.CacheReadTokens != w.cacheRead {
			t.Errorf("row %d: got input=%d output=%d cache_read=%d, want input=%d output=%d cache_read=%d (uncached-input derivation, not raw/additive)",
				i, row.InputTokens, row.OutputTokens, row.CacheReadTokens, w.input, w.output, w.cacheRead)
		}
	}

	// Distinct message_ids: three separate token_count events must not
	// collide onto a single ledger row.
	seen := map[string]bool{}
	for _, row := range cap.rows {
		if seen[row.MessageID] {
			t.Errorf("duplicate message_id %q across rows", row.MessageID)
		}
		seen[row.MessageID] = true
	}

	if len(up.calls) != 1 {
		t.Fatalf("expected 1 session upsert call, got %d: %+v", len(up.calls), up.calls)
	}
	sess := up.calls[0]
	if sess.SessionID != wantSessionID || sess.Cwd != "/tmp/test-workspace" || sess.Originator != "codex_exec" ||
		sess.Runtime != "codex" || sess.Model != "gpt-5.5" {
		t.Errorf("session upsert = %+v, want session_id=%s cwd=/tmp/test-workspace originator=codex_exec runtime=codex model=gpt-5.5",
			sess, wantSessionID)
	}
}

// TestProcessFile_ArchiveRescanIdempotent simulates the effect of `codex
// archive` moving a rollout file to a new path (archived_sessions/), which
// loses the tailer's path-keyed cursor and forces a full re-scan from byte 0.
// The derived message_ids must be identical to a first-time scan of the same
// content, since the ledger's uq_message unique index is what makes the
// re-insert an idempotent no-op at the DB layer — this test asserts the
// scraper-side half of that contract: same content, same derived keys.
func TestProcessFile_ArchiveRescanIdempotent(t *testing.T) {
	path, err := filepath.Abs("testdata/redteam-rollout-after-resume.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	s1, cap1, _ := newTestScraper(t)
	if err := s1.processFile(context.Background(), path); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Fresh scraper, fresh cursor map, same file content (as if it had been
	// moved to a new path and the tailer discovered it there for the first
	// time) — must reproduce identical message_ids.
	s2, cap2, _ := newTestScraper(t)
	if err := s2.processFile(context.Background(), path); err != nil {
		t.Fatalf("second (post-archive) scan: %v", err)
	}

	if len(cap1.rows) != len(cap2.rows) {
		t.Fatalf("row count differs across rescans: %d vs %d", len(cap1.rows), len(cap2.rows))
	}
	for i := range cap1.rows {
		if cap1.rows[i].MessageID != cap2.rows[i].MessageID {
			t.Errorf("row %d: message_id differs across rescans: %q vs %q — a post-archive rescan would NOT be an idempotent DB no-op",
				i, cap1.rows[i].MessageID, cap2.rows[i].MessageID)
		}
	}
}

// TestProcessFile_SubagentThreadBooksUnderParentSession is the regression
// test for the subagent cost-attribution bug (chunk-test2 evidence, live
// codex 0.142.5): a thread_spawn subagent's rollout file has session_meta.id
// == its own thread id, but session_meta.session_id == the PARENT's id.
// Ledger rows and the sessions upsert must use the PARENT id (session_id) so
// rollup's temporal_join can bridge subagent spend to the parent's focus
// intervals — booking under the thread's own id (the pre-fix behavior)
// orphans subagent spend into permanently-unallocated cost. Uses a synthetic
// fixture, not raw chunk rollouts (which carry operator content).
func TestProcessFile_SubagentThreadBooksUnderParentSession(t *testing.T) {
	s, cap, up := newTestScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-subagent.jsonl")

	const parentID = "019f3ed6-1354-7731-b35e-5356dd9af6d4"
	const threadID = "019f3ed8-17a7-7dc3-b11a-6cca251d9c86"
	writeLines(t, path, []string{
		subagentSessionMetaLine(threadID, parentID, parentID, "/tmp", "codex_exec", "0.142.5", "explorer", "Mencius"),
		turnContextLine("gpt-5.4"),
		tokenCountLine(100, 10, 0, 0),
	})
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	row := cap.rows[0]
	if row.SessionID != parentID {
		t.Errorf("ledger SessionID = %q, want parent id %q (booking under the thread's own id orphans subagent spend)", row.SessionID, parentID)
	}
	if row.MessageID != "codex:"+threadID+":000000" {
		t.Errorf("MessageID = %q, want keyed by thread id %q, not the parent session id", row.MessageID, threadID)
	}
	if row.AgentName != "@explorer" {
		t.Errorf("AgentName = %q, want \"@explorer\" (role-based, matching wms-mcp's existing focus-interval identity) — NOT \"@Mencius\" (nickname)", row.AgentName)
	}

	if len(up.calls) != 1 {
		t.Fatalf("expected 1 session upsert call, got %d", len(up.calls))
	}
	sess := up.calls[0]
	if sess.SessionID != parentID {
		t.Errorf("session upsert SessionID = %q, want parent id %q", sess.SessionID, parentID)
	}
	if sess.AgentName != "@explorer" {
		t.Errorf("session upsert AgentName = %q, want \"@explorer\"", sess.AgentName)
	}
}

// TestProcessFile_PreThreadSpawnSessionMetaFallsBackToID is the regression
// guard for 0.137.0-shaped rollout files (this package's original fixture
// era): session_meta has NO session_id field at all (the field is new
// somewhere in the 0.137→0.142 range). SessionID must fall back to the
// file's own id — the pre-fix behavior — so nothing regresses for hosts still
// on an older Codex CLI.
func TestProcessFile_PreThreadSpawnSessionMetaFallsBackToID(t *testing.T) {
	s, cap, up := newTestScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-pre-0142.jsonl")

	const id = "019f0000-0000-7000-8000-000000000001"
	writeLines(t, path, []string{
		sessionMetaLine(id, "/tmp", "codex_exec", "0.137.0"), // no session_id field, matches a real 0.137.0 file
		turnContextLine("gpt-5.5"),
		tokenCountLine(100, 10, 0, 0),
	})
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	if len(cap.rows) != 1 || cap.rows[0].SessionID != id {
		t.Fatalf("SessionID = %q, want fallback to file id %q", cap.rows[0].SessionID, id)
	}
	if cap.rows[0].MessageID != "codex:"+id+":000000" {
		t.Errorf("MessageID = %q, want keyed by %q", cap.rows[0].MessageID, id)
	}
	if cap.rows[0].AgentName != "" {
		t.Errorf("AgentName = %q, want empty (no agent_role on a non-subagent file)", cap.rows[0].AgentName)
	}
	if up.calls[0].AgentName != "" {
		t.Errorf("session upsert AgentName = %q, want empty", up.calls[0].AgentName)
	}
}

// TestProcessFile_ParentAndSubagentNoMessageIDCollision proves the collision
// trap the fix must avoid: a parent file and its subagent file share the same
// SessionID (the parent's id) but each has its own independent seq counter.
// If message_id were derived from SessionID instead of ThreadID, the parent's
// seq-0 row and the subagent's seq-0 row would both produce
// "codex:<parentID>:000000" and the DB's uq_message upsert would silently
// swallow one of them. Also asserts exactly one sessions row is upserted for
// the parent (agent_name="") and one for the subagent (agent_name="@worker") —
// same SessionID, different AgentName, matching the Claude Code Agent Teams
// sessions convention (one row per (session_id, agent_name) pair).
func TestProcessFile_ParentAndSubagentNoMessageIDCollision(t *testing.T) {
	s, cap, up := newTestScraper(t)
	dir := t.TempDir()

	const parentID = "019f3ed6-1354-7731-b35e-5356dd9af6d4"
	const threadID = "019f3ed8-1843-7d61-992b-f7a012bfa313"

	parentPath := filepath.Join(dir, "rollout-parent.jsonl")
	writeLines(t, parentPath, []string{
		sessionMetaLine(parentID, "/tmp/test-workspace", "codex-tui", "0.142.5"),
		turnContextLine("gpt-5.4"),
		tokenCountLine(100, 10, 0, 0),
	})
	subagentPath := filepath.Join(dir, "rollout-subagent.jsonl")
	writeLines(t, subagentPath, []string{
		subagentSessionMetaLine(threadID, parentID, parentID, "/tmp/test-workspace", "codex_exec", "0.142.5", "worker", "Dirac"),
		turnContextLine("gpt-5.4"),
		tokenCountLine(200, 20, 0, 0),
	})

	if err := s.processFile(context.Background(), parentPath); err != nil {
		t.Fatalf("processFile(parent): %v", err)
	}
	if err := s.processFile(context.Background(), subagentPath); err != nil {
		t.Fatalf("processFile(subagent): %v", err)
	}

	if len(cap.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(cap.rows), cap.rows)
	}
	seen := map[string]bool{}
	for _, row := range cap.rows {
		if row.SessionID != parentID {
			t.Errorf("row session_id = %q, want parent id %q for both parent and subagent rows", row.SessionID, parentID)
		}
		if seen[row.MessageID] {
			t.Fatalf("message_id collision: %q emitted by both parent and subagent files", row.MessageID)
		}
		seen[row.MessageID] = true
	}

	if len(up.calls) != 2 {
		t.Fatalf("expected 2 session upsert calls (one per (session_id, agent_name) pair), got %d: %+v", len(up.calls), up.calls)
	}
	byAgent := map[string]store.Session{}
	for _, c := range up.calls {
		byAgent[c.AgentName] = c
	}
	if s, ok := byAgent[""]; !ok || s.SessionID != parentID {
		t.Errorf("missing/wrong parent session upsert (agent_name=\"\"): %+v", byAgent[""])
	}
	if s, ok := byAgent["@worker"]; !ok || s.SessionID != parentID {
		t.Errorf("missing/wrong subagent session upsert (agent_name=\"@worker\"): %+v", byAgent["@worker"])
	}
}

// TestEmitLedgerRow_CachedAndReasoningAreSubsets is the regression test for
// the double-counting bug caught in review: cached_input_tokens and
// reasoning_output_tokens are SUBSETS of input_tokens/output_tokens, not
// additional tokens. Uses a synthetic event with non-zero reasoning_output
// (the real fixture happens to have reasoning_output=0 throughout, so it
// can't exercise this half of the derivation).
func TestEmitLedgerRow_CachedAndReasoningAreSubsets(t *testing.T) {
	s, cap, _ := newTestScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-subset.jsonl")

	// input=1000, cached_input=400 (subset of input), output=200,
	// reasoning_output=50 (subset of output). total_tokens = 1000+200=1200.
	writeLines(t, path, []string{
		sessionMetaLine("sess-subset", "/tmp", "codex_exec", "0.137.0"),
		turnContextLine("gpt-5.5"),
		tokenCountLine(1000, 200, 400, 50),
	})
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(cap.rows))
	}
	row := cap.rows[0]
	if row.InputTokens != 600 { // 1000 - 400 cached, NOT 1000
		t.Errorf("InputTokens = %d, want 600 (input_tokens - cached_input_tokens)", row.InputTokens)
	}
	if row.CacheReadTokens != 400 {
		t.Errorf("CacheReadTokens = %d, want 400", row.CacheReadTokens)
	}
	if row.OutputTokens != 200 { // as-is, NOT 200+50
		t.Errorf("OutputTokens = %d, want 200 (output_tokens as-is, reasoning already inside)", row.OutputTokens)
	}
	if row.ReasoningOutputTokens != 50 {
		t.Errorf("ReasoningOutputTokens = %d, want 50 (stored for fidelity, not summed into OutputTokens)", row.ReasoningOutputTokens)
	}
}

// TestProcessLine_IgnoresCodexAutoReviewModelSentinel covers the upstream
// bug (openai/codex#20981) where some internal Codex sub-tasks report the
// literal model string "codex-auto-review" instead of the real model. The
// tailer must not let that sentinel overwrite the last real model seen.
func TestProcessLine_IgnoresCodexAutoReviewModelSentinel(t *testing.T) {
	s, cap, _ := newTestScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-sentinel.jsonl")

	writeLines(t, path, []string{
		sessionMetaLine("sess-sentinel", "/tmp", "codex_exec", "0.137.0"),
		turnContextLine("gpt-5.5"),
		tokenCountLine(100, 10, 0, 0),
		turnContextLine("codex-auto-review"), // must be ignored
		tokenCountLine(50, 5, 0, 0),
	})
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if len(cap.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(cap.rows))
	}
	for i, row := range cap.rows {
		if row.Model != "gpt-5.5" {
			t.Errorf("row %d: model = %q, want gpt-5.5 (codex-auto-review sentinel must not overwrite it)", i, row.Model)
		}
	}
}

// TestProcessFile_Truncated mirrors token-scraper's truncation-reset
// behavior: if the file on disk is smaller than the persisted cursor offset
// (rotation, or Codex's history.max_bytes retention truncating the file),
// the cursor resets to zero rather than seeking past EOF.
func TestProcessFile_Truncated(t *testing.T) {
	s, cap, _ := newTestScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-test.jsonl")

	writeLines(t, path, []string{
		sessionMetaLine("sess-trunc", "/tmp", "codex_exec", "0.137.0"),
		turnContextLine("gpt-5.5"),
		tokenCountLine(100, 10, 5, 0),
	})
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row after first pass, got %d", len(cap.rows))
	}

	// Simulate truncation: replace with a shorter file (fresh session_meta,
	// new token_count).
	cap.rows = nil
	writeLines(t, path, []string{
		sessionMetaLine("sess-trunc-2", "/tmp2", "codex_exec", "0.137.0"),
		turnContextLine("gpt-5.5"),
		tokenCountLine(50, 5, 0, 0),
	})
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("second pass (post-truncation): %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row after truncation-reset pass, got %d: %+v", len(cap.rows), cap.rows)
	}
	if cap.rows[0].SessionID != "sess-trunc-2" {
		t.Errorf("session_id = %q, want sess-trunc-2 (cursor did not reset on truncation)", cap.rows[0].SessionID)
	}
}

// TestProcessFile_Vanished asserts a missing file is a silent no-op, not an
// error — a file can vanish between glob discovery and stat (or be archived
// away mid-poll).
func TestProcessFile_Vanished(t *testing.T) {
	s, _, _ := newTestScraper(t)
	if err := s.processFile(context.Background(), "/nonexistent/path/rollout.jsonl"); err != nil {
		t.Fatalf("processFile on vanished file returned error, want nil: %v", err)
	}
}

// TestProcessFile_PartialTrailingLine asserts the tailer never commits a
// line that has no trailing newline yet (Codex may still be mid-write) —
// the cursor must not advance past it, so the next poll re-reads it complete.
func TestProcessFile_PartialTrailingLine(t *testing.T) {
	s, cap, _ := newTestScraper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-partial.jsonl")

	full := sessionMetaLine("sess-partial", "/tmp", "codex_exec", "0.137.0") + "\n" +
		turnContextLine("gpt-5.5") + "\n" +
		tokenCountLine(10, 1, 0, 0) // no trailing newline: simulates a write in progress

	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(cap.rows) != 0 {
		t.Fatalf("expected 0 rows while the token_count line lacks a trailing newline, got %d", len(cap.rows))
	}

	// Complete the write (append the newline); the next pass must now pick
	// up the previously-uncommitted line.
	if err := os.WriteFile(path, []byte(full+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.processFile(context.Background(), path); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row once the line completed, got %d", len(cap.rows))
	}
}

func TestMcpCallOK(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantOK      bool
		wantMatched bool
	}{
		{
			name:        "success",
			raw:         `{"Ok":{"content":[{"type":"text","text":"58 open outcomes"}]}}`,
			wantOK:      true,
			wantMatched: true,
		},
		{
			name:        "cancelled/denied",
			raw:         `{"Err":"user cancelled MCP tool call"}`,
			wantOK:      false,
			wantMatched: true,
		},
		{
			name:        "empty",
			raw:         "",
			wantOK:      false,
			wantMatched: false,
		},
		{
			name:        "malformed",
			raw:         `not json`,
			wantOK:      false,
			wantMatched: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, matched := mcpCallOK(json.RawMessage(tt.raw))
			if ok != tt.wantOK || matched != tt.wantMatched {
				t.Errorf("mcpCallOK(%q) = (%v, %v), want (%v, %v)", tt.raw, ok, matched, tt.wantOK, tt.wantMatched)
			}
		})
	}
}

// TestDiscoverFiles_MissingRoot covers the common fresh-install case: a host
// that has never run `codex archive` has no archived_sessions/ directory at
// all. filepath.WalkDir errors on a non-existent root; discoverFiles must
// swallow that and still return results from the roots that do exist.
func TestDiscoverFiles_MissingRoot(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(existing, "rollout-a.jsonl"), []string{sessionMetaLine("a", "/tmp", "codex_exec", "0.137.0")})
	missing := filepath.Join(dir, "archived_sessions") // never created

	s := &scraper{roots: []string{existing, missing}}
	files := s.discoverFiles()
	if len(files) != 1 {
		t.Fatalf("expected 1 file from the existing root, got %d: %v", len(files), files)
	}
}

func TestDiscoverFiles(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions", "2026", "07", "07")
	archivedDir := filepath.Join(dir, "archived_sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(sessionsDir, "rollout-a.jsonl"), []string{sessionMetaLine("a", "/tmp", "codex_exec", "0.137.0")})
	writeLines(t, filepath.Join(archivedDir, "rollout-b.jsonl"), []string{sessionMetaLine("b", "/tmp", "codex_exec", "0.137.0")})
	// non-jsonl file must be ignored
	if err := os.WriteFile(filepath.Join(sessionsDir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &scraper{roots: []string{filepath.Join(dir, "sessions"), archivedDir}}
	files := s.discoverFiles()
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
}

// --- fixture builders (hand-written, zero model tokens) ---

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// sessionMetaLine builds a top-level (non-subagent) session_meta line —
// deliberately with NO session_id field, matching both a real 0.137.0 file
// (which never has one) and being harmless on 0.142.x (where a top-level
// file's session_id equals its own id, so the id-fallback produces the same
// result).
func sessionMetaLine(id, cwd, originator, cliVersion string) string {
	b, _ := json.Marshal(map[string]any{
		"timestamp": "2026-07-07T00:00:00.000Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id":          id,
			"cwd":         cwd,
			"originator":  originator,
			"cli_version": cliVersion,
		},
	})
	return string(b)
}

// subagentSessionMetaLine builds a thread_spawn subagent's session_meta line:
// id is the file's OWN thread id, sessionID/parentThreadID are the parent's
// id (both set — matches real chunk-test2 evidence), and role/nickname are
// the subagent's identity (e.g. "explorer"/"Mencius").
func subagentSessionMetaLine(id, sessionID, parentThreadID, cwd, originator, cliVersion, role, nickname string) string {
	b, _ := json.Marshal(map[string]any{
		"timestamp": "2026-07-07T00:00:00.000Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id":               id,
			"session_id":       sessionID,
			"parent_thread_id": parentThreadID,
			"cwd":              cwd,
			"originator":       originator,
			"cli_version":      cliVersion,
			"agent_role":       role,
			"agent_nickname":   nickname,
		},
	})
	return string(b)
}

func turnContextLine(model string) string {
	b, _ := json.Marshal(map[string]any{
		"timestamp": "2026-07-07T00:00:01.000Z",
		"type":      "turn_context",
		"payload": map[string]any{
			"model": model,
		},
	})
	return string(b)
}

// tokenCountLine builds a synthetic token_count event_msg line. cachedInput
// and reasoningOutput are SUBSETS of input/output respectively (matching
// real Codex semantics: total_tokens == input_tokens + output_tokens always),
// not additional tokens on top — callers must pass cachedInput <= input and
// reasoningOutput <= output.
func tokenCountLine(input, output, cachedInput, reasoningOutput int64) string {
	usage := func(mult int64) map[string]any {
		return map[string]any{
			"input_tokens": input * mult, "output_tokens": output * mult,
			"cached_input_tokens": cachedInput * mult, "reasoning_output_tokens": reasoningOutput * mult,
			"total_tokens": (input + output) * mult,
		}
	}
	b, _ := json.Marshal(map[string]any{
		"timestamp": "2026-07-07T00:00:02.000Z",
		"type":      "event_msg",
		"payload": map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": usage(100), // cumulative session total, always ignored by the tailer
				"last_token_usage":  usage(1),
			},
		},
	})
	return string(b)
}
