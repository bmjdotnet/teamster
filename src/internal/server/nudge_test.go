package server

import "testing"

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
