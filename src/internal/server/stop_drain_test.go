package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/hook"
	mcpwms "github.com/bmjdotnet/teamster/internal/mcp/wms"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// Stop is a TURN boundary, not a session boundary — Claude Code emits it at
// the end of every assistant turn and there is no SessionEnd hook. The
// handler used to mark every affected session closed and drain its open
// intervals; a lead's Stop carries no agent_type, so it took the
// session-wide branch and killed every teammate's interval too (teammates
// share the lead's session_id). Measured before removal: 279 intervals
// closed in 7 days, and only ~7% of a focused session's spend landed inside
// an interval. These tests pin the absence.

// spyDrainStore records the two writes the Stop handler must no longer make,
// plus the claim-path open.
type spyDrainStore struct {
	store.Store
	mu            sync.Mutex
	drainCalls    []string // "session|agent"
	closedUpserts []string // "session|agent" upserted with status=closed
	opens         []spyOpenCall
}

type spyOpenCall struct {
	key        store.SessionKey
	entityType string
	entityID   string
}

func (f *spyDrainStore) CloseSessionIntervals(_ context.Context, sessionID, agentName string, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainCalls = append(f.drainCalls, sessionID+"|"+agentName)
	return 0, nil
}

func (f *spyDrainStore) UpsertSession(_ context.Context, sess store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sess.Status == store.SessionStatusClosed {
		f.closedUpserts = append(f.closedUpserts, sess.SessionID+"|"+sess.AgentName)
	}
	return nil
}

func (f *spyDrainStore) OpenFocusInterval(_ context.Context, key store.SessionKey, entityType, entityID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, spyOpenCall{key: key, entityType: entityType, entityID: entityID})
	return nil
}

func (f *spyDrainStore) counts() (drains, closed, opens int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.drainCalls), len(f.closedUpserts), len(f.opens)
}

func (f *spyDrainStore) openAt(i int) spyOpenCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[i]
}

// settle gives any detached goroutine the handler might spawn time to run.
// These are negative assertions (proving a write never happens), so there is
// no positive signal to poll for — a fixed wait is the honest shape.
func settle() { time.Sleep(150 * time.Millisecond) }

func waitForOpens(t *testing.T, spy *spyDrainStore, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, n := spy.counts(); n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, got := spy.counts()
	t.Fatalf("OpenFocusInterval called %d time(s), want %d within deadline", got, want)
}

// A lead Stop (no agent_type) is the case that used to drain the whole
// session, teammates included. The tracker is seeded with a lead and two
// teammates so the pre-change code would have produced three drain calls.
func TestStop_DoesNotDrainIntervalsOrCloseSessions(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.sessions.Upsert("s-lead", "")
	s.sessions.Upsert("s-lead", "teammate-a")
	s.sessions.Upsert("s-lead", "teammate-b")

	s.dispatchObservability(hook.HookEvent{HookEventName: "Stop", SessionID: "s-lead"}, map[string]interface{}{
		"hook_event_name": "Stop",
		"session_id":      "s-lead",
		"ts":              time.Now().UTC().Format(time.RFC3339),
	})

	settle()
	drains, closed, _ := spy.counts()
	if drains != 0 {
		t.Errorf("CloseSessionIntervals called %d time(s) on Stop, want 0 (%v)", drains, spy.drainCalls)
	}
	if closed != 0 {
		t.Errorf("UpsertSession(status=closed) called %d time(s) on Stop, want 0 (%v)", closed, spy.closedUpserts)
	}
}

// A real subagent stop is agent-scoped and also must not write anything
// durable, but it MUST still do its in-memory per-turn bookkeeping.
func TestSubagentStop_NoDurableWritesButStillClearsTurnState(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.sessions.Upsert("s-team", "worker")
	s.turnStates.StartTurn("s-team", "@worker")
	s.wmsWarnings.queue("s-team", "@worker", "stale warning")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "SubagentStop", SessionID: "s-team", AgentType: "worker"},
		map[string]interface{}{
			"hook_event_name": "SubagentStop",
			"session_id":      "s-team",
			"agent_type":      "worker",
		})

	settle()
	drains, closed, _ := spy.counts()
	if drains != 0 || closed != 0 {
		t.Errorf("SubagentStop wrote durable state: %d drain(s), %d closed upsert(s)", drains, closed)
	}
	if got := s.wmsWarnings.consume("s-team", "@worker"); got != "" {
		t.Errorf("SubagentStop did not clear the agent's queued warnings, got %q", got)
	}
	if s.turnStates.IsProcessing("s-team", "@worker") {
		t.Error("SubagentStop did not end the agent's turn")
	}
}

