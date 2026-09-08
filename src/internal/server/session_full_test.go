package server

import "testing"

// WP11 §1: buildRecord must keep writing the 12-char `session` field
// (feed's display key, unchanged) while additively carrying the untruncated
// id in `session_full`, capped to sessions.session_id's VARCHAR(64) width —
// not the old 12-char cap. Every existing reader keys off `session`; nothing
// reads `session_full` yet, so this is purely additive.
func TestBuildRecord_SessionFullField(t *testing.T) {
	s := &Server{}
	full := "90274175-e389-4d47-8a74-cec717d9dd98"
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"session_id":      full,
	}
	rec := s.buildRecord(data)

	if got, want := rec["session"], full[:12]; got != want {
		t.Errorf("session = %v, want %q (unchanged 12-char truncation)", got, want)
	}
	if got := rec["session_full"]; got != full {
		t.Errorf("session_full = %v, want %q (untruncated)", got, full)
	}
}

// A session id longer than the sessions.session_id column width (64 chars)
// must not be written unbounded into session_full — cap it there too, per
// WP11's own note, rather than assuming every session_id is UUID-length.
func TestBuildRecord_SessionFullCappedAt64(t *testing.T) {
	s := &Server{}
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"session_id":      long,
	}
	rec := s.buildRecord(data)

	got, _ := rec["session_full"].(string)
	if len(got) != 64 {
		t.Errorf("session_full length = %d, want 64 (capped)", len(got))
	}
	if got != long[:64] {
		t.Errorf("session_full = %q, want first 64 chars of input", got)
	}
	if gotSession, _ := rec["session"].(string); len(gotSession) != 12 {
		t.Errorf("session length = %d, want 12 (unaffected by the wider cap)", len(gotSession))
	}
}

// A session id at or under 12 chars must round-trip identically into both
// fields — no padding, no divergence for the short-id case.
func TestBuildRecord_SessionFullShortSessionMatchesSession(t *testing.T) {
	s := &Server{}
	short := "sess-short1"
	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"session_id":      short,
	}
	rec := s.buildRecord(data)

	if got := rec["session"]; got != short {
		t.Errorf("session = %v, want %q", got, short)
	}
	if got := rec["session_full"]; got != short {
		t.Errorf("session_full = %v, want %q", got, short)
	}
}

// The relay-passthrough path (a pre-enriched line already carrying tag +
// display) is written as-is — it is not this code's job to retrofit
// session_full onto a line the origin hub already finished building.
func TestBuildRecord_RelayPassthroughDoesNotInjectSessionFull(t *testing.T) {
	s := &Server{}
	data := map[string]interface{}{
		"ts":      "2026-06-16T00:00:00Z",
		"tag":     " ACT",
		"display": "run sweep query",
		"session": "abcdefabcdef",
	}
	rec := s.buildRecord(data)
	if _, present := rec["session_full"]; present {
		t.Errorf("relay passthrough should not gain a session_full field it did not already carry, got %v", rec["session_full"])
	}
}
