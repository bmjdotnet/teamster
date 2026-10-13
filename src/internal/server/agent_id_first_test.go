package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/store"
)

func startEvent(session, agentType, agentID string) hook.HookEvent {
	return hook.HookEvent{HookEventName: "SubagentStart", SessionID: session, AgentType: agentType, AgentID: agentID}
}

func wipeInstanceState(s *Server, session string) {
	s.regMu.Lock()
	s.instanceRegistry = nil
	s.regMu.Unlock()
	s.subagentNames.clearSession(session)
}

func TestAgentIDFirst_RespawnDoesNotAdoptDeadPredecessor(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	s.dispatchObservability(startEvent("S", "redteam", "aredteam-OLD"), map[string]interface{}{})
	old := waitForRosterEntry(t, s.obsStore, "S", "@redteam")
	if old.AgentID != "aredteam-OLD" {
		t.Fatalf("old AgentID = %q, want aredteam-OLD (read-back)", old.AgentID)
	}

	wipeInstanceState(s, "S")
	s.dispatchObservability(startEvent("S", "redteam", "aredteam-NEW"), map[string]interface{}{})
	fresh := waitForRosterEntry(t, s.obsStore, "S", "@redteam-2")
	if fresh.AgentID != "aredteam-NEW" || fresh.RosterID == old.RosterID {
		t.Fatalf("respawn row = %+v, want new roster_id with AgentID NEW", fresh)
	}
	again, err := s.obsStore.GetRosterEntry(ctx, old.RosterID)
	if err != nil {
		t.Fatal(err)
	}
	if again.AgentID != "aredteam-OLD" || again.AgentName != "@redteam" {
		t.Fatalf("predecessor row mutated: %+v", again)
	}
}

func TestAgentIDFirst_ResumeAfterWipeWithStaleFIFO(t *testing.T) {
	s := newModelCaptureTestServer(t)
	s.dispatchObservability(startEvent("S", "Explore", "aX"), map[string]interface{}{})
	first := waitForRosterEntry(t, s.obsStore, "S", "@Explore")

	wipeInstanceState(s, "S")
	s.subagentNames.record("S", "Explore", "Explore", "someparent")
	s.dispatchObservability(startEvent("S", "Explore", "aX"), map[string]interface{}{})

	if _, err := s.obsStore.ResolveRosterID(context.Background(), "S", "@Explore-2"); err == nil {
		t.Fatal("resume minted @Explore-2; want existing @Explore row reused")
	}
	if rid, _ := s.obsStore.ResolveRosterID(context.Background(), "S", "@Explore"); rid != first.RosterID {
		t.Fatalf("roster_id changed: %q vs %q", rid, first.RosterID)
	}
	if name, spawner := s.subagentNames.pop("S", "Explore"); name != "@Explore" || spawner != "someparent" {
		t.Fatalf("stale FIFO record consumed: name=%q spawner=%q", name, spawner)
	}
}

