package server

import (
	"strings"
	"testing"
)

// A shape-identical fake 48-hex secret standing in for the DB password that
// leaked to the feed (gap analysis). buildRecord is the JSONL-append choke
// point (handleEvent calls it before marshalling + writing), so the secret must
// be gone from every command-bearing field it returns. The fake exercises the
// same -p'<hex>' / MYSQL_PWD= shapes without persisting a real credential.
const liveSecret = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// Go-enriched path: the Go hook client already set _bash_cmd. buildRecord must
// still scrub it (defense in depth — the client also redacts, but the choke
// point is the guarantee).
func TestBuildRecord_RedactsEnrichedBashCmd(t *testing.T) {
	s := &Server{}
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"session_id":      "abc123def456",
		"_bash_cmd":       `mysql -h 127.0.0.1 -P 3306 -u teamster -p'` + liveSecret + `' teamster -N -e "SELECT 1"`,
	}
	rec := s.buildRecord(data)
	got, _ := rec["bash_cmd"].(string)
	if strings.Contains(got, liveSecret) {
		t.Fatalf("choke point leaked secret in bash_cmd: %q", got)
	}
	if !strings.Contains(got, "-p<redacted>") {
		t.Errorf("expected -p<redacted>, got %q", got)
	}
	if !strings.Contains(got, "SELECT 1") {
		t.Errorf("expected command structure preserved, got %q", got)
	}
}

// Raw Python-remote-client path: the thin client forwards tool_input.command
// verbatim (no _bash_cmd). hookd enriches it server-side into _bash_cmd via
// EnrichRecord, then buildRecord persists it. The choke point must redact this
// path too — this is the un-enriched arrival the brief calls out.
func TestBuildRecord_RedactsRawPythonClientCommand(t *testing.T) {
	s := &Server{}
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"session_id":      "remotesession",
		"host":            "remotehost",
		"tool_input": map[string]interface{}{
			"command": "MYSQL_PWD=" + liveSecret + " mysql -u teamster teamster -N -e 'SELECT 1'",
		},
	}
	rec := s.buildRecord(data)
	got, _ := rec["bash_cmd"].(string)
	if got == "" {
		t.Fatal("expected bash_cmd to be enriched from raw tool_input.command")
	}
	if strings.Contains(got, liveSecret) {
		t.Fatalf("choke point leaked secret from raw python-client command: %q", got)
	}
	if !strings.Contains(got, "MYSQL_PWD=<redacted>") {
		t.Errorf("expected MYSQL_PWD=<redacted>, got %q", got)
	}
}

// Relay passthrough path: a pre-enriched JSONL line (has tag + display) is
// returned as-is, but its bash_cmd must still pass through the redactor — the
// replica's safety net against an un-redacted line arriving by any path.
func TestBuildRecord_RedactsRelayPassthrough(t *testing.T) {
	s := &Server{}
	data := map[string]interface{}{
		"ts":       "2026-06-16T00:00:00Z",
		"tag":      " ACT",
		"display":  "run sweep query",
		"bash_cmd": "mysql -u teamster -p" + liveSecret + " teamster",
	}
	rec := s.buildRecord(data)
	got, _ := rec["bash_cmd"].(string)
	if strings.Contains(got, liveSecret) {
		t.Fatalf("relay passthrough leaked secret: %q", got)
	}
	if !strings.Contains(got, "-p<redacted>") {
		t.Errorf("expected -p<redacted> in passthrough, got %q", got)
	}
}

// Negative: an ordinary command flows through the choke point unchanged.
func TestBuildRecord_OrdinaryCommandUntouched(t *testing.T) {
	s := &Server{}
	want := "go test ./... -p 4"
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"session_id":      "s",
		"_bash_cmd":       want,
	}
	rec := s.buildRecord(data)
	if got, _ := rec["bash_cmd"].(string); got != want {
		t.Errorf("ordinary command altered: got %q want %q", got, want)
	}
}
