package rollup

import (
	"context"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/store"
)

const gapMethod = "gap_recovery"

// GapStats summarizes one gap recovery pass.
type GapStats struct {
	Sessions  int
	Examined  int
	Recovered int
	Skipped   int
}

// RecoverGaps re-attributes method='unallocated' cost in partial-gap sessions —
// sessions where BOTH attributed and unallocated messages coexist. The gaps are
// typically lead threads that never called setFocus (the lead coordinates while
// teammates hold focus) or teammate threads spawned before focus was established.
//
// Algorithm:
//  1. Find gap threads: (session_id, agent_name) pairs with method='unallocated'
//     rows in sessions that also have non-unallocated rows (RecoveryStore.GapThreads).
//  2. For each gap thread, resolve the attribution entity from the session's
//     existing attributions (resolveGapEntity):
//     - Lead thread (agent_name=''): find the session's strategic Outcome.
//     - Teammate thread: check if this agent has attributed messages elsewhere in
//       the session; else inherit the session's strategic outcome.
//  3. If no entity can be resolved, skip the thread (left for LLM fallback).
//  4. ApplyRecovery: in-place UPDATE usage_attribution + INSERT gap_evidence.
//
// CONSERVATION: one row/message, weight 1.0; SUM(cost_facts) unchanged.
// REVERSIBLE: UncoverRecovery("gap") deletes by method + removes evidence.
func (r *Runner) RecoverGaps(ctx context.Context, dryRun bool) (GapStats, error) {
	threads, err := r.rec.GapThreads(ctx)
	if err != nil {
		return GapStats{}, fmt.Errorf("list gap threads: %w", err)
	}

	var stats GapStats
	for _, gt := range threads {
		stats.Sessions++

		entity, resolveMethod, resolvedFrom, err := r.resolveGapEntity(ctx, gt)
		if err != nil {
			r.log.Warn("gap-recovery: entity resolution failed; skipping thread",
				"session_id", gt.SessionID, "agent_name", gt.AgentName, "error", err)
			stats.Skipped += int(gt.MessageCount)
			continue
		}
		if entity.EntityType == "" || entity.EntityID == "" {
			stats.Skipped += int(gt.MessageCount)
			continue
		}

		msgs, err := r.rec.ReclaimableMessages(ctx, gt.SessionID, gt.AgentName, true, []string{"unallocated"})
		if err != nil {
			return stats, fmt.Errorf("session %s agent %q: list gap messages: %w",
				gt.SessionID, gt.AgentName, err)
		}
		stats.Examined += len(msgs)

		if dryRun {
			stats.Recovered += len(msgs)
			r.log.Info("gap-recovery (dry-run): would re-attribute",
				"session_id", gt.SessionID, "agent_name", gt.AgentName, "count", len(msgs),
				"to_entity_type", entity.EntityType, "to_entity_id", entity.EntityID,
				"resolution_method", resolveMethod)
			continue
		}
		if len(msgs) == 0 {
			continue
		}

		msgIDs := make([]string, len(msgs))
		for i, m := range msgs {
			msgIDs[i] = m.MessageID
		}
		if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
			Strategy:   "gap",
			Method:     gapMethod,
			MessageIDs: msgIDs,
			Entity:     entity,
			Evidence: map[string]any{
				"session_id":           gt.SessionID,
				"agent_name":           gt.AgentName,
				"resolution_method":    resolveMethod,
				"resolved_from_entity": resolvedFrom,
			},
		}); err != nil {
			return stats, fmt.Errorf("session %s: apply gap recovery: %w", gt.SessionID, err)
		}
		stats.Recovered += len(msgIDs)
	}

	if !dryRun && stats.Recovered > 0 {
		if err := r.alloc.BuildCostRollup(ctx); err != nil {
			return stats, fmt.Errorf("gap-recovery: rebuild cost_rollup: %w", err)
		}
		if _, err := r.alloc.AssembleIntervalCost(ctx); err != nil {
			return stats, fmt.Errorf("gap-recovery: reassemble interval cost: %w", err)
		}
	}

	r.log.Info("gap-recovery pass complete",
		"threads", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "skipped", stats.Skipped,
		"dry_run", dryRun)
	return stats, nil
}

// resolveGapEntity determines the attribution entity for a gap thread.
//
// For a teammate: first check if this agent has non-unallocated attributions
// elsewhere in the same session (it focused later). Otherwise fall back to the
// session's strategic outcome. For the lead (agent_name=''): go straight to
// the session's strategic outcome.
//
// Returns (entity, resolutionMethod, resolvedFrom, error). A zero-value entity
// means no resolution was possible.
func (r *Runner) resolveGapEntity(ctx context.Context, gt store.GapThread) (store.EntityRef, string, string, error) {
	if gt.AgentName != "" {
		cands, err := r.rec.AgentAttributionCandidates(ctx, gt.SessionID, gt.AgentName)
		if err != nil {
			return store.EntityRef{}, "", "", err
		}
		ref, ok := mostSpecific(strategicCandidates(cands))
		if !ok {
			ref, ok = mostSpecific(cands)
		}
		if ok {
			return ref, "agent_focus_inheritance", ref.EntityType + "/" + ref.EntityID, nil
		}
	}

	ref, err := r.sessionStrategicOutcome(ctx, gt.SessionID)
	if err != nil {
		return store.EntityRef{}, "", "", err
	}
	if ref.EntityType != "" && ref.EntityID != "" {
		return ref, "session_outcome_inference", ref.EntityType + "/" + ref.EntityID, nil
	}
	return store.EntityRef{}, "", "", nil
}

// sessionStrategicOutcome finds the session's strategic outcome from its
// attributed messages. Prefer entity_type='outcome' directly; if only
// workunits, look up their parent outcome; legacy v1 entity types are used
// as-is (the attribution points at a real entity even if the type is pre-v2).
func (r *Runner) sessionStrategicOutcome(ctx context.Context, sessionID string) (store.EntityRef, error) {
	cands, err := r.rec.SessionAttributionEntities(ctx, sessionID)
	if err != nil {
		return store.EntityRef{}, err
	}

	var outcomes, workunits, legacy []store.EntityRef
	for _, c := range cands {
		switch c.EntityType {
		case "outcome":
			outcomes = append(outcomes, c)
		case "workunit":
			workunits = append(workunits, c)
		default:
			legacy = append(legacy, c)
		}
	}

	if len(outcomes) > 0 {
		return outcomes[0], nil
	}
	if len(workunits) > 0 {
		ot, oid, err := r.resolveOutcome(ctx, "workunit", workunits[0].EntityID)
		if err != nil {
			return store.EntityRef{}, err
		}
		return store.EntityRef{EntityType: ot, EntityID: oid}, nil
	}
	if len(legacy) > 0 {
		return legacy[0], nil
	}
	return store.EntityRef{}, nil
}

// UncoverGaps reverses a gap recovery pass: deletes every method='gap_recovery'
// attribution and its evidence, returning those messages to the unallocated bucket.
func (r *Runner) UncoverGaps(ctx context.Context) (int, error) {
	n, err := r.rec.UncoverRecovery(ctx, "gap")
	return int(n), err
}
