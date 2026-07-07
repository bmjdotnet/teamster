package wms

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// resetConnectionState restores the package-level connection identity state
// (ConnectionClientName) after each test, and points ~/.claude/current-
// session-id at an isolated fake HOME so tests never touch the real operator
// file — this repo's own CLAUDE.md notes this host constantly has active
// Claude sessions rewriting that file.
func resetConnectionState(t *testing.T) (fakeHome string) {
	t.Helper()
	prevClient := ConnectionClientName
	t.Cleanup(func() { ConnectionClientName = prevClient })
	fakeHome = t.TempDir()
	t.Setenv("HOME", fakeHome)
	return fakeHome
}

// writeFallbackFile seeds ~/.claude/current-session-id under the given fake
// HOME with a sentinel value, so a test can assert whether resolveSessionID
// did or did not read it.
func writeFallbackFile(t *testing.T, fakeHome, sessionID string) {
	t.Helper()
	dir := filepath.Join(fakeHome, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fake .claude dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current-session-id"), []byte(sessionID), 0o644); err != nil {
		t.Fatalf("write fake current-session-id: %v", err)
	}
}

const fallbackSentinel = "claude-session-from-file-SENTINEL"

// TestResolveSessionID_ClaudeWithFallback: no clientInfo, no turn metadata —
// existing behavior is unchanged, the file fallback is still used.
func TestResolveSessionID_ClaudeWithFallback(t *testing.T) {
	fakeHome := resetConnectionState(t)
	writeFallbackFile(t, fakeHome, fallbackSentinel)
	ConnectionClientName = "" // no clientInfo ever received (legacy/back-compat)

	m := &Meta{}
	resolveSessionID(m)

	if m.SessionID != fallbackSentinel {
		t.Errorf("SessionID = %q, want the fallback file value %q", m.SessionID, fallbackSentinel)
	}
	if got := runtimeTag(); got != "claude" {
		t.Errorf("runtimeTag() = %q, want claude", got)
	}
}

// TestResolveSessionID_ClaudeCodeClientInfo: clientInfo.name == "claude-code"
// (the confirmed real value) behaves the same as the no-clientInfo case.
func TestResolveSessionID_ClaudeCodeClientInfo(t *testing.T) {
	fakeHome := resetConnectionState(t)
	writeFallbackFile(t, fakeHome, fallbackSentinel)
	ConnectionClientName = "claude-code"

	m := &Meta{}
	resolveSessionID(m)

	if m.SessionID != fallbackSentinel {
		t.Errorf("SessionID = %q, want the fallback file value %q", m.SessionID, fallbackSentinel)
	}
}

// TestResolveSessionID_CodexWithTurnMetadata: session id comes from
// x-codex-turn-metadata, never from the file — even though the file is
// present and would return a plainly different (wrong) value if read.
func TestResolveSessionID_CodexWithTurnMetadata(t *testing.T) {
	fakeHome := resetConnectionState(t)
	writeFallbackFile(t, fakeHome, fallbackSentinel)
	ConnectionClientName = codexClientName

	const codexSessionID = "019f3b34-5d26-7441-a825-732013587709"
	m := &Meta{CodexTurn: &CodexTurnMeta{SessionID: codexSessionID, Model: "gpt-5.5"}}
	resolveSessionID(m)

	if m.SessionID != codexSessionID {
		t.Errorf("SessionID = %q, want the Codex turn-metadata session id %q", m.SessionID, codexSessionID)
	}
	if m.SessionID == fallbackSentinel {
		t.Fatal("resolveSessionID read the Claude fallback file despite present turn metadata")
	}
	if got := runtimeTag(); got != "codex" {
		t.Errorf("runtimeTag() = %q, want codex", got)
	}
}

// TestResolveSessionID_CodexWithoutTurnMetadata: turn metadata absent (e.g. a
// future Codex release drops the field) but the connection is still
// identifiable as Codex via clientInfo. The fail-safe requirement: bucket to
// "unknown-codex", and the fallback file must NOT be read — asserted by
// checking the result isn't the sentinel the file holds.
func TestResolveSessionID_CodexWithoutTurnMetadata(t *testing.T) {
	fakeHome := resetConnectionState(t)
	writeFallbackFile(t, fakeHome, fallbackSentinel)
	ConnectionClientName = codexClientName

	m := &Meta{} // no CodexTurn at all
	resolveSessionID(m)

	if m.SessionID != "unknown-codex" {
		t.Errorf("SessionID = %q, want unknown-codex bucket", m.SessionID)
	}
	if m.SessionID == fallbackSentinel {
		t.Fatal("resolveSessionID read the Claude fallback file for a Codex connection with no turn metadata")
	}
}

