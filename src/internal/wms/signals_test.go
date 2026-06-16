package wms

import (
	"context"
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
