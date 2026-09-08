package server

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func TestFocusNudgeCache_NoFocus_Nudges(t *testing.T) {
	var c focusNudgeCache
	dbCalled := 0
	dbCheck := func() bool { dbCalled++; return false }

	msg, ok := c.check("sess1", "@agent", dbCheck)
	if !ok {
		t.Fatal("expected nudge on first check")
	}
	if msg != nudgeText {
		t.Fatalf("unexpected nudge text: %s", msg)
	}
	if dbCalled != 1 {
		t.Fatalf("expected 1 DB call on cache miss, got %d", dbCalled)
	}
}

func TestFocusNudgeCache_HasFocus_NoNudge(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return true }

	msg, ok := c.check("sess1", "@agent", dbCheck)
	if ok {
		t.Fatalf("should not nudge when DB says focus exists, got: %s", msg)
	}
}

func TestFocusNudgeCache_SetFocus_StopsNudge(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	_, ok := c.check("sess1", "@agent", dbCheck)
	if !ok {
		t.Fatal("expected first nudge")
	}

	c.setFocus("sess1", "@agent")

	_, ok = c.check("sess1", "@agent", dbCheck)
	if ok {
		t.Fatal("should not nudge after setFocus")
	}
}

func TestFocusNudgeCache_StopsAfterMaxNudges(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	for i := 0; i < nudgeMaxCount; i++ {
		_, ok := c.check("sess1", "@agent", dbCheck)
		if !ok {
			t.Fatalf("expected nudge on attempt %d", i+1)
		}
	}

	_, ok := c.check("sess1", "@agent", dbCheck)
	if ok {
		t.Fatal("should stop nudging after max count")
	}
}

func TestFocusNudgeCache_CachedHit_NoDB(t *testing.T) {
	var c focusNudgeCache
	dbCalled := 0
	dbCheck := func() bool { dbCalled++; return false }

	c.check("sess1", "@agent", dbCheck)
	if dbCalled != 1 {
		t.Fatalf("expected 1 DB call, got %d", dbCalled)
	}

	c.check("sess1", "@agent", dbCheck)
	if dbCalled != 1 {
		t.Fatalf("expected no additional DB calls, got %d", dbCalled)
	}
}

func TestFocusNudgeCache_ClearSession_PreservesHasFocus(t *testing.T) {
	var c focusNudgeCache
	c.setFocus("sess1", "@agent")

	c.clearSession("sess1")

	// hasFocus must survive the turn boundary — no nudge after setFocus even
	// after clearSession is called by the Stop handler.
	dbCheck := func() bool { return false }
	_, ok := c.check("sess1", "@agent", dbCheck)
	if ok {
		t.Fatal("should not nudge after clearSession when focus was set")
	}
}

func TestFocusNudgeCache_ClearSession_ResetsNudgeCount(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	// Exhaust the nudge budget for this turn.
	for i := 0; i < nudgeMaxCount; i++ {
		c.check("sess1", "@agent", dbCheck)
	}
	_, ok := c.check("sess1", "@agent", dbCheck)
	if ok {
		t.Fatal("nudge should be suppressed after max count")
	}

	// After a turn boundary (clearSession), the per-turn budget resets and
	// the agent should be nudgeable again (still no focus).
	c.clearSession("sess1")
	_, ok = c.check("sess1", "@agent", dbCheck)
	if !ok {
		t.Fatal("expected nudge after clearSession resets count on unfocused agent")
	}
}

func TestFocusNudgeCache_ClearAgentTurn_LeavesOtherAgents(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	// Exhaust the nudge budget for both agents this turn.
	for i := 0; i < nudgeMaxCount; i++ {
		c.check("sess1", "@agent", dbCheck)
		c.check("sess1", "@other", dbCheck)
	}

	// A teammate's Stop clears only its own nudge budget.
	c.clearAgentTurn("sess1", "@agent")

	if _, ok := c.check("sess1", "@agent", dbCheck); !ok {
		t.Fatal("expected nudge after clearAgentTurn resets this agent's count")
	}
	if _, ok := c.check("sess1", "@other", dbCheck); ok {
		t.Fatal("other agent's nudge budget should still be exhausted")
	}
}

