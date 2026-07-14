package wms

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJSONL writes lines to a temp file and returns its path.
func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

// TestReadSignals_RFC3339Timestamp is the B2 regression test. hookd writes the
// `ts` field as an RFC3339 STRING (e.g. "2026-05-17T23:54:04Z"). The reader
// previously declared ts as float64, so json.Unmarshal errored on every line
// and TotalEvents was always 0 — the classifier never produced a tag. This
// test asserts a realistic hookd line is parsed and the aggregates populate.
func TestReadSignals_RFC3339Timestamp(t *testing.T) {
	logFile := writeJSONL(t,
		`{"ts":"2026-05-17T23:54:04Z","session":"abc123def456ghi","agent_name":"@worker","tag":"READ","file":"/x/a.go"}`,
		`{"ts":"2026-05-17T23:54:06Z","session":"abc123def456ghi","agent_name":"@worker","tag":"GREP"}`,
		`{"ts":"2026-05-17T23:54:08Z","session":"abc123def456ghi","agent_name":"@worker","tag":"EXEC","bash_cmd":"go test ./..."}`,
		`{"ts":"2026-05-17T23:54:10Z","session":"abc123def456ghi","agent_name":"@worker","tag":"EDIT","file":"/x/readme.md"}`,
	)

	sw := SessionWindow{
		SessionPrefix: "abc123def456", // 12-char prefix, matching hookd truncation
		AgentName:     "@worker",
		Start:         mustTime(t, "2026-05-17T23:54:00Z"),
		End:           mustTime(t, "2026-05-17T23:55:00Z"),
	}

	r := NewJSONLSignalReader()
	sig, err := r.ReadSignals(context.Background(), []SessionWindow{sw}, logFile)
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}

	if sig.TotalEvents != 4 {
		t.Fatalf("TotalEvents = %d, want 4 (RFC3339 ts must parse — this is the B2 regression)", sig.TotalEvents)
	}
	if sig.ToolTagCounts["READ"] != 1 || sig.ToolTagCounts["GREP"] != 1 || sig.ToolTagCounts["EXEC"] != 1 || sig.ToolTagCounts["EDIT"] != 1 {
		t.Fatalf("ToolTagCounts wrong: %+v", sig.ToolTagCounts)
	}
	if len(sig.BashCommands) != 1 || sig.BashCommands[0] != "go test ./..." {
		t.Fatalf("BashCommands wrong: %+v", sig.BashCommands)
	}
	if sig.FilesTouched[".go"] != 1 || sig.FilesTouched[".md"] != 1 {
		t.Fatalf("FilesTouched wrong: %+v", sig.FilesTouched)
	}
}

// TestReadSignals_WindowAndAgentFiltering confirms out-of-window and
// wrong-agent events are excluded even when the ts parses fine.
func TestReadSignals_WindowAndAgentFiltering(t *testing.T) {
	logFile := writeJSONL(t,
		`{"ts":"2026-05-17T23:54:04Z","session":"abc123def456ghi","agent_name":"@worker","tag":"READ"}`, // in window, right agent
		`{"ts":"2026-05-17T23:59:00Z","session":"abc123def456ghi","agent_name":"@worker","tag":"READ"}`, // out of window
		`{"ts":"2026-05-17T23:54:05Z","session":"abc123def456ghi","agent_name":"@other","tag":"READ"}`,  // wrong agent
		`{"ts":"2026-05-17T23:54:06Z","session":"zzz999zzz999zzz","agent_name":"@worker","tag":"READ"}`, // wrong session
	)
	sw := SessionWindow{
		SessionPrefix: "abc123def456",
		AgentName:     "@worker",
		Start:         mustTime(t, "2026-05-17T23:54:00Z"),
		End:           mustTime(t, "2026-05-17T23:55:00Z"),
	}
	sig, err := NewJSONLSignalReader().ReadSignals(context.Background(), []SessionWindow{sw}, logFile)
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if sig.TotalEvents != 1 {
		t.Fatalf("TotalEvents = %d, want 1 (only the in-window same-agent same-session event)", sig.TotalEvents)
	}
}

