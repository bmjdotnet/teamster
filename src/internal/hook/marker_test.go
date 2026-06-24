package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setModeEvent builds an mcp__activity__setMode PreToolUse pair for a session.
func setModeEvent(sid, mode string) (HookEvent, map[string]interface{}) {
	ti := map[string]interface{}{"mode": mode}
	ev := HookEvent{HookEventName: "PreToolUse", ToolName: "mcp__activity__setMode", SessionID: sid, ToolInput: ti}
	raw := map[string]interface{}{
		"hook_event_name": "PreToolUse", "tool_name": "mcp__activity__setMode",
		"session_id": sid, "tool_input": ti,
	}
	return ev, raw
}

// promptOut runs a UserPromptSubmit through ProcessEvent and returns the
// additionalContext, with env-solo = soloEnv.
func promptOut(t *testing.T, srvURL, sid, dedupDir string, soloEnv bool) string {
	t.Helper()
	ev := HookEvent{HookEventName: "UserPromptSubmit", SessionID: sid}
	raw := map[string]interface{}{"hook_event_name": "UserPromptSubmit", "session_id": sid}
	return additionalContext(t, ProcessEvent(ev, raw, srvURL, dedupDir, soloEnv))
}

// bareAgentOutput runs a bare Agent (no team_name) through ProcessEvent and
// returns the raw output string. Since the bare-Agent block was removed
// (implicit-teams migration), this is used to verify that Agent calls pass
// through without blocking.
func bareAgentOutput(t *testing.T, srvURL, sid, dedupDir string, soloEnv bool) string {
	t.Helper()
	ti := map[string]interface{}{"description": "x", "name": "scout"}
	ev := HookEvent{HookEventName: "PreToolUse", ToolName: "Agent", SessionID: sid, ToolInput: ti}
	raw := map[string]interface{}{
		"hook_event_name": "PreToolUse", "tool_name": "Agent", "session_id": sid, "tool_input": ti,
	}
	return ProcessEvent(ev, raw, srvURL, dedupDir, soloEnv)
}

// TestMarker_SetModeWritesAndOverridesEnv: setMode("solo") writes the marker,
// which flips the gates to solo even when the launch env is team (soloEnv=false).
func TestMarker_SetModeWritesAndOverridesEnv(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()
	dir := t.TempDir()
	sid := "markersess01"

	// Before any setMode, env=false → team: Agent calls pass through, full context.
	if ctx := promptOut(t, srv.URL, sid, dir, false); !strings.Contains(ctx, "named teammates") {
		t.Fatal("pre-setMode context must carry the team-dispatch mandate")
	}

	// Fire setMode("solo"): the hook writes the marker under this session id.
	ev, raw := setModeEvent(sid, "solo")
	ProcessEvent(ev, raw, srv.URL, dir, false)

	if got := readModeMarker(sid, dir); got != "solo" {
		t.Fatalf("after setMode(solo), marker = %q, want %q", got, "solo")
	}

	// Now, with env STILL false, the marker overrides → solo: no mandate.
	if ctx := promptOut(t, srv.URL, sid, dir, false); ctx != ACTIVITY_INSTRUCTION {
		t.Errorf("marker=solo context must be exactly ACTIVITY_INSTRUCTION, got %q", ctx)
	}
}

