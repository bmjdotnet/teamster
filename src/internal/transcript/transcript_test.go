package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDedupKey covers the pure composite-key helper and the line-level fallback.
func TestDedupKey(t *testing.T) {
	if got := DedupKey("msg_X", "req_Y"); got != "msg_X|req_Y" {
		t.Errorf("DedupKey = %q, want msg_X|req_Y", got)
	}

	var l Line
	l.UUID = "u-only"
	if got := LineDedupKey(l); got != "u-only" {
		t.Errorf("LineDedupKey fallback = %q, want u-only", got)
	}
	l.Message.ID = "msg_X"
	l.RequestID = "req_Y"
	if got := LineDedupKey(l); got != "msg_X|req_Y" {
		t.Errorf("LineDedupKey = %q, want msg_X|req_Y", got)
	}
}

// TestComposite KeyJoinsLedgerRow asserts the spec §7.3 invariant: a known
// transcript message's composite key (message.id|requestId) is the exact form
// stored in token_ledger.message_id — and that the bare message.id does NOT
// match. We model the ledger key with the value the scraper would have written
// (LineDedupKey), then prove the recovery-side DedupKey reconstructs it.
func TestCompositeKeyJoinsLedgerRow(t *testing.T) {
	// A real-shaped assistant line (message.id + requestId from a live transcript).
	line := Line{
		Type:      "assistant",
		UUID:      "0f3e-uuid",
		RequestID: "req_011CbtoEAKjjUza43qRreyoj",
	}
	line.Message.ID = "msg_014mvijn7iyK3SearSDUuSUr"

	// What the scraper stored as token_ledger.message_id for this message.
	ledgerKey := LineDedupKey(line)

	// Recovery reconstructs the same key from the two fields → joins at 100%.
	if got := DedupKey(line.Message.ID, line.RequestID); got != ledgerKey {
		t.Fatalf("recovery key %q != ledger key %q (would 0%% join)", got, ledgerKey)
	}

	// The bare message.id is NOT the ledger key — guards against the §7.3 trap.
	if line.Message.ID == ledgerKey {
		t.Fatalf("bare message.id must not equal the composite ledger key")
	}
}