func TestFocusNudgeCache_AgentKeyMismatch_StillNudged(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	// setFocus arrives with empty AgentType (resolves to "").
	c.setFocus("sess1", "")

	// A named agent in the same session has a distinct per-agent key and is
	// nudged independently — the empty-agent setFocus does not suppress it.
	_, ok := c.check("sess1", "@hookd", dbCheck)
	if !ok {
		t.Fatal("@hookd should be nudged: its key is independent from the empty-agent key")
	}
}

func TestFocusNudgeCache_AgentKeyMismatch_NamedFirst(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	// setFocus arrives with @hookd.
	c.setFocus("sess1", "@hookd")

	// Empty-agent check has a distinct key ("") — not suppressed by @hookd's focus.
	_, ok := c.check("sess1", "", dbCheck)
	if !ok {
		t.Fatal("empty-agent should be nudged: its key is independent from @hookd's key")
	}
}

func TestFocusNudgeCache_SessionLevelSuppression(t *testing.T) {
	var c focusNudgeCache
	dbCheck := func() bool { return false }

	// hookd sets focus; store does not.
	c.setFocus("sess1", "@hookd")

	// @store shares the session but has its own key — it must still be nudged.
	_, ok := c.check("sess1", "@store", dbCheck)
	if !ok {
		t.Fatal("@store should be nudged: @hookd's focus does not suppress a sibling agent")
	}
}

func TestFocusNudgeCache_IndependentSessions(t *testing.T) {
	var c focusNudgeCache
	c.setFocus("sess1", "@agent")

	dbCheck := func() bool { return false }
	_, ok := c.check("sess2", "@agent", dbCheck)
	if !ok {
		t.Fatal("sess2 should still get nudged when sess1 has focus")
	}
}

// wh2-idle-teammate-exemption: HasAnyFocusInterval alone cannot reach the
// nudge in a live hookd — hasFocus, once true, is never re-checked by check
// (the cache-HIT branch returns before calling dbCheck at all). invalidateAll
// is what forces the re-check; these two tests pin its two outcomes.

// TestFocusNudgeCache_InvalidateAll_StillFocused_NoNudge is case (a): the
// agent's interval is still open when the reaper ticks (a different agent's
// staleness triggered the invalidation). dbCheck's call COUNT is the point —
// a test that only checked "no nudge" would pass vacuously even if
// invalidateAll silently did nothing, since a never-invalidated hasFocus=true
// entry also produces "no nudge" without ever calling dbCheck.
func TestFocusNudgeCache_InvalidateAll_StillFocused_NoNudge(t *testing.T) {
	var c focusNudgeCache
	c.setFocus("sess1", "@agent")

	c.invalidateAll()

	dbCalled := 0
	dbCheck := func() bool { dbCalled++; return true } // interval still open
	_, ok := c.check("sess1", "@agent", dbCheck)
	if ok {
		t.Fatal("still-focused agent should not be nudged after invalidation")
	}
	if dbCalled != 1 {
		t.Fatalf("expected exactly 1 DB call (a real cache miss) after invalidateAll, got %d", dbCalled)
	}
}

// TestFocusNudgeCache_InvalidateAll_ReapedInterval_Nudges is case (b): the
// agent's own interval was the one the reaper closed for staleness.
func TestFocusNudgeCache_InvalidateAll_ReapedInterval_Nudges(t *testing.T) {
	var c focusNudgeCache
	c.setFocus("sess1", "@agent")

	c.invalidateAll()

	dbCheck := func() bool { return false } // reaper closed this agent's interval
	_, ok := c.check("sess1", "@agent", dbCheck)
	if !ok {
		t.Fatal("expected a nudge for the agent whose interval the reaper just closed")
	}
}

// TestFocusNudgeCache_InvalidateAll_ClearsEverySession proves invalidateAll
// is hub-wide, not scoped to the session that went stale — the whole point
// is that ONE reaper tick can't cheaply know which OTHER cached entries are
// still good, so it drops all of them and lets each repopulate on its own
// next tool call.
func TestFocusNudgeCache_InvalidateAll_ClearsEverySession(t *testing.T) {
	var c focusNudgeCache
	c.setFocus("sess1", "@agent")
	c.setFocus("sess2", "@other")

	c.invalidateAll()

	dbCalled := 0
	dbCheck := func() bool { dbCalled++; return true }
	c.check("sess1", "@agent", dbCheck)
	c.check("sess2", "@other", dbCheck)
	if dbCalled != 2 {
		t.Fatalf("expected both sessions' entries invalidated (2 DB calls), got %d", dbCalled)
	}
}

