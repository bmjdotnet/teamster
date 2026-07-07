package rollup

import (
	"context"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/store"
)

// directiveMethod is the attribution method label for cost re-attributed from a
// focus-less remote TEAMMATE's dispatch-brief directive — the mandated
// wms_setFocus(entityType, entityID) instruction the lead embeds in every brief
// that the teammate was told to call FIRST but never did. Distinct from
// transcript_focus_recovery, admin_warmup, gap_recovery, and
// synthesized_outcome so directive-recovered cost is filterable and reversible.
const directiveMethod = "brief_directive_recovery"

// DirectiveStats summarizes one brief-directive recovery pass.
type DirectiveStats struct {
	Sessions  int // sessions with a brief_directive interval and reclaimable cost
	Examined  int // unallocated/sweep_skipped messages considered
	Recovered int // re-attributed to the directive's named entity
	NoEntity  int // directive named an entity that no longer exists — skipped
}

// RecoverDirective re-attributes a focus-less remote teammate's cost to the
// entity its dispatch brief told it to focus on. The remote token-scraper ships
// the brief's wms_setFocus directive to the hub, which writes it as a
// kind='focus' interval with identity_source='brief_directive' ONLY when the
// session has no real focus interval (subordinate write). This pass consumes
// those directive intervals:
//
//  1. Find (session, agent) groups that have a brief_directive interval AND
//     still hold reclaimable (unallocated/sweep_skipped) messages
//     (RecoveryStore.DirectiveSessions).
//  2. Validate the directive's named entity still exists (a workunit's parent
//     outcome, or an outcome itself, via resolveOutcome) — a dangling entity is
//     skipped (NoEntity), never invent attribution to a deleted entity. The
//     directive's OWN entity (not the resolved parent) is what gets attributed;
//     resolveOutcome here is purely an existence check.
//  3. Re-attribute EVERY reclaimable message of that session+agent to the named
//     entity — the directive is the teammate's FIRST instruction, so it covers
//     the whole session (no warmup split).
//
// CONSERVATION: ApplyRecovery's in-place UPDATE is scoped to reclaimable
// methods; one row/message, weight 1.0.
//
// HOST-NEUTRAL: unlike RecoverFocus/RecoverWarmup this pass reads NO
// transcript — it works purely from DB directive intervals the remote scraper
// already shipped, so it runs correctly on the hub for remote sessions and
// needs no host scoping.
func (r *Runner) RecoverDirective(ctx context.Context, dryRun bool) (DirectiveStats, error) {
	sessions, err := r.rec.DirectiveSessions(ctx)
	if err != nil {
		return DirectiveStats{}, fmt.Errorf("list directive sessions: %w", err)
	}

	var stats DirectiveStats
	for _, s := range sessions {
		if oType, oID, err := r.resolveOutcome(ctx, s.Entity.EntityType, s.Entity.EntityID); err != nil {
			return stats, fmt.Errorf("session %s: resolve directive entity: %w", s.SessionID, err)
		} else if oType == "" || oID == "" {
			stats.NoEntity++
			r.log.Warn("recover-directive: directive entity does not resolve; skipping session",
				"session_id", s.SessionID, "agent_name", s.AgentName,
				"entity_type", s.Entity.EntityType, "entity_id", s.Entity.EntityID)
			continue
		}

		msgs, err := r.rec.ReclaimableMessages(ctx, s.SessionID, s.AgentName, true, []string{"unallocated", "sweep_skipped"})
		if err != nil {
			return stats, fmt.Errorf("session %s: list reclaimable messages: %w", s.SessionID, err)
		}
		if len(msgs) == 0 {
			continue
		}
		stats.Sessions++
		stats.Examined += len(msgs)

		if dryRun {
			stats.Recovered += len(msgs)
			r.log.Info("recover-directive (dry-run): would re-attribute",
				"session_id", s.SessionID, "agent_name", s.AgentName, "count", len(msgs),
				"to_entity_type", s.Entity.EntityType, "to_entity_id", s.Entity.EntityID)
			continue
		}

		msgIDs := make([]string, len(msgs))
		for i, m := range msgs {
			msgIDs[i] = m.MessageID
		}
		if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
			Strategy:   "directive",
			Method:     directiveMethod,
			MessageIDs: msgIDs,
			Entity:     s.Entity,
			Evidence:   map[string]any{"session_id": s.SessionID, "agent_name": s.AgentName},
		}); err != nil {
			return stats, fmt.Errorf("session %s: apply directive: %w", s.SessionID, err)
		}
		stats.Recovered += len(msgIDs)
	}

	if !dryRun && stats.Recovered > 0 {
		if err := r.alloc.BuildCostRollup(ctx); err != nil {
			return stats, fmt.Errorf("recover-directive: rebuild cost_rollup: %w", err)
		}
		if _, err := r.alloc.AssembleIntervalCost(ctx); err != nil {
			return stats, fmt.Errorf("recover-directive: reassemble interval cost: %w", err)
		}
	}

	r.log.Info("recover-directive pass complete",
		"sessions", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "no_entity", stats.NoEntity, "dry_run", dryRun)
	return stats, nil
}

// UncoverDirective reverses a brief-directive recovery pass: deletes every
// method='brief_directive_recovery' attribution and its evidence, returning
// those messages to the unallocated bucket. The brief_directive focus
// intervals themselves are left in place (durable provenance the scraper
// re-ships).
func (r *Runner) UncoverDirective(ctx context.Context) (int, error) {
	n, err := r.rec.UncoverRecovery(ctx, "directive")
	return int(n), err
}
