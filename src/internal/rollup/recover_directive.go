package rollup

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// directiveMethod is the attribution method label for cost re-attributed from a
// focus-less remote TEAMMATE's dispatch-brief directive — the mandated
// wms_setFocus(entityType, entityID) instruction the lead embeds in every brief
// (skel/lib/plugin/skills/bootstrap/SKILL.md §"Write the technical brief") that
// the teammate was told to call FIRST but never did. It is distinct from
// transcript_focus_recovery (real setFocus read from a local transcript),
// admin_warmup, gap_recovery, and synthesized_outcome (LLM judgment) so
// directive-recovered cost is filterable and reversible. The link is
// deterministic and protocol-grounded — the brief names the exact entity — so
// this attributes to the CORRECT entity, not a synthesized placeholder. The
// method column is VARCHAR(48); this label is 25 chars.
const directiveMethod = "brief_directive_recovery"

// DirectiveStats summarizes one brief-directive recovery pass.
type DirectiveStats struct {
	Sessions   int // sessions with a brief_directive interval and reclaimable cost
	Examined   int // unallocated/sweep_skipped messages considered
	Recovered  int // re-attributed to the directive's named entity
	NoEntity   int // directive named an entity that no longer exists — skipped
}

// directiveSession is one (session, agent) group that has a brief_directive
// focus interval and at least one reclaimable (unallocated/sweep_skipped)
// message. entity_type/entity_id are the directive's named entity.
type directiveSession struct {
	sessionID  string
	agentName  string
	entityType string
	entityID   string
}

// A brief-directive recovery may overwrite only cost that is currently
// unattributed: method IN ('unallocated','sweep_skipped'). 'sweep_skipped' is
// included because the live focus-less remote sessions were already examined by
// the LLM sweep and marked skipped (it could not read the Mac transcript); a
// directive is a NEW, deterministic signal that supersedes that skip. No real
// attribution method (temporal_join, transcript_focus_recovery, admin_warmup,
// gap_recovery, synthesized_outcome) is ever touched.

// RecoverDirective re-attributes a focus-less remote teammate's cost to the
// entity its dispatch brief told it to focus on. The remote token-scraper ships
// the brief's wms_setFocus directive to the hub's /focus-timeline endpoint,
// which writes it as a kind='focus' interval with identity_source='brief_directive'
// ONLY when the session has no real focus interval (subordinate write). This
// pass consumes those directive intervals:
//
//  1. Find (session, agent) groups that have a brief_directive interval AND
//     still hold reclaimable (unallocated/sweep_skipped) messages.
//  2. Validate the directive's named entity still exists (a workunit's parent
//     outcome, or an outcome itself). A dangling entity is skipped (NoEntity) —
//     never invent attribution to a deleted entity.
//  3. Re-attribute EVERY reclaimable message of that session+agent to the named
//     entity with method='brief_directive_recovery'. The directive is the
//     teammate's FIRST instruction, so it covers the whole session (no warmup
//     split — unlike real setFocus, there is no pre-focus window to preserve).
//  4. Record directive_evidence provenance.
//
// CONSERVATION: in-place UPDATE scoped to reclaimable methods; one row/message,
// weight 1.0; SUM(cost_facts) unchanged.
//
// REVERSIBLE: UncoverDirective deletes by method + evidence, returning messages
// to the unallocated bucket; a subsequent Allocate restores prior state.
//
// HOST-NEUTRAL: unlike RecoverFocus/RecoverWarmup this pass reads NO transcript —
// it works purely from DB directive intervals the remote scraper already shipped,
// so it runs correctly on the hub for remote sessions and needs no host scoping.
// A directive interval only exists for a session that had no real focus, so this
// pass never competes with real-focus attribution.
func (r *Runner) RecoverDirective(ctx context.Context, dryRun bool) (DirectiveStats, error) {
	sessions, err := r.directiveSessions(ctx)
	if err != nil {
		return DirectiveStats{}, fmt.Errorf("list directive sessions: %w", err)
	}

	now := time.Now().UTC()
	var stats DirectiveStats

	for _, s := range sessions {
		// Validate the named entity still resolves (its parent outcome exists, or
		// it is an existing outcome). resolveOutcome returns ("","") for a missing
		// or dangling entity — we then skip rather than attribute to a ghost.
		if oType, oID, err := r.resolveOutcome(ctx, s.entityType, s.entityID); err != nil {
			return stats, fmt.Errorf("session %s: resolve directive entity: %w", s.sessionID, err)
		} else if oType == "" || oID == "" {
			stats.NoEntity++
			r.log.Warn("recover-directive: directive entity does not resolve; skipping session",
				"session_id", s.sessionID, "agent_name", s.agentName,
				"entity_type", s.entityType, "entity_id", s.entityID)
			continue
		}

		msgs, err := r.reclaimableMessages(ctx, s.sessionID, s.agentName)
		if err != nil {
			return stats, fmt.Errorf("session %s: list reclaimable messages: %w", s.sessionID, err)
		}
		if len(msgs) == 0 {
			continue
		}
		stats.Sessions++

		for _, m := range msgs {
			stats.Examined++
			if dryRun {
				stats.Recovered++
				r.log.Info("recover-directive (dry-run): would re-attribute",
					"message_id", m.messageID, "session_id", s.sessionID,
					"agent_name", s.agentName,
					"to_entity_type", s.entityType, "to_entity_id", s.entityID)
				continue
			}
			if err := r.applyDirective(ctx, m, s, now); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply directive: %w", s.sessionID, m.messageID, err)
			}
			stats.Recovered++
		}
	}

	if !dryRun && stats.Recovered > 0 {
		rows, err := r.BuildCostRollup(ctx)
		if err != nil {
			return stats, fmt.Errorf("recover-directive: rebuild cost_rollup: %w", err)
		}
		intervals, err := r.AssembleIntervalCost(ctx)
		if err != nil {
			return stats, fmt.Errorf("recover-directive: reassemble interval cost: %w", err)
		}
		r.log.Info("recover-directive rebuilt aggregates", "rollup_rows", rows, "intervals_costed", intervals)
	}

	r.log.Info("recover-directive pass complete",
		"sessions", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "no_entity", stats.NoEntity, "dry_run", dryRun)
	return stats, nil
}