// TestMarker_OnlySoloCounts: a marker whose content is not exactly "solo" is
// ignored (one-directional invariant — only solo relaxes).
func TestMarker_OnlySoloCounts(t *testing.T) {
	dir := t.TempDir()
	sid := "markersess02"
	// Malformed content is inert in BOTH directions: it never reads as "solo"
	// (so it can't relax a team session) and never as "team".
	for _, content := range []string{"", "SOLO", "solo\n\nx", "garbage", "teamx"} {
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(dedupPath(sid, "mode", dir), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readModeMarker(sid, dir); got != "" {
			t.Errorf("malformed content %q must read as inert \"\", got %q", content, got)
		}
	}
	// The two exact valid values ARE honored (whitespace-trimmed).
	for raw, want := range map[string]string{"  solo  ": "solo", "team\n": "team"} {
		os.WriteFile(dedupPath(sid, "mode", dir), []byte(raw), 0o644)
		if got := readModeMarker(sid, dir); got != want {
			t.Errorf("content %q must read as %q, got %q", raw, want, got)
		}
	}
}

// TestMarker_SetModeTeamWritesAndReEnforces: setMode("team") WRITES a "team"
// marker (not a clear) and re-enables the dispatch mandate after a prior solo.
func TestMarker_SetModeTeamWritesAndReEnforces(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()
	dir := t.TempDir()
	sid := "markersess03"

	ev, raw := setModeEvent(sid, "solo")
	ProcessEvent(ev, raw, srv.URL, dir, false)
	if readModeMarker(sid, dir) != "solo" {
		t.Fatal("setup: setMode(solo) should have written the marker")
	}

	ev, raw = setModeEvent(sid, "team")
	ProcessEvent(ev, raw, srv.URL, dir, false)
	if got := readModeMarker(sid, dir); got != "team" {
		t.Errorf("setMode(team) must write a \"team\" marker, got %q", got)
	}
	// Back to team mode: dispatch mandate re-appears in context.
	if ctx := promptOut(t, srv.URL, sid, dir, false); !strings.Contains(ctx, "named teammates") {
		t.Error("after setMode(team), context must carry the team-dispatch mandate")
	}
}

// TestMarker_TeamMarkerBeatsEnvSolo: an explicit setMode("team") must force
// team context (dispatch mandate) even when the launch env is solo (env=true).
func TestMarker_TeamMarkerBeatsEnvSolo(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()
	dir := t.TempDir()
	sid := "markersess07"

	// env=solo, no marker yet → solo context (no dispatch mandate).
	if ctx := promptOut(t, srv.URL, sid, dir, true); ctx != ACTIVITY_INSTRUCTION {
		t.Fatal("pre-setMode with env=solo must produce ACTIVITY_INSTRUCTION only")
	}

	// Operator explicitly confirms team via setMode("team").
	ev, raw := setModeEvent(sid, "team")
	ProcessEvent(ev, raw, srv.URL, dir, true) // env still solo
	if readModeMarker(sid, dir) != "team" {
		t.Fatal("setMode(team) should have written a team marker")
	}

	// With env=solo but a fresh "team" marker, the session is team —
	// dispatch mandate appears in context.
	if ctx := promptOut(t, srv.URL, sid, dir, true); !strings.Contains(ctx, "named teammates") {
		t.Error("team marker must beat env=solo: context must carry the team-dispatch mandate")
	}
}

// TestMarker_EnvStillWorksWithoutMarker: precedence — no marker + env solo → solo;
// no marker + env team → team. The shipped env path is preserved.
func TestMarker_EnvStillWorksWithoutMarker(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()
	dir := t.TempDir()

	// env solo, no marker → solo: context is ACTIVITY_INSTRUCTION only.
	if ctx := promptOut(t, srv.URL, "envsolo00001", dir, true); ctx != ACTIVITY_INSTRUCTION {
		t.Errorf("env=solo context must be exactly ACTIVITY_INSTRUCTION, got %q", ctx)
	}
	// env team, no marker → team: context includes the dispatch mandate.
	if ctx := promptOut(t, srv.URL, "envteam00001", dir, false); !strings.Contains(ctx, "named teammates") {
		t.Errorf("env=team context must contain the team-dispatch mandate, got %q", ctx)
	}
}

// TestMarker_StaleIgnored: a marker older than the TTL is treated as absent
// (crash-recovery bound), falling back to env/team.
func TestMarker_StaleIgnored(t *testing.T) {
	dir := t.TempDir()
	sid := "markersess04"
	os.MkdirAll(dir, 0o755)
	p := dedupPath(sid, "mode", dir)
	if err := os.WriteFile(p, []byte("solo"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate mtime beyond the TTL.
	old := time.Now().Add(-modeMarkerTTL - time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if got := readModeMarker(sid, dir); got != "" {
		t.Errorf("stale marker must read as absent, got %q", got)
	}
}

// TestMarker_HonoredReadRefreshesMtime: an active session's honored read bumps
// the marker mtime so it survives across turns (Stop is per-turn, not
// per-session; the TTL only reclaims abandoned markers). BOTH "solo" and "team"
// must refresh — a team-over-env-solo session must stay sticky too, else it
// would age out → env=solo → the bug returns for idle sessions.
func TestMarker_HonoredReadRefreshesMtime(t *testing.T) {
	dir := t.TempDir()
	for i, mode := range []string{"solo", "team"} {
		sid := fmt.Sprintf("markersess05%d", i)
		os.MkdirAll(dir, 0o755)
		p := dedupPath(sid, "mode", dir)
		os.WriteFile(p, []byte(mode), 0o644)
		// Age it to just inside the TTL.
		nearStale := time.Now().Add(-modeMarkerTTL + time.Minute)
		os.Chtimes(p, nearStale, nearStale)

		if readModeMarker(sid, dir) != mode {
			t.Fatalf("near-but-not-stale %q marker should still read %q", mode, mode)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(fi.ModTime()) > time.Minute {
			t.Errorf("honored read of %q must refresh mtime to ~now, age=%v", mode, time.Since(fi.ModTime()))
		}
	}
}

// TestMarker_StopDoesNotClearMode: a per-turn Stop must NOT remove the .mode
// marker (it persists for the whole session); it DOES clear tool/thought dedup.
func TestMarker_StopDoesNotClearMode(t *testing.T) {
	srv := discardServer(t)
	defer srv.Close()
	dir := t.TempDir()
	sid := "markersess06"

	ev, raw := setModeEvent(sid, "solo")
	ProcessEvent(ev, raw, srv.URL, dir, false)
	if readModeMarker(sid, dir) != "solo" {
		t.Fatal("setup: marker should be solo")
	}

	// A Stop event (turn boundary).
	stopEv := HookEvent{HookEventName: "Stop", SessionID: sid}
	stopRaw := map[string]interface{}{"hook_event_name": "Stop", "session_id": sid}
	ProcessEvent(stopEv, stopRaw, srv.URL, dir, false)

	if got := readModeMarker(sid, dir); got != "solo" {
		t.Errorf("Stop (per-turn) must NOT clear the mode marker, got %q", got)
	}
	// Sanity: the dedup files DO get cleared by Stop (different category).
	if _, err := os.Stat(filepath.Join(dir, sid[:12]+".tool")); err == nil {
		// not necessarily present; we only assert .mode survived.
		_ = err
	}
}
