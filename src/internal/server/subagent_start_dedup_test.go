package server

import (
	"testing"

	"github.com/bmjdotnet/teamster/internal/hook"
)

// TestSubagentStartDoesNotDuplicateRosterEntryOnTurnResume is the regression
// for the SubagentStart roster-dedup fix: SubagentStart fires on every
// Agent-Teams teammate turn-resume (mailbox wakeup) carrying the SAME
// agent_type as the teammate's original registration — a second
// registration for the same entity must not mint a second roster_id.
func TestSubagentStartDoesNotDuplicateRosterEntryOnTurnResume(t *testing.T) {
	s := newModelCaptureTestServer(t)

	// First SubagentStart: genuinely new teammate, must register.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-1",
		AgentType:     "collector",
		AgentID:       "acollector111",
	}, map[string]interface{}{})
	first := waitForRosterEntry(t, s.obsStore, "sess-1", "@collector")

	// Second SubagentStart: same teammate resuming on a later mailbox
	// wakeup — same session, same agent_type, DIFFERENT agent_id (a fresh
	// per-turn id, not the discriminator the fix keys on for teammates).
	// Must NOT create a second roster entry.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-1",
		AgentType:     "collector",
		AgentID:       "acollector222",
	}, map[string]interface{}{})

	second := waitForRosterEntry(t, s.obsStore, "sess-1", "@collector")
	if second.RosterID != first.RosterID {
		t.Errorf("roster_id changed across turn-resume: first=%q second=%q, want identical (no duplicate registration)", first.RosterID, second.RosterID)
	}
}

// TestSubagentStartRegistersGenuinelyNewAgent proves the fix doesn't just
// suppress ALL SubagentStart registration: a different agent_type on the
// same session (a distinct teammate, or the very first event for this
// entity) must still register normally.
func TestSubagentStartRegistersGenuinelyNewAgent(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-1",
		AgentType:     "collector",
		AgentID:       "acollector111",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-1", "@collector")

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-1",
		AgentType:     "ux-design",
		AgentID:       "aux-design111",
	}, map[string]interface{}{})
	entry := waitForRosterEntry(t, s.obsStore, "sess-1", "@ux-design")
	if entry.AgentName != "@ux-design" {
		t.Errorf("second agent's roster entry AgentName = %q, want @ux-design", entry.AgentName)
	}
}
