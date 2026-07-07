package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

const remoteFloorMethod = "synthesized_remote_floor"

// RemoteOrphanStats summarizes one remote-orphan synthesis pass.
type RemoteOrphanStats struct {
	Examined          int
	Synthesized       int
	NoConcurrentFocus int
	Skipped           int
	DryRun            bool
}

// SynthesizeRemoteOrphans is the B2 pass: for remote sessions that have NO
// focus interval, NO brief directive, and NO accessible transcript, it uses
// temporal correlation — attributing orphan cost to whatever WMS entity
// concurrent sessions on the SAME host were focused on.
//
// This is the LAST deterministic attribution pass in the sweep pipeline. It
// runs AFTER recover-directives and BEFORE any LLM tier.
//
// For each orphan, the pass finds focus intervals from OTHER sessions on the
// same host that overlap the orphan's time window (RecoveryStore.
// ConcurrentFocusCandidates, raw — F1), picks the most specific covering
// entity (preferring Outcomes over WorkUnits; ties broken by temporal
// overlap) via concurrentFocusEntity's Go-side ranking, and applies via
// ApplyRecovery.
func (r *Runner) SynthesizeRemoteOrphans(ctx context.Context, hubHost string, dryRun bool) (RemoteOrphanStats, error) {
	var stats RemoteOrphanStats
	stats.DryRun = dryRun

	orphanIDs, err := r.rec.RemoteOrphans(ctx, hubHost)
	if err != nil {
		return stats, fmt.Errorf("list remote orphans: %w", err)
	}
	if len(orphanIDs) == 0 {
		r.log.Info("synthesize-remote-orphans: no orphans to process")
		return stats, nil
	}

	for _, sessionID := range orphanIDs {
		stats.Examined++

		window, ok, err := r.rec.SessionTimeWindow(ctx, sessionID)
		if err != nil {
			r.log.Warn("synthesize-remote-orphans: session time window failed; skipping",
				"session_id", sessionID, "error", err)
			stats.Skipped++
			continue
		}
		if !ok {
			stats.Skipped++
			continue
		}

		msgs, err := r.rec.ReclaimableMessages(ctx, sessionID, "", false, []string{"unallocated", "sweep_skipped"})
		if err != nil {
			return stats, fmt.Errorf("session %s: list reclaimable messages: %w", sessionID, err)
		}
		if len(msgs) == 0 {
			continue
		}
		// Any message's host is the orphan session's host — RemoteOrphans
		// guarantees every message on this session shares one non-hub host.
		host := msgs[0].Host

		entity, ok, err := r.concurrentFocusEntity(ctx, sessionID, host, window)
		if err != nil {
			r.log.Warn("synthesize-remote-orphans: concurrent focus query failed; skipping",
				"session_id", sessionID, "error", err)
			stats.Skipped++
			continue
		}
		if !ok {
			stats.NoConcurrentFocus++
			continue
		}

		if dryRun {
			stats.Synthesized += len(msgs)
			r.log.Info("synthesize-remote-orphans (dry-run): would re-attribute",
				"session_id", sessionID, "count", len(msgs),
				"to_entity_type", entity.EntityType, "to_entity_id", entity.EntityID)
			continue
		}

		msgIDs := make([]string, len(msgs))
		for i, m := range msgs {
			msgIDs[i] = m.MessageID
		}
		if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
			Strategy:   "remote_floor",
			Method:     remoteFloorMethod,
			MessageIDs: msgIDs,
			Entity:     entity,
			Evidence: map[string]any{
				"session_id": sessionID,
				"confidence": "temporal_correlation",
				"evidence_excerpt": fmt.Sprintf("concurrent focus on %s/%s from host %s",
					entity.EntityType, entity.EntityID, host),
				"mapping_source": "temporal_correlation",
			},
		}); err != nil {
			return stats, fmt.Errorf("session %s: apply remote floor: %w", sessionID, err)
		}
		stats.Synthesized += len(msgIDs)
	}

	if !dryRun && stats.Synthesized > 0 {
		if err := r.alloc.BuildCostRollup(ctx); err != nil {
			return stats, fmt.Errorf("synthesize-remote-orphans: rebuild cost_rollup: %w", err)
		}
		if _, err := r.alloc.AssembleIntervalCost(ctx); err != nil {
			return stats, fmt.Errorf("synthesize-remote-orphans: reassemble interval cost: %w", err)
		}
	}

	r.log.Info("synthesize-remote-orphans pass complete",
		"examined", stats.Examined, "synthesized", stats.Synthesized,
		"no_concurrent_focus", stats.NoConcurrentFocus,
		"skipped", stats.Skipped, "dry_run", dryRun)
	return stats, nil
}

// UnsynthesizeRemoteFloor reverses a B2 pass: deletes every
// method='synthesized_remote_floor' attribution and its evidence, returning
// those messages to the unallocated bucket.
func (r *Runner) UnsynthesizeRemoteFloor(ctx context.Context) (int, error) {
	n, err := r.rec.UncoverRecovery(ctx, "remote_floor")
	return int(n), err
}

// concurrentFocusEntity finds the WMS entity that OTHER sessions on the same
// host were focused on during the orphan's time window. Picks the entity with
// the most temporal overlap; among ties, prefers the more specific entity
// type. F1 fix: this ranking runs in Go over raw candidates from
// ConcurrentFocusCandidates — the backend supplies intervals only, never picks
// a winner (already true in the pre-port code; this port keeps it in Go
// rather than letting a "resolved" store method bury it per-backend).
func (r *Runner) concurrentFocusEntity(ctx context.Context, orphanSessionID, host string, w store.TimeWindow) (store.EntityRef, bool, error) {
	cands, err := r.rec.ConcurrentFocusCandidates(ctx, orphanSessionID, host, w)
	if err != nil {
		return store.EntityRef{}, false, err
	}

	var bestEntity store.EntityRef
	var bestOverlap int64
	found := false
	for _, c := range cands {
		end := c.End
		if end.IsZero() {
			// Open interval: assume it extends an hour past the window for
			// ranking purposes, mirroring the pre-port SQL's
			// COALESCE(ended_at, windowEnd+1h).
			end = w.End.Add(time.Hour)
		}
		overlapEnd := end
		if w.End.Before(overlapEnd) {
			overlapEnd = w.End
		}
		overlapStart := c.Start
		if w.Start.After(overlapStart) {
			overlapStart = w.Start
		}
		// >= 0, not > 0: an orphan session with a single token_ledger row has
		// a zero-width SessionTimeWindow (Start == End), so a covering focus
		// interval legitimately produces overlap == 0. Mirrors the inclusive
		// boundary comparison ConcurrentFocusCandidates' SQL already used to
		// select this candidate (started_at <= w.End AND ended_at >= w.Start).
		overlap := int64(overlapEnd.Sub(overlapStart).Seconds())
		if overlap < 0 {
			continue
		}

		if !found || overlap > bestOverlap ||
			(overlap == bestOverlap && entitySpecificity[c.Entity.EntityType] > entitySpecificity[bestEntity.EntityType]) {
			bestEntity, bestOverlap, found = c.Entity, overlap, true
		}
	}
	return bestEntity, found, nil
}
