package rollup

import (
	"context"
	"fmt"
	"time"
)

const gapMethod = "gap_recovery"

// GapStats summarizes one gap recovery pass.
type GapStats struct {
	Sessions  int
	Examined  int
	Recovered int
	Skipped   int
}

// gapThread is one (session_id, agent_name) pair that has unallocated messages
// inside a session that ALSO holds non-unallocated (attributed) messages.
type gapThread struct {
	sessionID string
	agentName string
	msgCount  int
}

// RecoverGaps re-attributes method='unallocated' cost in partial-gap sessions —
// sessions where BOTH attributed and unallocated messages coexist. The gaps are
// typically lead threads that never called setFocus (the lead coordinates while
// teammates hold focus) or teammate threads spawned before focus was established.
//
// Algorithm:
//  1. Find gap threads: (session_id, agent_name) pairs with method='unallocated'
//     rows in sessions that also have non-unallocated rows.
//  2. For each gap thread, resolve the attribution entity from the session's
//     existing attributions:
//     - Lead thread (agent_name=''): find the session's strategic Outcome — the
//       highest-level entity that attributed messages in the same session point to.
//       Prefer entity_type='outcome'; if only workunits, look up their parent.
//     - Teammate thread: check if this agent has attributed messages elsewhere in
//       the session (it focused later). If so, use that entity. Otherwise, inherit
//       the session's strategic outcome (same as lead resolution).
//  3. If no entity can be resolved, skip the thread (left for LLM fallback).
//  4. In-place UPDATE usage_attribution SET method='gap_recovery'.
//  5. Record gap_evidence provenance.
//
// CONSERVATION: in-place UPDATE scoped to method='unallocated'; one row/message,
// weight 1.0; SUM(cost_facts) unchanged.
//
// REVERSIBLE: UncoverGaps deletes by method + removes evidence.
func (r *Runner) RecoverGaps(ctx context.Context, dryRun bool) (GapStats, error) {
	threads, err := r.gapThreads(ctx)
	if err != nil {
		return GapStats{}, fmt.Errorf("list gap threads: %w", err)
	}

	now := time.Now().UTC()
	var stats GapStats

	for _, gt := range threads {
		stats.Sessions++

		entityType, entityID, resolveMethod, resolvedFrom, err := r.resolveGapEntity(ctx, gt)
		if err != nil {
			r.log.Warn("gap-recovery: entity resolution failed; skipping thread",
				"session_id", gt.sessionID, "agent_name", gt.agentName, "error", err)
			stats.Skipped += gt.msgCount
			continue
		}
		if entityType == "" || entityID == "" {
			stats.Skipped += gt.msgCount
			continue
		}

		msgs, err := r.gapMessages(ctx, gt)
		if err != nil {
			return stats, fmt.Errorf("session %s agent %q: list gap messages: %w",
				gt.sessionID, gt.agentName, err)
		}

		for _, m := range msgs {
			stats.Examined++

			if dryRun {
				stats.Recovered++
				r.log.Info("gap-recovery (dry-run): would re-attribute",
					"message_id", m.messageID, "session_id", gt.sessionID,
					"agent_name", gt.agentName,
					"to_entity_type", entityType, "to_entity_id", entityID,
					"resolution_method", resolveMethod)
				continue
			}

			if err := r.applyGapRecovery(ctx, m, gt, entityType, entityID, resolveMethod, resolvedFrom, now); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply gap recovery: %w",
					gt.sessionID, m.messageID, err)
			}
			stats.Recovered++
		}
	}

	if !dryRun && stats.Recovered > 0 {
		rows, err := r.BuildCostRollup(ctx)
		if err != nil {
			return stats, fmt.Errorf("gap-recovery: rebuild cost_rollup: %w", err)
		}
		intervals, err := r.AssembleIntervalCost(ctx)
		if err != nil {
			return stats, fmt.Errorf("gap-recovery: reassemble interval cost: %w", err)
		}
		r.log.Info("gap-recovery rebuilt aggregates", "rollup_rows", rows, "intervals_costed", intervals)
	}

	r.log.Info("gap-recovery pass complete",
		"threads", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "skipped", stats.Skipped,
		"dry_run", dryRun)
	return stats, nil
}

