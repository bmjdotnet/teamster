package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// MarkStaleSessionsClosed became load-bearing when the per-turn Stop-time
// close was removed: it is now the only writer of SessionStatusClosed, and
// the roster's "closed" liveness tier is derived from sessions.status alone.
// It is also half of an ORDERED pair with CloseIntervalsForStaleSessions —
// intervals first, then sessions — because that method skips sessions already
// marked closed.

func openStaleSession(ctx context.Context, t *testing.T, s store.Store, sessionID string, age time.Duration) store.SessionKey {
	t.Helper()
	key := store.SessionKey{SessionID: sessionID}
	seen := time.Now().UTC().Add(-age)
	if err := s.UpsertSession(ctx, store.Session{
		SessionID: sessionID,
		Host:      "h",
		FirstSeen: seen,
		LastSeen:  seen,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, "wu-"+sessionID); err != nil {
		t.Fatal(err)
	}
	return key
}

func sessionStatus(ctx context.Context, t *testing.T, s store.Store, key store.SessionKey) store.SessionStatus {
	t.Helper()
	sess, err := s.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", key.SessionID, err)
	}
	return sess.Status
}

// openStaleSessionAgent is openStaleSession's teammate-shaped variant: same
// setup, but with a caller-supplied AgentName instead of "" (lead-shaped).
// The *ExceptLiveLead tests below need a lead row and a named row sharing
// one session_id (the Linux in-process shape) as well as named rows
// with no lead row anywhere in their session (the macOS remote shape).
func openStaleSessionAgent(ctx context.Context, t *testing.T, s store.Store, sessionID, agentName string, age time.Duration) store.SessionKey {
	t.Helper()
	key := store.SessionKey{SessionID: sessionID, AgentName: agentName}
	seen := time.Now().UTC().Add(-age)
	if err := s.UpsertSession(ctx, store.Session{
		SessionID: sessionID,
		AgentName: agentName,
		Host:      "h",
		FirstSeen: seen,
		LastSeen:  seen,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, "wu-"+sessionID+agentName); err != nil {
		t.Fatal(err)
	}
	return key
}

// focusIsOpen answers "is (session, agent)'s focus open right now" via the
// same store method the nudge uses post-wh2-idle-teammate-exemption.
func focusIsOpen(ctx context.Context, t *testing.T, s store.Store, key store.SessionKey) bool {
	t.Helper()
	has, err := s.HasAnyFocusInterval(ctx, key)
	if err != nil {
		t.Fatalf("HasAnyFocusInterval(%s/%s): %v", key.SessionID, key.AgentName, err)
	}
	return has
}

// TestCloseIntervalsForStaleSessionsExceptLiveLead exercises the
// wh2-idle-teammate-exemption predicate: a named-agent row is skipped only
// when its lead row (same session_id, agent_name = "") exists and is itself
// not stale. These are the populations RULING 2's own live-data query
// distinguished (VERIFY-AUDITOR.md §A3).
func TestCloseIntervalsForStaleSessionsExceptLiveLead(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		threshold := time.Now().UTC().Add(-2 * time.Hour)

		t.Run("teammate exempted when its lead is live", func(t *testing.T) {
			lead := openStaleSessionAgent(ctx, t, s, "S-exempt-live", "", 1*time.Hour)      // fresh
			mate := openStaleSessionAgent(ctx, t, s, "S-exempt-live", "@mate", 3*time.Hour) // stale

			n, err := s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("closed %d interval(s), want 0 (lead is live, teammate must be exempted)", n)
			}
			if !focusIsOpen(ctx, t, s, mate) {
				t.Error("teammate's interval was closed despite a live lead in the same session")
			}
			if !focusIsOpen(ctx, t, s, lead) {
				t.Error("lead's own (fresh) interval was closed unexpectedly")
			}
		})

		t.Run("teammate closes normally when its lead is also stale", func(t *testing.T) {
			lead := openStaleSessionAgent(ctx, t, s, "S-exempt-dead-lead", "", 3*time.Hour)
			mate := openStaleSessionAgent(ctx, t, s, "S-exempt-dead-lead", "@mate", 3*time.Hour)

			n, err := s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("closed %d interval(s), want 2 (lead is stale too, no exemption applies)", n)
			}
			if focusIsOpen(ctx, t, s, mate) {
				t.Error("teammate's interval survived a stale lead")
			}
			if focusIsOpen(ctx, t, s, lead) {
				t.Error("lead's own stale interval survived")
			}
		})

		t.Run("teammate closes normally with no lead row at all", func(t *testing.T) {
			mate := openStaleSessionAgent(ctx, t, s, "S-exempt-no-lead", "@solo", 3*time.Hour)

			n, err := s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("closed %d interval(s), want 1", n)
			}
			if focusIsOpen(ctx, t, s, mate) {
				t.Error("interval survived with no lead row to exempt it")
			}
		})

		t.Run("macOS shape: named rows on distinct sessions are never exempted by an unrelated lead-shaped row", func(t *testing.T) {
			// studio's actual live shape (VERIFY-AUDITOR.md §A3): three
			// named teammate rows, each its own session_id, plus one
			// agent_name='' row on a FOURTH, unrelated session_id. The
			// exemption must not leak across session_id.
			a := openStaleSessionAgent(ctx, t, s, "S-macos-a", "@scrollz", 3*time.Hour)
			b := openStaleSessionAgent(ctx, t, s, "S-macos-b", "@workspacemisc", 3*time.Hour)
			c := openStaleSessionAgent(ctx, t, s, "S-macos-c", "@teamstercore", 3*time.Hour)
			openStaleSessionAgent(ctx, t, s, "S-macos-lead-elsewhere", "", 0) // fresh, unrelated session

			n, err := s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 3 {
				t.Fatalf("closed %d interval(s), want 3 (all three named rows, none exempted)", n)
			}
			for _, k := range []store.SessionKey{a, b, c} {
				if focusIsOpen(ctx, t, s, k) {
					t.Errorf("%s/%s exempted by a fresh lead-shaped row in an unrelated session", k.SessionID, k.AgentName)
				}
			}
		})

		t.Run("a lone lead row is never exempted by the join's own agent_name<>'' guard", func(t *testing.T) {
			lead := openStaleSessionAgent(ctx, t, s, "S-exempt-lead-alone", "", 3*time.Hour)

			n, err := s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("closed %d interval(s), want 1 (a lead row is never itself exemptable)", n)
			}
			if focusIsOpen(ctx, t, s, lead) {
				t.Error("lone lead row was exempted; the join must only ever protect agent_name<>'' rows")
			}
		})
	})
}