// A phantom SubagentStop (no agent_type) must still be ignored outright.
func TestPhantomSubagentStop_StillIgnored(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.sessions.Upsert("s-phantom", "")
	s.wmsWarnings.queue("s-phantom", "", "must survive a phantom stop")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "SubagentStop", SessionID: "s-phantom"},
		map[string]interface{}{
			"hook_event_name": "SubagentStop",
			"session_id":      "s-phantom",
		})

	settle()
	if got := s.wmsWarnings.consume("s-phantom", ""); got == "" {
		t.Error("phantom SubagentStop cleared session state; it must be ignored entirely")
	}
}

// claimStatusChange is the WMSStatusChange a successful pending->active claim
// raises. wms_agent_name is empty because the stdio wms-mcp client sends no
// identity in _meta — that emptiness is the defect being recovered from.
func claimStatusChange(sessionID, entityID, agentName string) map[string]interface{} {
	return map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   entityID,
		"wms_old_status":  wms.StatusPending,
		"wms_new_status":  wms.StatusActive,
		"wms_session_id":  sessionID,
		"wms_agent_name":  agentName,
	}
}

// The teammate case: PreToolUse stashed "@worker"'s identity, the MCP call
// carried none, and the interval must open under the stashed name. Before
// this recovery the guard skipped the open entirely — 170 of 170 claims in
// the live hub's history recorded an empty agent, so it never once fired.
func TestClaimFocusInterval_RecoversStashedTeammateIdentity(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-1", "s-1", "worker")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-1", "wu-1", ""))

	waitForOpens(t, spy, 1)
	got := spy.openAt(0)
	if got.key.AgentName != "@worker" {
		t.Errorf("interval opened under agent %q, want %q", got.key.AgentName, "@worker")
	}
	if got.entityID != "wu-1" || got.entityType != wms.EntityWorkUnit {
		t.Errorf("opened %s/%s, want workunit/wu-1", got.entityType, got.entityID)
	}
}

// The lead case: hook payloads carry no agent_type for a lead, so the stash
// holds an empty string. That is a real answer, not a miss — the interval
// belongs to the lead and keys to an empty agent_name.
func TestClaimFocusInterval_StashedEmptyIdentityMeansLead(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-lead", "s-2", "")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-2", "wu-lead", ""))

	waitForOpens(t, spy, 1)
	if got := spy.openAt(0).key.AgentName; got != "" {
		t.Errorf("lead interval opened under agent %q, want empty", got)
	}
}

// wms_updateStatus is the path where recovery is load-bearing and was
// originally missed. Its PreToolUse case only stashes (server.go:913-916) —
// unlike wms_updateWorkUnitStatus, nothing opens the interval for it — yet
// with entityType "workunit" it runs the same UpdateWorkUnitStatus and
// OnStatusChange as a claim, with the same empty _meta agent on stdio. Before
// the fix this fell through to the "could not identify" nudge while the
// identity sat one stash key away.
func TestClaimFocusInterval_RecoversFromUpdateStatusStash(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.stashMCPIdentity(mcpwms.ToolUpdateStatus, "wu-updstatus", "s-3", "worker")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-3", "wu-updstatus", ""))

	waitForOpens(t, spy, 1)
	if got := spy.openAt(0).key.AgentName; got != "@worker" {
		t.Errorf("interval opened under agent %q, want %q", got, "@worker")
	}
	if got := spy.openAt(0).entityID; got != "wu-updstatus" {
		t.Errorf("opened entity %q, want %q", got, "wu-updstatus")
	}
}

// wms_createWorkUnit(status:"active") is the fourth route to a workunit
// pending->active, found in review one tool after the updateStatus MAJOR.
// Belt-and-braces like updateWorkUnitStatus — its own PreToolUse already
// opened the interval — so this pins set completeness, not attribution.
func TestClaimFocusInterval_RecoversFromCreateWorkUnitStash(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.stashMCPIdentity(mcpwms.ToolCreateWorkUnit, "wu-created", "s-7", "worker")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-7", "wu-created", ""))

	waitForOpens(t, spy, 1)
	if got := spy.openAt(0).key.AgentName; got != "@worker" {
		t.Errorf("interval opened under agent %q, want %q", got, "@worker")
	}
}

// wms_updateWorkUnitStatus is belt-and-braces: its own PreToolUse already
// opened the interval, so recovery here is redundant by design. Kept so the
// branch does not silently depend on that ordering holding.
func TestClaimFocusInterval_RecoversFromUpdateWorkUnitStatusStash(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)
	s.stashMCPIdentity(mcpwms.ToolUpdateWorkUnitStatus, "wu-upd", "s-6", "worker")

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-6", "wu-upd", ""))

	waitForOpens(t, spy, 1)
	if got := spy.openAt(0).key.AgentName; got != "@worker" {
		t.Errorf("interval opened under agent %q, want %q", got, "@worker")
	}
}