// gapThreads returns (session_id, agent_name) pairs that have method='unallocated'
// rows in sessions that ALSO contain non-unallocated rows — the partial-gap set.
func (r *Runner) gapThreads(ctx context.Context) ([]gapThread, error) {
	const q = `
		SELECT t.session_id, t.agent_name, COUNT(*) AS cnt
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated'
		  AND t.session_id <> ''
		  AND t.session_id IN (
			SELECT DISTINCT t2.session_id
			FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE ua2.method <> 'unallocated' AND t2.session_id <> ''
		  )
		GROUP BY t.session_id, t.agent_name`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []gapThread
	for rows.Next() {
		var gt gapThread
		if err := rows.Scan(&gt.sessionID, &gt.agentName, &gt.msgCount); err != nil {
			return nil, err
		}
		out = append(out, gt)
	}
	return out, rows.Err()
}

// gapMessages returns the method='unallocated' messages for one gap thread.
func (r *Runner) gapMessages(ctx context.Context, gt gapThread) ([]unallocatedMsg, error) {
	const q = `
		SELECT ua.message_id, t.agent_name, t.timestamp
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated'
		  AND t.session_id = ? AND t.agent_name = ?`
	rows, err := r.db.QueryContext(ctx, q, gt.sessionID, gt.agentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []unallocatedMsg
	for rows.Next() {
		var m unallocatedMsg
		if err := rows.Scan(&m.messageID, &m.agentName, &m.ts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// resolveGapEntity determines the attribution entity for a gap thread.
//
// For a teammate: first check if this agent has non-unallocated attributions
// elsewhere in the same session (it focused later); use that entity. Otherwise
// fall back to the session's strategic outcome.
//
// For the lead (agent_name=''): resolve the session's strategic outcome from
// the attributed messages of teammates in the same session. Prefer
// entity_type='outcome'; if only workunits, look up their parent outcome.
//
// Returns (entityType, entityID, resolutionMethod, resolvedFrom, error).
// Empty entityType/entityID means no resolution was possible.
func (r *Runner) resolveGapEntity(ctx context.Context, gt gapThread) (string, string, string, string, error) {
	// For teammates, first check if this agent has attributed messages in the
	// same session (the agent focused later in the session).
	if gt.agentName != "" {
		et, eid, err := r.agentAttributionInSession(ctx, gt.sessionID, gt.agentName)
		if err != nil {
			return "", "", "", "", err
		}
		if et != "" && eid != "" {
			return et, eid, "agent_focus_inheritance", et + "/" + eid, nil
		}
	}

	// Fall back to the session's strategic outcome.
	et, eid, err := r.sessionStrategicOutcome(ctx, gt.sessionID)
	if err != nil {
		return "", "", "", "", err
	}
	if et != "" && eid != "" {
		return et, eid, "session_outcome_inference", et + "/" + eid, nil
	}
	return "", "", "", "", nil
}

// agentAttributionInSession finds a non-unallocated attribution for a specific
// agent within a session. If the agent focused on multiple entities, prefer
// outcome over workunit (the strategic entity).
func (r *Runner) agentAttributionInSession(ctx context.Context, sessionID, agentName string) (string, string, error) {
	const q = `
		SELECT ua.entity_type, ua.entity_id
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method <> 'unallocated'
		  AND t.session_id = ?
		  AND t.agent_name = ?
		  AND ua.entity_type <> ''
		GROUP BY ua.entity_type, ua.entity_id`
	rows, err := r.db.QueryContext(ctx, q, sessionID, agentName)
	if err != nil {
		return "", "", err
	}
	defer rows.Close() //nolint:errcheck

	var cands []focusCandidate
	for rows.Next() {
		var c focusCandidate
		if err := rows.Scan(&c.entityType, &c.entityID); err != nil {
			return "", "", err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}

	// Prefer strategic entities for the gap attribution.
	if et, ei, ok := mostSpecific(strategicCandidates(cands)); ok {
		return et, ei, nil
	}
	et, ei, ok := mostSpecific(cands)
	if !ok {
		return "", "", nil
	}
	return et, ei, nil
}

// sessionStrategicOutcome finds the session's strategic outcome from its
// attributed messages. Prefer entity_type='outcome' directly; if only
// workunits, look up their parent outcome.
func (r *Runner) sessionStrategicOutcome(ctx context.Context, sessionID string) (string, string, error) {
	const q = `
		SELECT DISTINCT ua.entity_type, ua.entity_id
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method <> 'unallocated'
		  AND t.session_id = ?
		  AND ua.entity_type <> ''`
	rows, err := r.db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close() //nolint:errcheck

	var outcomes []focusCandidate
	var workunits []focusCandidate
	var legacy []focusCandidate
	for rows.Next() {
		var c focusCandidate
		if err := rows.Scan(&c.entityType, &c.entityID); err != nil {
			return "", "", err
		}
		switch c.entityType {
		case "outcome":
			outcomes = append(outcomes, c)
		case "workunit":
			workunits = append(workunits, c)
		default:
			legacy = append(legacy, c)
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}

	// Direct outcome wins.
	if len(outcomes) > 0 {
		return outcomes[0].entityType, outcomes[0].entityID, nil
	}

	// Look up the parent outcome of the first workunit.
	if len(workunits) > 0 {
		ot, oid, err := r.resolveOutcome(ctx, "workunit", workunits[0].entityID)
		if err != nil {
			return "", "", err
		}
		return ot, oid, nil
	}

	// Legacy v1 entity types (project, goal, task). Use them as-is — the
	// attribution points at a real entity even if the type is pre-v2.
	if len(legacy) > 0 {
		return legacy[0].entityType, legacy[0].entityID, nil
	}
	return "", "", nil
}

// applyGapRecovery re-attributes one unallocated gap message and records
// provenance, mirroring the transactional pattern from applyRecovery.
func (r *Runner) applyGapRecovery(ctx context.Context, m unallocatedMsg, gt gapThread, entityType, entityID, resolveMethod, resolvedFrom string, now time.Time) error {
	var intervalID uint64
	if id, ok, err := r.intervalAt(ctx, entityType, entityID, m.ts); err != nil {
		return fmt.Errorf("intervalAt: %w", err)
	} else if ok {
		intervalID = id
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_attribution
		SET entity_type = ?, entity_id = ?, method = ?, interval_id = ?, computed_at = ?
		WHERE message_id = ? AND method = 'unallocated'`,
		entityType, entityID, gapMethod, intervalID, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO gap_evidence
			(message_id, entity_type, entity_id, session_id, agent_name, resolution_method, resolved_from_entity, recovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type          = VALUES(entity_type),
			entity_id            = VALUES(entity_id),
			session_id           = VALUES(session_id),
			agent_name           = VALUES(agent_name),
			resolution_method    = VALUES(resolution_method),
			resolved_from_entity = VALUES(resolved_from_entity),
			recovered_at         = VALUES(recovered_at)`,
		m.messageID, entityType, entityID, gt.sessionID, gt.agentName,
		resolveMethod, resolvedFrom, now); err != nil {
		return fmt.Errorf("insert gap evidence: %w", err)
	}

	return tx.Commit()
}

// UncoverGaps reverses a gap recovery pass: deletes every method='gap_recovery'
// attribution and its evidence, returning those messages to the unallocated bucket.
func (r *Runner) UncoverGaps(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		DELETE ge FROM gap_evidence ge
		JOIN usage_attribution ua ON ua.message_id = ge.message_id
		WHERE ua.method = ?`, gapMethod); err != nil {
		return 0, fmt.Errorf("delete gap evidence: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = ?`, gapMethod)
	if err != nil {
		return 0, fmt.Errorf("delete gap attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