// TestSetFocusTimeline builds a fixture with a lead thread (main file) and a
// teammate thread (subagents/agent-*.jsonl), each issuing ordered wms_setFocus
// calls, and asserts the extractor returns the per-thread ordered timeline and
// that FocusAt resolves the most-recent-at-or-before entity, with the
// pre-first-setFocus warmup case yielding ok=false.
func TestSetFocusTimeline(t *testing.T) {
	projects := t.TempDir()
	sessionID := "11111111-2222-3333-4444-555555555555"
	projDir := filepath.Join(projects, "-mnt-ai-projects-teamster")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Main (lead) transcript: two setFocus calls out of order in time on the line
	// stream to prove sorting; plus a non-setFocus tool_use line that must be
	// ignored. Lead agent_name == "".
	mainPath := filepath.Join(projDir, sessionID+".jsonl")
	writeLines(t, mainPath, []string{
		focusLine("2026-06-10T02:34:17.774Z", "outcome", "out-A", "first lead focus"),
		// a non-setFocus tool_use line — must NOT produce an event
		`{"type":"assistant","timestamp":"2026-06-10T02:35:00.000Z","sessionId":"` + sessionID +
			`","message":{"id":"m2","content":[{"type":"tool_use","name":"mcp__wms__wms_updateStatus","input":{"entityID":"x"}}]}}`,
		focusLine("2026-06-10T02:40:00.000Z", "workunit", "wu-B", "second lead focus"),
		// a user line — must be ignored
		`{"type":"user","timestamp":"2026-06-10T02:41:00.000Z"}`,
	})

	// Teammate transcript under subagents/, stamped via meta -> "@PizzaOven".
	subDir := filepath.Join(strings.TrimSuffix(mainPath, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subPath := filepath.Join(subDir, "agent-abc.jsonl")
	writeLines(t, subPath, []string{
		focusLine("2026-06-10T02:36:00.000Z", "workunit", "wu-T", "teammate focus"),
	})
	if err := os.WriteFile(filepath.Join(subDir, "agent-abc.meta.json"),
		[]byte(`{"agentType":"PizzaOven"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	tl, err := SetFocusTimeline(projects, sessionID)
	if err != nil {
		t.Fatalf("SetFocusTimeline: %v", err)
	}

	// Lead thread: 2 events, ascending, ignoring the non-setFocus + user lines.
	lead := tl.Events[""]
	if len(lead) != 2 {
		t.Fatalf("lead events = %d, want 2: %+v", len(lead), lead)
	}
	if lead[0].EntityID != "out-A" || lead[1].EntityID != "wu-B" {
		t.Errorf("lead order = %q,%q want out-A,wu-B", lead[0].EntityID, lead[1].EntityID)
	}
	if lead[0].EntityType != "outcome" || lead[0].Focus != "first lead focus" {
		t.Errorf("lead[0] = %+v, want outcome/first lead focus", lead[0])
	}

	// Teammate thread keyed by "@PizzaOven".
	team := tl.Events["@PizzaOven"]
	if len(team) != 1 || team[0].EntityID != "wu-T" {
		t.Fatalf("teammate events = %+v, want one wu-T", team)
	}

	// FocusAt: most-recent-at-or-before on the lead thread.
	mustTime := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	// Before the first setFocus → warmup, ok=false.
	if _, ok := tl.FocusAt("", mustTime("2026-06-10T02:00:00.000Z")); ok {
		t.Errorf("FocusAt before first setFocus should be ok=false")
	}
	// Between the two lead foci → resolves to out-A.
	if ev, ok := tl.FocusAt("", mustTime("2026-06-10T02:38:00.000Z")); !ok || ev.EntityID != "out-A" {
		t.Errorf("FocusAt mid = %+v ok=%v, want out-A", ev, ok)
	}
	// After the second focus → resolves to wu-B.
	if ev, ok := tl.FocusAt("", mustTime("2026-06-10T03:00:00.000Z")); !ok || ev.EntityID != "wu-B" {
		t.Errorf("FocusAt late = %+v ok=%v, want wu-B", ev, ok)
	}
	// Exactly at a focus timestamp → at-or-before includes it.
	if ev, ok := tl.FocusAt("", mustTime("2026-06-10T02:40:00.000Z")); !ok || ev.EntityID != "wu-B" {
		t.Errorf("FocusAt at-boundary = %+v ok=%v, want wu-B", ev, ok)
	}
	// Teammate thread lookup is independent of the lead thread.
	if ev, ok := tl.FocusAt("@PizzaOven", mustTime("2026-06-10T02:50:00.000Z")); !ok || ev.EntityID != "wu-T" {
		t.Errorf("FocusAt teammate = %+v ok=%v, want wu-T", ev, ok)
	}
}

// TestSetFocusTimelineMissingTranscript: a session with no transcript on disk is
// not an error; the timeline is empty and every FocusAt yields ok=false.
func TestSetFocusTimelineMissingTranscript(t *testing.T) {
	tl, err := SetFocusTimeline(t.TempDir(), "no-such-session")
	if err != nil {
		t.Fatalf("missing transcript should not error: %v", err)
	}
	if _, ok := tl.FocusAt("", time.Now()); ok {
		t.Errorf("empty timeline FocusAt should be ok=false")
	}
}

// TestSetFocusTimelineTextMentionFalsePositive proves that transcript lines which
// mention "mcp__wms__wms_setFocus" in text blocks (CLAUDE.md content, system
// prompts, assistant narration) do NOT produce FocusEvents. Only actual tool_use
// content blocks with the correct name count.
func TestSetFocusTimelineTextMentionFalsePositive(t *testing.T) {
	projects := t.TempDir()
	sessionID := "fp-session-0000-0000-000000000000"
	projDir := filepath.Join(projects, "-test-project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mainPath := filepath.Join(projDir, sessionID+".jsonl")
	writeLines(t, mainPath, []string{
		// User line containing CLAUDE.md content that mentions the tool name
		`{"type":"user","timestamp":"2026-06-10T01:00:00.000Z","message":{"content":"Call mcp__wms__wms_setFocus to declare focus."}}`,
		// Assistant text block mentioning the tool name in prose
		`{"type":"assistant","timestamp":"2026-06-10T01:01:00.000Z","message":{"id":"m1","content":[{"type":"text","text":"I will call mcp__wms__wms_setFocus with entityType=outcome and wms_setFocus to set focus."}]}}`,
		// Assistant text block with setFocus substring but no tool_use
		`{"type":"assistant","timestamp":"2026-06-10T01:02:00.000Z","message":{"id":"m2","content":[{"type":"text","text":"The setFocus tool_use is used for focus attribution tracking."}]}}`,
		// System-reminder style line
		`{"type":"user","timestamp":"2026-06-10T01:03:00.000Z","message":{"content":"<system-reminder>setFocus tool_use mcp__wms__wms_setFocus</system-reminder>"}}`,
	})

	tl, err := SetFocusTimeline(projects, sessionID)
	if err != nil {
		t.Fatalf("SetFocusTimeline: %v", err)
	}
	if len(tl.Events) != 0 {
		t.Fatalf("text-mention-only transcript produced %d event threads; want 0: %+v",
			len(tl.Events), tl.Events)
	}
}

// TestSetFocusTimelineNameVariants verifies all accepted tool name variants
// produce events.
func TestSetFocusTimelineNameVariants(t *testing.T) {
	projects := t.TempDir()
	sessionID := "variant-session-0000-0000-000000000000"
	projDir := filepath.Join(projects, "-test-project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mainPath := filepath.Join(projDir, sessionID+".jsonl")
	writeLines(t, mainPath, []string{
		`{"type":"assistant","timestamp":"2026-06-10T01:00:00.000Z","message":{"id":"m1","content":[{"type":"tool_use","name":"mcp__wms__wms_setFocus","input":{"entityType":"outcome","entityID":"out-1","focus":"full mcp name"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-10T01:01:00.000Z","message":{"id":"m2","content":[{"type":"tool_use","name":"wms_setFocus","input":{"entityType":"outcome","entityID":"out-2","focus":"short mcp name"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-10T01:02:00.000Z","message":{"id":"m3","content":[{"type":"tool_use","name":"setFocus","input":{"entityType":"outcome","entityID":"out-3","focus":"bare name"}}]}}`,
	})

	tl, err := SetFocusTimeline(projects, sessionID)
	if err != nil {
		t.Fatalf("SetFocusTimeline: %v", err)
	}
	lead := tl.Events[""]
	if len(lead) != 3 {
		t.Fatalf("expected 3 events from 3 name variants, got %d: %+v", len(lead), lead)
	}
	wantIDs := []string{"out-1", "out-2", "out-3"}
	for i, want := range wantIDs {
		if lead[i].EntityID != want {
			t.Errorf("event[%d].EntityID = %q, want %q", i, lead[i].EntityID, want)
		}
	}
}

// TestReadWindow verifies the timestamp-windowed transcript reader returns only
// messages within [start, end) and respects maxLines.
func TestReadWindow(t *testing.T) {
	projects := t.TempDir()
	sessionID := "rw-session-0000-0000-000000000000"
	projDir := filepath.Join(projects, "-test-project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mainPath := filepath.Join(projDir, sessionID+".jsonl")
	writeLines(t, mainPath, []string{
		// Before window
		`{"type":"user","timestamp":"2026-06-10T01:00:00.000Z","message":{"content":"before window"}}`,
		// Inside window — user string content
		`{"type":"user","timestamp":"2026-06-10T02:00:00.000Z","message":{"content":"user msg inside"}}`,
		// Inside window — assistant text block
		`{"type":"assistant","timestamp":"2026-06-10T02:30:00.000Z","message":{"id":"m1","content":[{"type":"text","text":"assistant reply"}]}}`,
		// Inside window — non-message line (mode)
		`{"type":"mode","timestamp":"2026-06-10T02:35:00.000Z","mode":"normal"}`,
		// Inside window — assistant with tool_use (text should be empty, skipped)
		`{"type":"assistant","timestamp":"2026-06-10T02:40:00.000Z","message":{"id":"m2","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		// Inside window — second user message
		`{"type":"user","timestamp":"2026-06-10T02:50:00.000Z","message":{"content":"second user msg"}}`,
		// At end boundary (excluded, half-open)
		`{"type":"user","timestamp":"2026-06-10T03:00:00.000Z","message":{"content":"at boundary"}}`,
		// After window
		`{"type":"user","timestamp":"2026-06-10T04:00:00.000Z","message":{"content":"after window"}}`,
	})

	mustTime := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	start := mustTime("2026-06-10T02:00:00.000Z")
	end := mustTime("2026-06-10T03:00:00.000Z")

	lines, err := ReadWindow(sessionID, projects, start, end, 100)
	if err != nil {
		t.Fatalf("ReadWindow: %v", err)
	}

	// Expect 3 lines: user "user msg inside", assistant "assistant reply", user "second user msg"
	// Tool-use-only assistant is skipped (no text), mode line skipped, boundary excluded.
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].Role != "user" || lines[0].Content != "user msg inside" {
		t.Errorf("lines[0] = %+v, want user/user msg inside", lines[0])
	}
	if lines[1].Role != "assistant" || lines[1].Content != "assistant reply" {
		t.Errorf("lines[1] = %+v, want assistant/assistant reply", lines[1])
	}
	if lines[2].Role != "user" || lines[2].Content != "second user msg" {
		t.Errorf("lines[2] = %+v, want user/second user msg", lines[2])
	}

	// Test maxLines cap
	capped, err := ReadWindow(sessionID, projects, start, end, 2)
	if err != nil {
		t.Fatalf("ReadWindow capped: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("capped got %d lines, want 2", len(capped))
	}
}

// TestReadWindowMissingTranscript: missing transcript returns empty, not error.
func TestReadWindowMissingTranscript(t *testing.T) {
	lines, err := ReadWindow("no-such-session", t.TempDir(), time.Now().Add(-time.Hour), time.Now(), 100)
	if err != nil {
		t.Fatalf("missing transcript should not error: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected 0 lines for missing transcript, got %d", len(lines))
	}
}

// focusLine builds one assistant transcript line carrying a wms_setFocus tool_use.
func focusLine(ts, etype, eid, focus string) string {
	return `{"type":"assistant","timestamp":"` + ts +
		`","message":{"id":"m","content":[{"type":"tool_use","name":"mcp__wms__wms_setFocus",` +
		`"input":{"entityType":"` + etype + `","entityID":"` + eid + `","focus":"` + focus + `"}}]}}`
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var buf string
	for _, l := range lines {
		buf += l + "\n"
	}
	if err := os.WriteFile(path, []byte(buf), 0o644); err != nil {
		t.Fatal(err)
	}
}
