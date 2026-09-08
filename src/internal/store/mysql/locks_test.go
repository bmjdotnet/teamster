package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// These tests target the MySQL named-lock 64-character cap (error 4163):
// GET_LOCK/RELEASE_LOCK reject any name over 64 bytes outright rather than
// truncating it, and withFocusLock's original raw-concatenation name
// ("teamster_focus:"+sessionID+":"+agentName) silently lost every focus
// interval for a long-enough agent name for six weeks — the acquire error
// was real, but the fire-and-forget call site discarded it. See the
// parent-ref-fifo-fix WU for the forensic trace.

// TestNamedLockID_BoundedLength fails against the pre-fix code: raw
// concatenation of a realistic long session ID + agent name/entity ID
// exceeds 64 characters and MySQL rejects the GET_LOCK acquire outright.
func TestNamedLockID_BoundedLength(t *testing.T) {
	sessionID := "f823d271-0a18-429c-921a-6215db902bf7" // real UUID length, 36 chars
	longAgentNames := []string{
		"@teamster:implementer", // 21 chars — one of the two rows that actually broke
		"@some-much-longer-teammate-name-than-usual",
		strings.Repeat("x", 200), // pathological, must still be bounded
	}
	for _, agent := range longAgentNames {
		name := namedLockID("teamster_focus", sessionID, agent)
		if len(name) > maxLockNameLen {
			t.Errorf("namedLockID(%q, %q) = %q (%d bytes), want <= %d", sessionID, agent, name, len(name), maxLockNameLen)
		}
	}

	// withStateLock's entity_id column is VARCHAR(128) with no length
	// validation anywhere above the store — a realistic long workunit/
	// outcome slug is well within schema limits and must still fit.
	longEntityID := strings.Repeat("a", 128)
	name := namedLockID("teamster_state", "workunit", longEntityID)
	if len(name) > maxLockNameLen {
		t.Errorf("namedLockID(%q) = %q (%d bytes), want <= %d", longEntityID, name, len(name), maxLockNameLen)
	}
}

// TestNamedLockID_Deterministic verifies the same (kind, parts) always
// produces the same name — GET_LOCK/RELEASE_LOCK only serialize callers that
// agree on the name, so any nondeterminism silently stops the lock from
// doing its job.
func TestNamedLockID_Deterministic(t *testing.T) {
	a := namedLockID("teamster_focus", "sess-1", "@agent")
	b := namedLockID("teamster_focus", "sess-1", "@agent")
	if a != b {
		t.Errorf("namedLockID not deterministic: %q != %q", a, b)
	}
}

// TestNamedLockID_CollisionResistant is the correctness trap the naive fix
// (truncate to 64 bytes) falls into: two distinct inputs that share a long
// common prefix, or that differ only in where a part boundary falls, must
// never produce the same lock name — a silent collision would serialize two
// unrelated callers against each other with no error, the same invisible
// failure mode as the original bug.
func TestNamedLockID_CollisionResistant(t *testing.T) {
	cases := []struct {
		name   string
		aKind  string
		aParts []string
		bKind  string
		bParts []string
	}{
		{
			name:   "shared long prefix, differ only past the truncation point",
			aKind:  "teamster_focus",
			aParts: []string{"session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "@agent-one"},
			bKind:  "teamster_focus",
			bParts: []string{"session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "@agent-two"},
		},
		{
			name:   "part-boundary ambiguity: (ab,c) vs (a,bc)",
			aKind:  "teamster_state",
			aParts: []string{"ab", "c"},
			bKind:  "teamster_state",
			bParts: []string{"a", "bc"},
		},
		{
			name:   "different kind, identical parts",
			aKind:  "teamster_focus",
			aParts: []string{"same", "parts"},
			bKind:  "teamster_state",
			bParts: []string{"same", "parts"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := namedLockID(tc.aKind, tc.aParts...)
			b := namedLockID(tc.bKind, tc.bParts...)
			if a == b {
				t.Errorf("collision: namedLockID(%q, %v) == namedLockID(%q, %v) == %q", tc.aKind, tc.aParts, tc.bKind, tc.bParts, a)
			}
		})
	}
}

// TestOpenFocusInterval_LongAgentName is the integration-level regression:
// against real MySQL, a long-but-realistic agent name must not make
// OpenFocusInterval fail, and a real wms_intervals row must land. This is
// the actual production symptom (a silently-dropped focus interval), not
// just a property of the name-construction helper.
func TestOpenFocusInterval_LongAgentName(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	// "teamster:implementer" is one of the two agent names that actually
	// broke in production; pad it further to also cover names longer than
	// any observed so far.
	key := store.SessionKey{
		SessionID: "f823d271-0a18-429c-921a-6215db902bf7",
		AgentName: "@teamster:implementer-with-a-much-longer-suffix-than-usual",
	}

	if err := s.OpenFocusInterval(ctx, key, "outcome", oid); err != nil {
		t.Fatalf("OpenFocusInterval with long agent name: %v", err)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='`+key.SessionID+`' AND agent_name='`+key.AgentName+`' AND ended_at IS NULL`,
		1, "focus interval row for the long agent name")
}