func TestAgentIDFirst_LegacyAdoptStampsAgentID(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	sid := "S"
	now := time.Now().UTC()
	if err := s.obsStore.UpsertRosterEntry(ctx, store.RosterEntry{
		RosterID: "r-legacy", SessionID: &sid, AgentName: "@old", Host: "testhost",
		Runtime: "claude_code", Relationship: "teammate", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	s.dispatchObservability(startEvent("S", "old", "aZ"), map[string]interface{}{})
	got := waitForRosterEntry(t, s.obsStore, "S", "@old")
	if got.RosterID != "r-legacy" || got.AgentID != "aZ" {
		t.Fatalf("legacy adopt: roster_id=%q agent_id=%q, want r-legacy/aZ", got.RosterID, got.AgentID)
	}
}

func TestAgentIDFirst_EarlyPathRefusesToOverwriteOtherInstance(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	sid := "S"
	if err := s.obsStore.UpsertRosterEntry(ctx, store.RosterEntry{
		RosterID: "r-red", SessionID: &sid, AgentName: "@redteam", Host: "testhost",
		Runtime: "claude_code", Relationship: "teammate", AgentID: "aOLD", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "PreToolUse", SessionID: "S", AgentType: "redteam", AgentID: "aNEW", ToolName: "Read",
	}, map[string]interface{}{})
	time.Sleep(200 * time.Millisecond)
	got, err := s.obsStore.GetRosterEntry(ctx, "r-red")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "aOLD" {
		t.Fatalf("early path overwrote row: AgentID=%q", got.AgentID)
	}
	if rid, _ := s.obsStore.ResolveRosterID(ctx, "S", "@redteam"); rid != "r-red" {
		t.Fatalf("roster_id changed to %q", rid)
	}
	s.dispatchObservability(startEvent("S", "redteam", "aNEW"), map[string]interface{}{})
	fresh := waitForRosterEntry(t, s.obsStore, "S", "@redteam-2")
	if fresh.AgentID != "aNEW" {
		t.Fatalf("@redteam-2 AgentID=%q", fresh.AgentID)
	}
}

func healAttempts(s *Server, key string) int {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return s.instanceRegistry[key].healAttempts
}

func waitHealAttempts(t *testing.T, s *Server, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if healAttempts(s, key) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("healAttempts never reached %d (got %d)", want, healAttempts(s, key))
}

func TestAgentIDFirst_SelfHealOnToolEventsBounded(t *testing.T) {
	old := selfHealMinSpacing
	selfHealMinSpacing = 0
	t.Cleanup(func() { selfHealMinSpacing = old })
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	childDir := filepath.Join(dir, "sess")
	transcript := childDir + ".jsonl"

	s.dispatchObservability(startEvent("S", "startup-analyst", "aspawner"), map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "S", "@startup-analyst")

	ev := hook.HookEvent{HookEventName: "SubagentStart", SessionID: "S", AgentType: "worker", AgentID: "aheal", TranscriptPath: transcript}
	s.dispatchObservability(ev, map[string]interface{}{})
	entry := waitForRosterEntry(t, s.obsStore, "S", "@worker")
	if entry.ParentRef != nil {
		t.Fatalf("precondition: ParentRef=%q, want nil", *entry.ParentRef)
	}
	key := "S|aheal"

	tool := ev
	tool.HookEventName = "PreToolUse"
	tool.ToolName = "Read"

	writeSidecar(t, childDir, "aheal", subagentMeta{ParentAgentID: "aspawner", SpawnDepth: 1})
	s.resolveSubagentName(tool, map[string]interface{}{})
	waitHealAttempts(t, s, key, 1)
	parentID, _ := s.obsStore.ResolveRosterID(ctx, "S", "@startup-analyst")
	deadline := time.Now().Add(2 * time.Second)
	var got store.RosterEntry
	for time.Now().Before(deadline) {
		got, _ = s.obsStore.GetRosterEntry(ctx, entry.RosterID)
		if got.ParentRef != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.ParentRef == nil || *got.ParentRef != parentID || got.Relationship != "subagent" {
		t.Fatalf("after tool event: ParentRef=%v rel=%q, want %q/subagent", got.ParentRef, got.Relationship, parentID)
	}

	// Bounded: a second instance with an unreadable sidecar stops at the cap.
	ev2 := hook.HookEvent{HookEventName: "SubagentStart", SessionID: "S", AgentType: "other", AgentID: "abound", TranscriptPath: filepath.Join(dir, "nope") + ".jsonl"}
	s.dispatchObservability(ev2, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "S", "@other")
	tool2 := ev2
	tool2.HookEventName = "PreToolUse"
	tool2.ToolName = "Read"
	for i := 1; i <= selfHealMaxAttempts; i++ {
		s.resolveSubagentName(tool2, map[string]interface{}{})
		waitHealAttempts(t, s, "S|abound", i)
	}
	s.resolveSubagentName(tool2, map[string]interface{}{})
	time.Sleep(150 * time.Millisecond)
	if n := healAttempts(s, "S|abound"); n != selfHealMaxAttempts {
		t.Fatalf("healAttempts=%d after extra event, want capped at %d", n, selfHealMaxAttempts)
	}
}

func TestAgentIDFirst_ConcurrentLegacyClaimExactlyOneAdopts(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	sid := "S"
	if err := s.obsStore.UpsertRosterEntry(ctx, store.RosterEntry{
		RosterID: "r-legacy", SessionID: &sid, AgentName: "@old", Host: "testhost",
		Runtime: "claude_code", Relationship: "teammate", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"aP", "aQ"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s.dispatchObservability(startEvent("S", "old", id), map[string]interface{}{})
		}(id)
	}
	wg.Wait()
	waitForRosterEntry(t, s.obsStore, "S", "@old-2")
	a := waitForRosterEntry(t, s.obsStore, "S", "@old")
	b := waitForRosterEntry(t, s.obsStore, "S", "@old-2")
	if a.RosterID != "r-legacy" {
		t.Fatalf("adopter must keep the legacy roster_id, got %q", a.RosterID)
	}
	if a.AgentID == "" || b.AgentID == "" || a.AgentID == b.AgentID {
		t.Fatalf("agent_ids must be distinct and set: %q vs %q", a.AgentID, b.AgentID)
	}
	if n, err := s.obsStore.ResolveByAgentID(ctx, "S", a.AgentID); err != nil || n != "@old" {
		t.Fatalf("adopter agent_id resolves to %q err=%v", n, err)
	}
}

func TestAgentIDFirst_EarlyRegistrationConsumesFIFOAndSetsParent(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	s.dispatchObservability(startEvent("S", "startup-analyst", "aspawner"), map[string]interface{}{})
	spawner := waitForRosterEntry(t, s.obsStore, "S", "@startup-analyst")

	s.subagentNames.record("S", "Explore", "scout", "startup-analyst")
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "PreToolUse", SessionID: "S", AgentType: "Explore", AgentID: "aX", ToolName: "Read",
	}, map[string]interface{}{})
	x := waitForRosterEntry(t, s.obsStore, "S", "@Explore")
	if x.AgentID != "aX" {
		t.Fatalf("early path AgentID=%q", x.AgentID)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.regMu.Lock()
		_, ok := s.earlyRegistered["S|aX"]
		s.regMu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s.dispatchObservability(startEvent("S", "Explore", "aX"), map[string]interface{}{})
	s.subagentNames.record("S", "Explore", "scout2", "")
	s.dispatchObservability(startEvent("S", "Explore", "aY"), map[string]interface{}{})

	y := waitForRosterEntry(t, s.obsStore, "S", "@scout2")
	if y.AgentID != "aY" {
		t.Fatalf("@scout2 AgentID=%q, want aY", y.AgentID)
	}
	if _, err := s.obsStore.ResolveRosterID(ctx, "S", "@scout"); err == nil {
		t.Fatal("second spawn popped the first spawn's record (@scout registered)")
	}
	xx, err := s.obsStore.GetRosterEntry(ctx, x.RosterID)
	if err != nil {
		t.Fatal(err)
	}
	if xx.ParentRef == nil || *xx.ParentRef != spawner.RosterID || xx.Relationship != "subagent" {
		t.Fatalf("X parent_ref=%v rel=%q, want %q/subagent", xx.ParentRef, xx.Relationship, spawner.RosterID)
	}
}
