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
//     configurable threshold (disabled when gcStaleHours == 0).
//
// The reaper closes INTERVALS only, never entities.
type reaper struct {
	store        store.Store
	interval     time.Duration
	gcStaleHours int
	stopCh       <-chan struct{}
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

// sweep runs one reaper pass. The Stop handler (W2) may have already drained
// some of these intervals — that's benign: the UPDATE is idempotent (ended_at
// IS NULL guard means already-closed rows are skipped, 0 rows affected).
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

	// Phase 3: stale sessions (guarded, disabled by default).
	if r.gcStaleHours > 0 {
		threshold := time.Now().UTC().Add(-time.Duration(r.gcStaleHours) * time.Hour)
		n3, err := r.store.CloseIntervalsForStaleSessions(ctx, threshold)
		if err != nil {
			slog.Warn("reaper phase 3 failed", "error", err)
		} else if n3 > 0 {
			slog.Info("reaper: closed intervals for stale sessions",
				"closed", n3, "stale_hours", r.gcStaleHours)
		}
	}
}