// TestMarkStaleSessionsClosedExceptLiveLead mirrors
// TestCloseIntervalsForStaleSessionsExceptLiveLead for session-status
// marking: same predicate, same populations, different table.
func TestMarkStaleSessionsClosedExceptLiveLead(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		threshold := time.Now().UTC().Add(-2 * time.Hour)

		t.Run("teammate exempted when its lead is live", func(t *testing.T) {
			lead := openStaleSessionAgent(ctx, t, s, "S-mark-exempt-live", "", 1*time.Hour)
			mate := openStaleSessionAgent(ctx, t, s, "S-mark-exempt-live", "@mate", 3*time.Hour)

			n, err := s.MarkStaleSessionsClosedExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("marked %d session(s) closed, want 0", n)
			}
			if got := sessionStatus(ctx, t, s, mate); got == store.SessionStatusClosed {
				t.Error("teammate's session was marked closed despite a live lead in the same session")
			}
			if got := sessionStatus(ctx, t, s, lead); got == store.SessionStatusClosed {
				t.Error("lead's own (fresh) session was marked closed unexpectedly")
			}
		})

		t.Run("teammate marked closed normally when its lead is also stale", func(t *testing.T) {
			lead := openStaleSessionAgent(ctx, t, s, "S-mark-exempt-dead-lead", "", 3*time.Hour)
			mate := openStaleSessionAgent(ctx, t, s, "S-mark-exempt-dead-lead", "@mate", 3*time.Hour)

			n, err := s.MarkStaleSessionsClosedExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("marked %d session(s) closed, want 2", n)
			}
			if got := sessionStatus(ctx, t, s, mate); got != store.SessionStatusClosed {
				t.Errorf("teammate session status = %q, want closed", got)
			}
			if got := sessionStatus(ctx, t, s, lead); got != store.SessionStatusClosed {
				t.Errorf("lead session status = %q, want closed", got)
			}
		})

		t.Run("teammate marked closed normally with no lead row at all", func(t *testing.T) {
			mate := openStaleSessionAgent(ctx, t, s, "S-mark-exempt-no-lead", "@solo", 3*time.Hour)

			n, err := s.MarkStaleSessionsClosedExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("marked %d session(s) closed, want 1", n)
			}
			if got := sessionStatus(ctx, t, s, mate); got != store.SessionStatusClosed {
				t.Errorf("session status = %q, want closed", got)
			}
		})

		t.Run("macOS shape: distinct sessions are never exempted by an unrelated lead-shaped row", func(t *testing.T) {
			a := openStaleSessionAgent(ctx, t, s, "S-mark-macos-a", "@scrollz", 3*time.Hour)
			b := openStaleSessionAgent(ctx, t, s, "S-mark-macos-b", "@workspacemisc", 3*time.Hour)
			c := openStaleSessionAgent(ctx, t, s, "S-mark-macos-c", "@teamstercore", 3*time.Hour)
			openStaleSessionAgent(ctx, t, s, "S-mark-macos-lead-elsewhere", "", 0)

			n, err := s.MarkStaleSessionsClosedExceptLiveLead(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if n != 3 {
				t.Fatalf("marked %d session(s) closed, want 3", n)
			}
			for _, k := range []store.SessionKey{a, b, c} {
				if got := sessionStatus(ctx, t, s, k); got != store.SessionStatusClosed {
					t.Errorf("%s/%s status = %q, want closed", k.SessionID, k.AgentName, got)
				}
			}
		})
	})
}