// fakeReaperStore lets a test control exactly what each reaper phase reports
// closing without implementing store.Store's full surface — the same
// partial-fake shape as spyDrainStore in stop_drain_test.go (embed the
// interface, override only what the test drives).
type fakeReaperStore struct {
	store.Store
	staleIntervalsClosed int64
	sessionsMarked       int64
}

func (f *fakeReaperStore) CloseIntervalsOnTerminalEntities(context.Context) (int64, error) {
	return 0, nil
}

func (f *fakeReaperStore) CloseIntervalsForClosedSessions(context.Context) (int64, error) {
	return 0, nil
}

func (f *fakeReaperStore) CloseIntervalsForStaleSessionsExceptLiveLead(context.Context, time.Time) (int64, error) {
	return f.staleIntervalsClosed, nil
}

func (f *fakeReaperStore) MarkStaleSessionsClosedExceptLiveLead(context.Context, time.Time) (int64, error) {
	return f.sessionsMarked, nil
}

// TestReaperSweep_InvalidatesNudgeCacheOnlyWhenPhase3ClosedSomething is case
// (c): the reaper-integration half. A sweep that closes zero stale intervals
// must leave the cache untouched — a focused agent's next check takes the
// cache-HIT path (no DB call). A sweep that closes at least one must
// invalidate, forcing a real re-check.
func TestReaperSweep_InvalidatesNudgeCacheOnlyWhenPhase3ClosedSomething(t *testing.T) {
	t.Run("phase 3 closes nothing: cache untouched", func(t *testing.T) {
		var nudge focusNudgeCache
		nudge.setFocus("sess1", "@mate")

		r := &reaper{
			store:        &fakeReaperStore{staleIntervalsClosed: 0, sessionsMarked: 0},
			gcStaleHours: 2,
			nudgeCache:   &nudge,
		}
		r.sweep()

		dbCalled := 0
		dbCheck := func() bool { dbCalled++; return true }
		_, ok := nudge.check("sess1", "@mate", dbCheck)
		if ok {
			t.Fatal("focused agent should not be nudged")
		}
		if dbCalled != 0 {
			t.Fatalf("expected the cache-hit path (0 DB calls) when phase 3 closed nothing, got %d", dbCalled)
		}
	})

	t.Run("phase 3 closes an interval: cache invalidated", func(t *testing.T) {
		var nudge focusNudgeCache
		nudge.setFocus("sess1", "@mate")

		r := &reaper{
			store:        &fakeReaperStore{staleIntervalsClosed: 1, sessionsMarked: 1},
			gcStaleHours: 2,
			nudgeCache:   &nudge,
		}
		r.sweep()

		dbCalled := 0
		dbCheck := func() bool { dbCalled++; return true } // this agent's own interval is still open
		_, ok := nudge.check("sess1", "@mate", dbCheck)
		if ok {
			t.Fatal("agent with an open interval should not be nudged")
		}
		if dbCalled != 1 {
			t.Fatalf("expected a real DB re-check (1 call) after phase 3 closed something, got %d", dbCalled)
		}
	})

	t.Run("gcStaleHours == 0: phase 3 disabled, cache untouched", func(t *testing.T) {
		var nudge focusNudgeCache
		nudge.setFocus("sess1", "@mate")

		r := &reaper{
			store:        &fakeReaperStore{staleIntervalsClosed: 1, sessionsMarked: 1}, // would close if run
			gcStaleHours: 0,
			nudgeCache:   &nudge,
		}
		r.sweep()

		dbCalled := 0
		dbCheck := func() bool { dbCalled++; return true }
		_, ok := nudge.check("sess1", "@mate", dbCheck)
		if ok {
			t.Fatal("focused agent should not be nudged")
		}
		if dbCalled != 0 {
			t.Fatalf("expected the cache-hit path (0 DB calls) when phase 3 is disabled, got %d", dbCalled)
		}
	})
}
