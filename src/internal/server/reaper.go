package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// reaper periodically closes orphaned wms_intervals in three phases:
//
//  1. Terminal entities — intervals on done outcomes/workunits.
//  2. Closed sessions — intervals on sessions marked closed.
//  3. Stale sessions — intervals on sessions with no activity past a
//     configurable threshold, then the session rows themselves (disabled
//     when gcStaleHours == 0).
//
// CONTRACT, deliberately widened: the reaper closes INTERVALS and, in
// phase 3 only, the SESSION ROWS those intervals belong to. It still never
// touches WMS ENTITIES — outcomes and workunits — which is what the old
// "closes INTERVALS only, never entities" wording was protecting. A session
// is transport bookkeeping (who was connected, and when), not a unit of
// work; the reaper deciding a session went quiet says nothing about whether
// its outcome or workunit is finished, and must not.
//
// Phase 3 became the primary terminator when the per-turn Stop-time close
// was removed: Stop is a turn boundary, not a session boundary, so nothing
// durable is written from there any more. That close was also the only
// writer of sessions.status='closed', so phase 3 inherits it — see
// MarkStaleSessionsClosed.
type reaper struct {
	store        store.Store
	interval     time.Duration
	gcStaleHours int
	stopCh       <-chan struct{}

	// nudgeCache is invalidated when phase 3 actually closes a stale
	// interval, so the nudge re-derives focus from the DB instead of
	// trusting a cached hasFocus=true forever (wh2-idle-teammate-exemption;
	// see focusNudgeCache.invalidateAll's doc in nudge.go). nil in a test
	// that constructs a reaper directly without one is fine — sweep guards
	// the call.
	nudgeCache *focusNudgeCache
}

func (s *Server) startReaper() {
	if s.obsStore == nil {
		return
	}
	r := &reaper{
		store:        s.obsStore,
		interval:     s.cfg.ReaperInterval,
		gcStaleHours: s.cfg.GCStaleHours,
		stopCh:       s.sweepStop,
		nudgeCache:   &s.focusNudge,
	}
	go r.run()
}

func (r *reaper) run() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.sweep()
		}
	}
}

// sweep runs one reaper pass. Every phase is idempotent (the ended_at IS NULL
// and status <> 'closed' guards mean already-handled rows are skipped, 0 rows
// affected), so overlapping with rollup --sweep's copy of the same phases is
// benign.
func (r *reaper) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Phase 1: close intervals on terminal entities.
	n1, err := r.store.CloseIntervalsOnTerminalEntities(ctx)
	if err != nil {
		slog.Warn("reaper phase 1 failed", "error", err)
	} else if n1 > 0 {
		slog.Info("reaper: closed intervals on terminal entities", "closed", n1)
	}

	// Phase 2: close intervals for closed sessions.
	n2, err := r.store.CloseIntervalsForClosedSessions(ctx)
	if err != nil {
		slog.Warn("reaper phase 2 failed", "error", err)
	} else if n2 > 0 {
		slog.Info("reaper: closed intervals for closed sessions", "closed", n2)
	}

	// Phase 3: stale sessions. This is now the PRIMARY terminator, not a
	// backstop — Stop no longer closes anything — so it is on by default.
	// TEAMSTER_GC_STALE_HOURS=0 still disables it. Uses the *ExceptLiveLead
	// variants (wh2-idle-teammate-exemption): an in-process teammate whose
	// own session timestamp goes quiet while idle — the protocol's normal
	// steady state, not a hung or abandoned session — must not have its
	// interval closed and its session marked out from under it while its
	// lead session is plainly still connected. See store.go's interface doc
	// for the exemption's exact shape and why the two manual drains (`wms
	// drain`, `POST /wms/api/drain`) deliberately do not get it.
	if r.gcStaleHours > 0 {
		threshold := time.Now().UTC().Add(-time.Duration(r.gcStaleHours) * time.Hour)
		n3, err := r.store.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, threshold)
		if err != nil {
			slog.Warn("reaper phase 3 failed", "error", err)
		} else if n3 > 0 {
			slog.Info("reaper: closed intervals for stale sessions",
				"closed", n3, "stale_hours", r.gcStaleHours)
			// Only when something actually closed: invalidating on every
			// tick regardless would force a DB round-trip for every cached
			// agent on the hub on its next tool call for no reason. n3 == 0
			// means no interval anywhere was stale, so no cached hasFocus
			// entry can be wrong.
			if r.nudgeCache != nil {
				r.nudgeCache.invalidateAll()
			}
		}

		// Mark the sessions themselves closed, strictly AFTER closing their
		// intervals: CloseIntervalsForStaleSessionsExceptLiveLead skips
		// sessions already marked closed, so doing this first would strand
		// their intervals open until a later pass. Keeps the roster's
		// "closed" liveness tier reachable — the removed Stop-time close
		// was its only writer. Uses the matching exempt variant: marking an
		// exempted teammate's session closed anyway would let phase 2
		// (CloseIntervalsForClosedSessions — status='closed' only, no
		// staleness check) close that same just-exempted interval on the
		// very next sweep, quietly defeating the exemption one reaper
		// interval later.
		nS, err := r.store.MarkStaleSessionsClosedExceptLiveLead(ctx, threshold)
		if err != nil {
			slog.Warn("reaper phase 3 session close failed", "error", err)
		} else if nS > 0 {
			slog.Info("reaper: marked stale sessions closed",
				"sessions", nS, "stale_hours", r.gcStaleHours)
		}
	}
}