// No stash and no _meta identity: decline to guess, and SAY SO. The warning
// used to live only in the error branch, which an unidentified claim never
// reached — that silence is why the defect survived 170 claims.
func TestClaimFocusInterval_WarnsWhenIdentityUnknown(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-4", "wu-orphan", ""))

	settle()
	if _, _, opens := spy.counts(); opens != 0 {
		t.Errorf("opened %d interval(s) with no identity, want 0", opens)
	}
	warn := s.wmsWarnings.consume("s-4", "")
	if warn == "" {
		t.Fatal("no warning queued for an unidentified claim; silence is the defect")
	}
	if !strings.Contains(warn, "wu-orphan") || !strings.Contains(warn, "wms_setFocus") {
		t.Errorf("warning does not name the WU and the remedy: %q", warn)
	}
}

// An explicit _meta identity still wins without consulting the stash, so a
// runtime that does send one is unaffected.
func TestClaimFocusInterval_ExplicitIdentityStillUsed(t *testing.T) {
	spy := &spyDrainStore{}
	s := newIntervalCloseTestServer(t, spy)

	s.dispatchObservability(
		hook.HookEvent{HookEventName: "WMSStatusChange"},
		claimStatusChange("s-5", "wu-explicit", "codexer"))

	waitForOpens(t, spy, 1)
	if got := spy.openAt(0).key.AgentName; got != "@codexer" {
		t.Errorf("interval opened under agent %q, want %q", got, "@codexer")
	}
}

func TestPeekMCPIdentity(t *testing.T) {
	// Named for what it proves: repeated peeks keep returning the entry, so
	// nothing was removed. It does NOT drive injectMCPIdentity — that the HTTP
	// path still gets its FIFO entry follows from non-removal, and is visible
	// by inspection (this function contains no delete).
	t.Run("repeated peeks keep returning the entry", func(t *testing.T) {
		s := &Server{}
		s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-a", "s", "worker")
		for i := range 3 {
			got, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-a", "s")
			if !ok || got != "worker" {
				t.Fatalf("peek %d = (%q, %v), want (worker, true)", i, got, ok)
			}
		}
	})

	t.Run("misses on a different session", func(t *testing.T) {
		s := &Server{}
		s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-b", "s-other", "worker")
		if _, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-b", "s"); ok {
			t.Error("peek matched an entry from a different session")
		}
	})

	t.Run("misses when nothing was stashed", func(t *testing.T) {
		s := &Server{}
		if _, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-none", "s"); ok {
			t.Error("peek reported a hit with an empty stash")
		}
	})

	// Candidates that DISAGREE are a miss: they share the lead's session_id so
	// sessionID cannot separate them, and FIFO order is arrival order rather
	// than commit order, so choosing would risk the claim loser.
	t.Run("declines when candidates disagree", func(t *testing.T) {
		s := &Server{}
		s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-race", "s", "worker-a")
		s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-race", "s", "worker-b")
		if got, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-race", "s"); ok {
			t.Errorf("peek guessed %q between two differing candidates, want a miss", got)
		}
	})

	// Multiplicity alone is NOT ambiguity. Agreeing candidates resolve to one
	// key, so there is no wrong answer to pick — declining here would produce a
	// false warning on a retried claim. Note this also admits two different
	// same-type teammates, since the stash holds the raw agentType they share;
	// that is safe precisely because agentNameFor maps both to "@Explore".
	t.Run("returns the answer when candidates agree", func(t *testing.T) {
		s := &Server{}
		s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-retry", "s", "Explore")
		s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-retry", "s", "Explore")
		got, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-retry", "s")
		if !ok {
			t.Fatal("peek declined two agreeing candidates; multiplicity is not ambiguity")
		}
		if got != "Explore" {
			t.Errorf("peek = %q, want %q", got, "Explore")
		}
	})

	// Agreement is judged only over LIVE candidates: an expired entry that
	// disagrees must not veto a live one.
	t.Run("expired disagreeing entry does not veto a live one", func(t *testing.T) {
		s := &Server{}
		s.pendingMCPIdent = map[string][]mcpIdentity{
			mcpwms.ToolClaimWorkUnit + ":wu-mixed": {
				{SessionID: "s", AgentType: "stale", ExpiresAt: time.Now().Add(-time.Second)},
				{SessionID: "s", AgentType: "live", ExpiresAt: time.Now().Add(time.Minute)},
			},
		}
		got, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-mixed", "s")
		if !ok || got != "live" {
			t.Errorf("peek = (%q, %v), want (live, true)", got, ok)
		}
	})

	t.Run("misses once expired", func(t *testing.T) {
		s := &Server{}
		s.pendingMCPIdent = map[string][]mcpIdentity{
			mcpwms.ToolClaimWorkUnit + ":wu-old": {{
				SessionID: "s",
				AgentType: "worker",
				ExpiresAt: time.Now().Add(-time.Second),
			}},
		}
		if _, ok := s.peekMCPIdentity(mcpwms.ToolClaimWorkUnit, "wu-old", "s"); ok {
			t.Error("peek returned an expired entry")
		}
	})
}