func TestParseEventTime(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		wantSec int64 // unix seconds when parseable
	}{
		{"rfc3339 second precision", "2026-05-17T23:54:04Z", false, mustTime(t, "2026-05-17T23:54:04Z").Unix()},
		{"rfc3339 nano", "2026-05-17T23:54:04.123456Z", false, mustTime(t, "2026-05-17T23:54:04Z").Unix()},
		{"legacy numeric epoch", "1747526044", false, 1747526044},
		{"empty", "", true, 0},
		{"garbage", "not-a-time", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseEventTime(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseEventTime(%q) = %v, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEventTime(%q): %v", c.in, err)
			}
			if got.Unix() != c.wantSec {
				t.Fatalf("parseEventTime(%q).Unix() = %d, want %d", c.in, got.Unix(), c.wantSec)
			}
			if got.Location() != time.UTC {
				t.Fatalf("parseEventTime(%q) not UTC: %v", c.in, got.Location())
			}
		})
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

// TestReadSignalsBatch_MatchesReadSignalsPerWindow is the correctness anchor
// for the batched path (GH #13 perf fix): for a set of independent windows —
// including two windows sharing the same session, which ReadSignals' own
// "first match wins" semantics would under-count if naively reused — each
// window's ReadSignalsBatch result must equal what a standalone ReadSignals
// call for that single window would have produced.
func TestReadSignalsBatch_MatchesReadSignalsPerWindow(t *testing.T) {
	logFile := writeJSONL(t,
		`{"ts":"2026-05-17T23:54:04Z","session":"abc123def456ghi","agent_name":"@worker","tag":"READ","file":"/x/a.go"}`,
		`{"ts":"2026-05-17T23:54:06Z","session":"abc123def456ghi","agent_name":"@worker","tag":"EDIT","file":"/x/b.go"}`,
		// A second window on the SAME session+agent but a LATER time range —
		// proves per-window independence, not a shared/merged total.
		`{"ts":"2026-05-17T23:58:00Z","session":"abc123def456ghi","agent_name":"@worker","tag":"EXEC","bash_cmd":"go test ./..."}`,
		// A different session entirely.
		`{"ts":"2026-05-17T23:54:05Z","session":"zzz999zzz999zzz","agent_name":"@other","tag":"GREP"}`,
	)

	w1 := SessionWindow{
		SessionPrefix: "abc123def456", AgentName: "@worker",
		Start: mustTime(t, "2026-05-17T23:54:00Z"), End: mustTime(t, "2026-05-17T23:55:00Z"),
	}
	w2 := SessionWindow{
		SessionPrefix: "abc123def456", AgentName: "@worker",
		Start: mustTime(t, "2026-05-17T23:57:00Z"), End: mustTime(t, "2026-05-17T23:59:00Z"),
	}
	w3 := SessionWindow{
		SessionPrefix: "zzz999zzz999", AgentName: "@other",
		Start: mustTime(t, "2026-05-17T23:54:00Z"), End: mustTime(t, "2026-05-17T23:55:00Z"),
	}
	windows := []SessionWindow{w1, w2, w3}

	r := NewJSONLSignalReader()
	got, err := r.ReadSignalsBatch(context.Background(), windows, logFile, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ReadSignalsBatch: %v", err)
	}
	if len(got) != len(windows) {
		t.Fatalf("ReadSignalsBatch returned %d results, want %d", len(got), len(windows))
	}

	for i, w := range windows {
		want, err := r.ReadSignals(context.Background(), []SessionWindow{w}, logFile)
		if err != nil {
			t.Fatalf("ReadSignals(window %d): %v", i, err)
		}
		if got[i].TotalEvents != want.TotalEvents {
			t.Errorf("window %d: TotalEvents = %d, want %d (independent per-window count)", i, got[i].TotalEvents, want.TotalEvents)
		}
		if len(got[i].BashCommands) != len(want.BashCommands) {
			t.Errorf("window %d: BashCommands = %v, want %v", i, got[i].BashCommands, want.BashCommands)
		}
	}
	if got[0].TotalEvents != 2 {
		t.Errorf("w1 TotalEvents = %d, want 2 (READ+EDIT)", got[0].TotalEvents)
	}
	if got[1].TotalEvents != 1 || len(got[1].BashCommands) != 1 {
		t.Errorf("w2 TotalEvents/BashCommands = %d/%v, want 1/[go test ./...]", got[1].TotalEvents, got[1].BashCommands)
	}
	if got[2].TotalEvents != 1 {
		t.Errorf("w3 TotalEvents = %d, want 1", got[2].TotalEvents)
	}
}

