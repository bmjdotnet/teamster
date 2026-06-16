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

func TestFocusNudgeCache_ClearSession(t *testing.T) {
	var c focusNudgeCache
	c.setFocus("sess1", "@agent")

	c.clearSession("sess1")

	dbCheck := func() bool { return false }
	_, ok := c.check("sess1", "@agent", dbCheck)
	if !ok {
		t.Fatal("expected nudge after session clear")
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