// directiveSessions returns the (session, agent, entity) groups that have a
// brief_directive focus interval AND still hold reclaimable cost. The join to
// usage_attribution+token_ledger ensures we only touch sessions with something
// to reclaim. agent_name comes from the directive interval; a session may carry
// directives for several agents (each its own row).
//
// When a session somehow has more than one brief_directive interval for the
// same agent (should not happen — the hub's subordinate write inserts at most
// one), GROUP BY collapses them and MIN() picks a single deterministic entity.
func (r *Runner) directiveSessions(ctx context.Context) ([]directiveSession, error) {
	q := `
		SELECT i.session_id, i.agent_name, MIN(i.entity_type), MIN(i.entity_id)
		FROM wms_intervals i
		WHERE i.kind = 'focus' AND i.identity_source = 'brief_directive'
		  AND EXISTS (
			SELECT 1 FROM usage_attribution ua
			JOIN token_ledger t ON t.message_id = ua.message_id
			WHERE t.session_id = i.session_id
			  AND TRIM(LEADING '@' FROM t.agent_name) = TRIM(LEADING '@' FROM i.agent_name)
			  AND ua.method IN ('unallocated','sweep_skipped'))
		GROUP BY i.session_id, i.agent_name`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []directiveSession
	for rows.Next() {
		var s directiveSession
		if err := rows.Scan(&s.sessionID, &s.agentName, &s.entityType, &s.entityID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// reclaimableMessages returns the unallocated/sweep_skipped messages of one
// (session, agent) group. agent_name is matched @-insensitively on both sides
// (the directive interval and the ledger agree on "@name", but normalize for
// defense-in-depth, mirroring focusAt).
func (r *Runner) reclaimableMessages(ctx context.Context, sessionID, agentName string) ([]unallocatedMsg, error) {
	const q = `
		SELECT ua.message_id, t.agent_name, t.timestamp
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method IN ('unallocated','sweep_skipped')
		  AND t.session_id = ?
		  AND TRIM(LEADING '@' FROM t.agent_name) = ?`
	rows, err := r.db.QueryContext(ctx, q, sessionID, strings.TrimPrefix(agentName, "@"))
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

// applyDirective re-attributes one reclaimable message to the directive's named
// entity and records provenance, mirroring applyRecovery's transactional pattern.
// The UPDATE is scoped to reclaimable methods so it never clobbers a real
// attribution that a concurrent pass may have written.
func (r *Runner) applyDirective(ctx context.Context, m unallocatedMsg, s directiveSession, now time.Time) error {
	var intervalID uint64
	if id, ok, err := r.intervalAt(ctx, s.entityType, s.entityID, m.ts); err != nil {
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
		WHERE message_id = ? AND method IN ('unallocated','sweep_skipped')`,
		s.entityType, s.entityID, directiveMethod, intervalID, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Row was no longer reclaimable (raced). Skip evidence.
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO directive_evidence
			(message_id, entity_type, entity_id, session_id, agent_name, directive_type, directive_id, recovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type    = VALUES(entity_type),
			entity_id      = VALUES(entity_id),
			session_id     = VALUES(session_id),
			agent_name     = VALUES(agent_name),
			directive_type = VALUES(directive_type),
			directive_id   = VALUES(directive_id),
			recovered_at   = VALUES(recovered_at)`,
		m.messageID, s.entityType, s.entityID, s.sessionID, s.agentName,
		s.entityType, s.entityID, now); err != nil {
		return fmt.Errorf("insert directive evidence: %w", err)
	}

	return tx.Commit()
}

// UncoverDirective reverses a brief-directive recovery pass: deletes every
// method='brief_directive_recovery' attribution and its evidence, returning
// those messages to the unallocated bucket. Scoped strictly to the directive
// method, so it never disturbs other attribution. The brief_directive focus
// intervals themselves are left in place (they are durable provenance the
// scraper re-ships); a follow-up Allocate leaves the reverted rows unallocated
// because Allocate does not consult directive intervals (only RecoverDirective
// does), so the reversal is stable.
func (r *Runner) UncoverDirective(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		DELETE de FROM directive_evidence de
		JOIN usage_attribution ua ON ua.message_id = de.message_id
		WHERE ua.method = ?`, directiveMethod); err != nil {
		return 0, fmt.Errorf("delete directive evidence: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = ?`, directiveMethod)
	if err != nil {
		return 0, fmt.Errorf("delete directive attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