// TestReadSignalsBatch_BoundsExcludeOutOfRangeEvents proves the
// lowerBound/upperBound arguments — the runtime-determined bound classify
// computes from its current interval batch (GH #13 tier 2) — actually filter
// out events, even when those events would otherwise fall inside a window.
func TestReadSignalsBatch_BoundsExcludeOutOfRangeEvents(t *testing.T) {
	logFile := writeJSONL(t,
		// Before the lower bound — must be excluded even though it's inside w's window.
		`{"ts":"2026-05-17T23:50:00Z","session":"abc123def456ghi","agent_name":"@worker","tag":"READ"}`,
		// Inside bounds.
		`{"ts":"2026-05-17T23:54:04Z","session":"abc123def456ghi","agent_name":"@worker","tag":"EDIT"}`,
		// After the upper bound.
		`{"ts":"2026-05-17T23:59:59Z","session":"abc123def456ghi","agent_name":"@worker","tag":"WRITE"}`,
	)
	w := SessionWindow{
		SessionPrefix: "abc123def456", AgentName: "@worker",
		Start: mustTime(t, "2026-05-17T23:00:00Z"), End: mustTime(t, "2026-05-18T00:00:00Z"),
	}

	r := NewJSONLSignalReader()
	got, err := r.ReadSignalsBatch(context.Background(), []SessionWindow{w}, logFile,
		mustTime(t, "2026-05-17T23:53:00Z"), mustTime(t, "2026-05-17T23:55:00Z"))
	if err != nil {
		t.Fatalf("ReadSignalsBatch: %v", err)
	}
	if got[0].TotalEvents != 1 {
		t.Fatalf("TotalEvents = %d, want 1 (only the in-bound EDIT event)", got[0].TotalEvents)
	}
	if got[0].ToolTagCounts["EDIT"] != 1 {
		t.Errorf("ToolTagCounts = %+v, want EDIT:1", got[0].ToolTagCounts)
	}
}

// BenchmarkReadSignals_PerWindow simulates the pre-fix classify hot path: one
// ReadSignals call per interval, each re-scanning the whole file.
func BenchmarkReadSignals_PerWindow(b *testing.B) {
	logFile, windows := benchmarkFixture(b)
	r := NewJSONLSignalReader()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, w := range windows {
			if _, err := r.ReadSignals(ctx, []SessionWindow{w}, logFile); err != nil {
				b.Fatalf("ReadSignals: %v", err)
			}
		}
	}
}

// BenchmarkReadSignalsBatch is the fixed classify hot path: one scan for the
// whole batch.
func BenchmarkReadSignalsBatch(b *testing.B) {
	logFile, windows := benchmarkFixture(b)
	r := NewJSONLSignalReader()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.ReadSignalsBatch(ctx, windows, logFile, time.Time{}, time.Time{}); err != nil {
			b.Fatalf("ReadSignalsBatch: %v", err)
		}
	}
}

// benchmarkFixture builds a JSONL log with 5000 events spread across 200
// sessions and a batch of 500 windows (intervalBatch in internal/classify) —
// representative of one classify pass's worth of work.
func benchmarkFixture(b *testing.B) (logFile string, windows []SessionWindow) {
	b.Helper()
	const numSessions = 200
	const eventsPerSession = 25
	const numWindows = 500

	base, err := time.Parse(time.RFC3339, "2026-05-17T00:00:00Z")
	if err != nil {
		b.Fatalf("parse base time: %v", err)
	}
	var lines []string
	for s := 0; s < numSessions; s++ {
		session := fmt.Sprintf("sess%08dab", s)
		for e := 0; e < eventsPerSession; e++ {
			ts := base.Add(time.Duration(s*eventsPerSession+e) * time.Second).Format(time.RFC3339)
			lines = append(lines, fmt.Sprintf(`{"session":%q,"agent_name":"@w","ts":%q,"tag":"EDIT","file":"x.go"}`, session, ts))
		}
	}
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		b.Fatalf("write jsonl: %v", err)
	}

	for i := 0; i < numWindows; i++ {
		s := i % numSessions
		session := fmt.Sprintf("sess%08dab", s)
		windows = append(windows, SessionWindow{
			SessionPrefix: session[:12],
			AgentName:     "@w",
			Start:         base,
			End:           base.Add(time.Duration(numSessions*eventsPerSession) * time.Second),
		})
	}
	return path, windows
}
