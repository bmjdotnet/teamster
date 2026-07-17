package server

import (
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
)

// TestRegisterSubagentStart_ConcurrentSameTypeSpawnsGetUniqueNames covers the
// uniqueify path in registerNewSubagentInstance: four Agent-tool spawns of
// the same subagent_type ("Explore"), each with its own distinct
// agent_id (the real per-instance discriminator), must each
// register a distinct roster entry — @Explore, @Explore-2, @Explore-3,
// @Explore-4 — all with relationship "subagent" (a non-empty spawner type
// was queued for every one of them).
func TestRegisterSubagentStart_ConcurrentSameTypeSpawnsGetUniqueNames(t *testing.T) {
	s := newModelCaptureTestServer(t)

	// Simulate four Agent-tool PreToolUse calls all spawning subagent_type
	// "Explore" without an explicit name — resolveSubagentName would queue
	// each with displayName == agentType.
	for i := 0; i < 4; i++ {
		s.subagentNames.record("sess-concurrent", "Explore", "Explore", "orchestrator")
	}

	agentIDs := []string{
		"aexplore-001",
		"aexplore-002",
		"aexplore-003",
		"aexplore-004",
	}
	for _, aid := range agentIDs {
		s.dispatchObservability(hook.HookEvent{
			HookEventName: "SubagentStart",
			SessionID:     "sess-concurrent",
			AgentType:     "Explore",
			AgentID:       aid,
		}, map[string]interface{}{})
	}

	wantNames := []string{"@Explore", "@Explore-2", "@Explore-3", "@Explore-4"}
	for _, name := range wantNames {
		entry := waitForRosterEntry(t, s.obsStore, "sess-concurrent", name)
		if entry.Relationship != "subagent" {
			t.Errorf("entry %s relationship = %q, want %q", name, entry.Relationship, "subagent")
		}
	}
}

// TestSubagentStop_ResolvesCorrectAutoNumberedName is the regression for the
// SubagentStop instanceRegistry lookup: with two same-type subagents
// registered (@Explore and, after uniqueify, @Explore-2), a SubagentStop
// carrying the SECOND instance's own agent_id must close only
// that instance — not misattribute to the raw agentType-derived "@Explore"
// both instances share.
func TestSubagentStop_ResolvesCorrectAutoNumberedName(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName:       "SubagentStart",
		SessionID:           "sess-b",
		AgentType:           "Explore",
		AgentID: "aexplore-001",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-b", "@Explore")

	s.dispatchObservability(hook.HookEvent{
		HookEventName:       "SubagentStart",
		SessionID:           "sess-b",
		AgentType:           "Explore",
		AgentID: "aexplore-002",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-b", "@Explore-2")

	// Stop the SECOND instance specifically, by its own transcript path.
	s.dispatchObservability(hook.HookEvent{
		HookEventName:       "SubagentStop",
		SessionID:           "sess-b",
		AgentType:           "Explore",
		AgentID: "aexplore-002",
	}, map[string]interface{}{})

	var firstStatus, secondStatus observability.SessionStatus
	for _, snap := range s.sessions.Snapshot() {
		if snap.SessionID != "sess-b" {
			continue
		}
		switch snap.AgentName {
		case "@Explore":
			firstStatus = snap.Status
		case "@Explore-2":
			secondStatus = snap.Status
		}
	}
	if secondStatus != observability.SessionStatusStopped {
		t.Errorf("@Explore-2 status = %q, want %q — CloseAgent must resolve the auto-numbered name", secondStatus, observability.SessionStatusStopped)
	}
	if firstStatus != observability.SessionStatusActive {
		t.Errorf("@Explore status = %q, want %q — must not be closed by the second instance's stop", firstStatus, observability.SessionStatusActive)
	}

	s.regMu.Lock()
	_, firstStillRegistered := s.instanceRegistry["sess-b|aexplore-001"]
	_, secondStillRegistered := s.instanceRegistry["sess-b|aexplore-002"]
	s.regMu.Unlock()
	if !firstStillRegistered {
		t.Error("first agent's instanceRegistry entry was removed, want it to survive the second agent's SubagentStop")
	}
	if secondStillRegistered {
		t.Error("second agent's instanceRegistry entry should have been deleted on its own SubagentStop")
	}
}

// TestSubagentNameMap_PopExpiresStaleEntries covers pop()'s TTL check: an
// entry recorded more than subagentEntryTTL ago must not be reported as a
// confirmed match (the Agent-tool spawn it predicted never sent a timely
// SubagentStart), even though it's still dequeued from the FIFO and mirrored
// into resolved as a display fallback.
func TestSubagentNameMap_PopExpiresStaleEntries(t *testing.T) {
	m := &subagentNameMap{}
	m.record("sess-ttl", "Explore", "stale-scout", "lead")

	key := subagentNameKey("sess-ttl", "Explore")
	m.mu.Lock()
	queue := m.m[key]
	if len(queue) != 1 {
		m.mu.Unlock()
		t.Fatalf("expected 1 queued entry before aging it, got %d", len(queue))
	}
	queue[0].recordedAt = time.Now().Add(-(subagentEntryTTL + time.Second))
	m.mu.Unlock()

	name, spawner := m.pop("sess-ttl", "Explore")
	if name != "" || spawner != "" {
		t.Errorf("pop() of expired entry = (%q, %q), want (\"\", \"\") — stale entry must not be reported as a confirmed match", name, spawner)
	}

	m.mu.Lock()
	remaining := len(m.m[key])
	m.mu.Unlock()
	if remaining != 0 {
		t.Errorf("FIFO still has %d entries after popping the expired one, want 0 (pop must still dequeue)", remaining)
	}

	if resolved, _ := m.peek("sess-ttl", "Explore"); resolved != "@stale-scout" {
		t.Errorf("peek() after an expired pop = %q, want %q (still mirrored into resolved as a display fallback)", resolved, "@stale-scout")
	}
}