// TestResolveSessionID_ThirdPartyClient: an MCP client that is neither Claude
// Code nor Codex (an inspector, another agent CLI) must never land on a live
// Claude Code session id (redteam n6's generalized refusal).
func TestResolveSessionID_ThirdPartyClient(t *testing.T) {
	fakeHome := resetConnectionState(t)
	writeFallbackFile(t, fakeHome, fallbackSentinel)
	ConnectionClientName = "mcp-inspector"

	m := &Meta{}
	resolveSessionID(m)

	if m.SessionID == fallbackSentinel {
		t.Fatal("resolveSessionID read the Claude fallback file for an unrecognized third-party client")
	}
	if m.SessionID != "unknown-unknown" {
		t.Errorf("SessionID = %q, want an unknown-* bucket (got runtimeTag=%q)", m.SessionID, runtimeTag())
	}
	if got := runtimeTag(); got != "unknown" {
		t.Errorf("runtimeTag() = %q, want unknown", got)
	}
}

// TestResolveSessionID_ExplicitMetaSessionIDTrusted: if some future client
// populates the generic _meta.session_id directly, it's trusted as-is and the
// fallback file is never consulted — unaffected by this change, and
// independent of ConnectionClientName.
func TestResolveSessionID_ExplicitMetaSessionIDTrusted(t *testing.T) {
	fakeHome := resetConnectionState(t)
	writeFallbackFile(t, fakeHome, fallbackSentinel)
	ConnectionClientName = "some-other-client"

	m := &Meta{SessionID: "explicit-session-123"}
	resolveSessionID(m)

	if m.SessionID != "explicit-session-123" {
		t.Errorf("SessionID = %q, want the explicit value preserved", m.SessionID)
	}
}

// TestFallbackEligible_RuntimeEnvOverridesClientInfo: the TEAMSTER_RUNTIME=
// codex belt-and-suspenders discriminator refuses the fallback even if
// clientInfo contradicts it (defense in depth against protocol drift).
func TestFallbackEligible_RuntimeEnvOverridesClientInfo(t *testing.T) {
	resetConnectionState(t)
	ConnectionClientName = claudeCodeClientName
	t.Setenv("TEAMSTER_RUNTIME", "codex")

	if fallbackEligible() {
		t.Error("fallbackEligible() = true, want false when TEAMSTER_RUNTIME=codex regardless of clientInfo")
	}
	if got := runtimeTag(); got != "codex" {
		t.Errorf("runtimeTag() = %q, want codex (env should win)", got)
	}
}

// TestRuntimeTagAppliedOnCreate: end-to-end through HandleToolCall — creating
// an outcome over a simulated Codex connection (turn metadata present) tags
// the entity runtime:codex; over a plain Claude connection, runtime:claude;
// over an unrecognized client, runtime:unknown. Source is "classifier",
// mirroring applyCreatorUserTag. Requires TEAMSTER_TEST_MYSQL_DSN (see
// newStewardStore); SKIPs otherwise.
func TestRuntimeTagAppliedOnCreate(t *testing.T) {
	store, oid := newStewardStore(t)
	resetConnectionState(t)

	// Codex connection, turn metadata present.
	ConnectionClientName = codexClientName
	rawCodex, err := json.Marshal(map[string]interface{}{
		"name": ToolCreateOutcome,
		"arguments": map[string]interface{}{
			"id": "out-codex", "title": "codex-created outcome",
		},
		"_meta": map[string]interface{}{
			"x-codex-turn-metadata": map[string]interface{}{
				"session_id": "019f3b34-5d26-7441-a825-732013587709",
				"model":      "gpt-5.5",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal codex params: %v", err)
	}
	if _, ce := HandleToolCall(store, noopEngine{}, rawCodex); ce != nil {
		t.Fatalf("createOutcome (codex): %v", ce)
	}
	if ok, src := entityHasTag(t, store, wms.EntityOutcome, "out-codex", "runtime", "codex"); !ok || src != "classifier" {
		t.Errorf("codex outcome runtime tag: ok=%v source=%q, want true/classifier", ok, src)
	}

	// Claude connection (no clientInfo — legacy/back-compat path).
	ConnectionClientName = ""
	if _, ce := call(t, store, ToolCreateOutcome, map[string]interface{}{
		"id": "out-claude", "title": "claude-created outcome",
	}); ce != nil {
		t.Fatalf("createOutcome (claude): %v", ce)
	}
	if ok, src := entityHasTag(t, store, wms.EntityOutcome, "out-claude", "runtime", "claude"); !ok || src != "classifier" {
		t.Errorf("claude outcome runtime tag: ok=%v source=%q, want true/classifier", ok, src)
	}

	// Unrecognized third-party client.
	ConnectionClientName = "mcp-inspector"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": "wu-unknown", "title": "unknown-client workunit", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit (unknown client): %v", ce)
	}
	if ok, src := entityHasTag(t, store, wms.EntityWorkUnit, "wu-unknown", "runtime", "unknown"); !ok || src != "classifier" {
		t.Errorf("unknown-client workunit runtime tag: ok=%v source=%q, want true/classifier", ok, src)
	}
}