// TestStaleSessionClose_ExceptLiveLead_SurvivesNextSweep proves the reason
// MarkStaleSessionsClosedExceptLiveLead has to exist at all, not just
// CloseIntervalsForStaleSessionsExceptLiveLead: if phase 3 exempted a
// teammate's INTERVAL but marked its SESSION closed anyway (the plain
// MarkStaleSessionsClosed instead of the exempt variant), the very next
// reaper sweep's phase 2 (CloseIntervalsForClosedSessions — status='closed'
// only, no staleness check of its own) would close that same interval,
// silently defeating the exemption one reaper interval later. Both halves
// of phase 3 must use the exempt variant together.
func TestStaleSessionClose_ExceptLiveLead_SurvivesNextSweep(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		threshold := time.Now().UTC().Add(-2 * time.Hour)

		lead := openStaleSessionAgent(ctx, t, s, "S-survive", "", 1*time.Hour)      // fresh
		mate := openStaleSessionAgent(ctx, t, s, "S-survive", "@mate", 3*time.Hour) // stale

		// Phase 3, the reaper's actual order: intervals first, then sessions.
		if _, err := s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkStaleSessionsClosedExceptLiveLead(ctx, threshold); err != nil {
			t.Fatal(err)
		}
		if !focusIsOpen(ctx, t, s, mate) {
			t.Fatal("teammate's interval closed by phase 3 despite the exemption")
		}
		if got := sessionStatus(ctx, t, s, mate); got == store.SessionStatusClosed {
			t.Fatal("teammate's session marked closed by phase 3 despite the exemption")
		}

		// Simulate the NEXT reaper tick's phase 2, which has no staleness
		// predicate of its own: if phase 3 had marked the session closed
		// anyway, this would now close the exempted interval too.
		if _, err := s.CloseIntervalsForClosedSessions(ctx); err != nil {
			t.Fatal(err)
		}
		if !focusIsOpen(ctx, t, s, mate) {
			t.Fatal("a later sweep's phase 2 closed the exempted interval — " +
				"the exemption was defeated because the session got marked closed")
		}
		if !focusIsOpen(ctx, t, s, lead) {
			t.Error("lead's own interval was unexpectedly closed by phase 2")
		}
	})
}

func TestMarkStaleSessionsClosed(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// 2h is the shipped GCStaleHours default; 3h is comfortably past it
		// and 1h comfortably short of it.
		threshold := time.Now().UTC().Add(-2 * time.Hour)

		stale := openStaleSession(ctx, t, s, "S-mark-stale", 3*time.Hour)
		fresh := openStaleSession(ctx, t, s, "S-mark-fresh", 1*time.Hour)

		n, err := s.MarkStaleSessionsClosed(ctx, threshold)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("marked %d session(s) closed, want 1 (the fresh one must be untouched)", n)
		}
		if got := sessionStatus(ctx, t, s, stale); got != store.SessionStatusClosed {
			t.Errorf("stale session status = %q, want %q", got, store.SessionStatusClosed)
		}
		if got := sessionStatus(ctx, t, s, fresh); got == store.SessionStatusClosed {
			t.Error("fresh session was marked closed; the threshold is not being applied")
		}

		// Idempotent: a second pass must find nothing left to do, so an
		// every-15-minutes reaper does not keep rewriting the same rows.
		n2, err := s.MarkStaleSessionsClosed(ctx, threshold)
		if err != nil {
			t.Fatal(err)
		}
		if n2 != 0 {
			t.Errorf("second pass marked %d session(s), want 0", n2)
		}
	})
}

// The ordering contract, stated as a test: closing the intervals must happen
// first. Run the other way round, CloseIntervalsForStaleSessions' own
// "status <> 'closed'" predicate excludes the very sessions just marked, and
// their intervals are stranded open.
func TestStaleSessionClose_OrderingContract(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		threshold := time.Now().UTC().Add(-2 * time.Hour)

		t.Run("intervals first, then sessions (the reaper's order)", func(t *testing.T) {
			openStaleSession(ctx, t, s, "S-order-right", 3*time.Hour)

			nI, err := s.CloseIntervalsForStaleSessions(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if nI != 1 {
				t.Fatalf("closed %d interval(s), want 1", nI)
			}
			nS, err := s.MarkStaleSessionsClosed(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if nS < 1 {
				t.Fatalf("marked %d session(s) closed, want at least 1", nS)
			}
		})

		t.Run("sessions first strands the intervals", func(t *testing.T) {
			openStaleSession(ctx, t, s, "S-order-wrong", 3*time.Hour)

			if _, err := s.MarkStaleSessionsClosed(ctx, threshold); err != nil {
				t.Fatal(err)
			}
			nI, err := s.CloseIntervalsForStaleSessions(ctx, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if nI != 0 {
				t.Fatalf("closed %d interval(s) after marking sessions first, want 0 — "+
					"if this now closes them the predicate changed and the reaper's "+
					"ordering comment is stale", nI)
			}
		})
	})
}
