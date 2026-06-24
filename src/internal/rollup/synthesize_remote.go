package rollup

import (
	"context"
	"fmt"
	"time"
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

// remoteOrphan is one session whose cost is entirely unattributed and which
// originated on a non-hub host — the B2 target set.
type remoteOrphan struct {
	sessionID string
	host      string
	username  string
	msgCount  int
}

// SynthesizeRemoteOrphans is the B2 pass: for remote sessions that have NO
// focus interval, NO brief directive, and NO accessible transcript, it uses
// temporal correlation — attributing orphan cost to whatever WMS entity
// concurrent sessions on the SAME host were focused on.
//
// This is the LAST deterministic attribution pass in the sweep pipeline. It
// runs AFTER recover-directives (Step 7) and BEFORE any LLM tier.
//
// Candidate selection: a session qualifies when its host differs from the hub
// AND every one of its usage_attribution rows has method IN
// ('unallocated','sweep_skipped') AND it has no kind='focus' interval AND no
// identity_source='brief_directive' interval.
//
// For each orphan, the pass finds kind='focus' intervals from OTHER sessions
// on the same host that overlap the orphan's time window, picks the most
// specific covering entity (preferring Outcomes over WorkUnits; ties broken by
// temporal overlap), and applies the attribution via applySynthesis.
//
// CONSERVATION: in-place UPDATE scoped to reclaimable methods; one row/message,
// weight 1.0; SUM(cost_facts) unchanged.
//
// REVERSIBLE: UnsynthesizeRemoteFloor deletes by method + removes evidence.
func (r *Runner) SynthesizeRemoteOrphans(ctx context.Context, hubHost string, dryRun bool) (RemoteOrphanStats, error) {
	var stats RemoteOrphanStats
	stats.DryRun = dryRun

	orphans, err := r.remoteOrphans(ctx, hubHost)
	if err != nil {
		return stats, fmt.Errorf("list remote orphans: %w", err)
	}

	if len(orphans) == 0 {
		r.log.Info("synthesize-remote-orphans: no orphans to process")
		return stats, nil
	}

	now := time.Now().UTC()

	for _, o := range orphans {
		stats.Examined++

		minTS, maxTS, err := r.sessionTimeWindow(ctx, o.sessionID)
		if err != nil {
			r.log.Warn("synthesize-remote-orphans: session time window failed; skipping",
				"session_id", o.sessionID, "error", err)
			stats.Skipped++
			continue
		}

		entityType, entityID, err := r.concurrentFocusEntity(ctx, o.sessionID, o.host, minTS, maxTS)
		if err != nil {
			r.log.Warn("synthesize-remote-orphans: concurrent focus query failed; skipping",
				"session_id", o.sessionID, "error", err)
			stats.Skipped++
			continue
		}
		if entityType == "" || entityID == "" {
			stats.NoConcurrentFocus++
			continue
		}

		msgs, err := r.reclaimableSessionMessages(ctx, o.sessionID)
		if err != nil {
			return stats, fmt.Errorf("session %s: list reclaimable messages: %w", o.sessionID, err)
		}

		for _, m := range msgs {
			if dryRun {
				stats.Synthesized++
				r.log.Info("synthesize-remote-orphans (dry-run): would re-attribute",
					"message_id", m.messageID, "session_id", o.sessionID,
					"to_entity_type", entityType, "to_entity_id", entityID)
				continue
			}

			mapping := &SynthesisMapping{
				SessionID:       o.sessionID,
				EntityType:      entityType,
				EntityID:        entityID,
				Confidence:      "temporal_correlation",
				EvidenceExcerpt: fmt.Sprintf("concurrent focus on %s/%s from host %s", entityType, entityID, o.host),
			}
			if err := r.applyRemoteFloor(ctx, m, o.sessionID, mapping, now); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply remote floor: %w",
					o.sessionID, m.messageID, err)
			}
			stats.Synthesized++
		}
	}

	if !dryRun && stats.Synthesized > 0 {
		rows, err := r.BuildCostRollup(ctx)
		if err != nil {
			return stats, fmt.Errorf("synthesize-remote-orphans: rebuild cost_rollup: %w", err)
		}
		intervals, err := r.AssembleIntervalCost(ctx)
		if err != nil {
			return stats, fmt.Errorf("synthesize-remote-orphans: reassemble interval cost: %w", err)
		}
		r.log.Info("synthesize-remote-orphans rebuilt aggregates",
			"rollup_rows", rows, "intervals_costed", intervals)
	}

	r.log.Info("synthesize-remote-orphans pass complete",
		"examined", stats.Examined, "synthesized", stats.Synthesized,
		"no_concurrent_focus", stats.NoConcurrentFocus,
		"skipped", stats.Skipped, "dry_run", dryRun)
	return stats, nil
}

// remoteOrphans returns sessions that qualify for B2: host != hubHost, ALL
// messages have method IN ('unallocated','sweep_skipped'), and no kind='focus'
// interval exists (neither real focus nor brief_directive).
func (r *Runner) remoteOrphans(ctx context.Context, hubHost string) ([]remoteOrphan, error) {
	const q = `
		SELECT t.session_id, t.host, t.username, COUNT(*) AS cnt
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method IN ('unallocated','sweep_skipped')
		  AND t.session_id <> ''
		  AND t.host <> ?
		  AND NOT EXISTS (
			SELECT 1 FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE t2.session_id = t.session_id
			  AND ua2.method NOT IN ('unallocated','sweep_skipped')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM wms_intervals wi
			WHERE wi.session_id = t.session_id
			  AND wi.kind = 'focus'
		  )
		GROUP BY t.session_id, t.host, t.username`
	rows, err := r.db.QueryContext(ctx, q, hubHost)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []remoteOrphan
	for rows.Next() {
		var o remoteOrphan
		if err := rows.Scan(&o.sessionID, &o.host, &o.username, &o.msgCount); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// sessionTimeWindow returns the min and max timestamps of a session's
// token_ledger rows.
func (r *Runner) sessionTimeWindow(ctx context.Context, sessionID string) (min, max time.Time, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT MIN(timestamp), MAX(timestamp)
		FROM token_ledger
		WHERE session_id = ?`, sessionID).Scan(&min, &max)
	return min, max, err
}

// concurrentFocusEntity finds the WMS entity that OTHER sessions on the same
// host were focused on during the orphan's time window. It picks the entity
// with the most temporal overlap; among ties, prefers Outcomes over WorkUnits.
func (r *Runner) concurrentFocusEntity(ctx context.Context, orphanSessionID, host string, windowStart, windowEnd time.Time) (string, string, error) {
	const q = `
		SELECT wi.entity_type, wi.entity_id,
			TIMESTAMPDIFF(SECOND,
				GREATEST(wi.started_at, ?),
				LEAST(COALESCE(wi.ended_at, ?), ?)
			) AS overlap_seconds
		FROM wms_intervals wi
		WHERE wi.kind = 'focus'
		  AND wi.identity_source <> 'brief_directive'
		  AND wi.session_id <> ?
		  AND wi.session_id IN (
			SELECT DISTINCT t.session_id FROM token_ledger t WHERE t.host = ?
		  )
		  AND wi.started_at <= ?
		  AND (wi.ended_at IS NULL OR wi.ended_at >= ?)
		ORDER BY overlap_seconds DESC
		LIMIT 10`
	rows, err := r.db.QueryContext(ctx, q,
		windowStart, windowEnd.Add(time.Hour), windowEnd,
		orphanSessionID, host, windowEnd, windowStart)
	if err != nil {
		return "", "", err
	}
	defer rows.Close() //nolint:errcheck

	type candidate struct {
		entityType string
		entityID   string
		overlap    int64
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.entityType, &c.entityID, &c.overlap); err != nil {
			return "", "", err
		}
		if c.overlap > 0 {
			cands = append(cands, c)
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if len(cands) == 0 {
		return "", "", nil
	}

	// Pick best: most overlap, then most specific entity type.
	best := cands[0]
	for _, c := range cands[1:] {
		if c.overlap > best.overlap {
			best = c
		} else if c.overlap == best.overlap {
			if entitySpecificity[c.entityType] > entitySpecificity[best.entityType] {
				best = c
			}
		}
	}
	return best.entityType, best.entityID, nil
}

// reclaimableSessionMessages returns all unallocated/sweep_skipped messages
// for a session (across all agents), ordered by timestamp.
func (r *Runner) reclaimableSessionMessages(ctx context.Context, sessionID string) ([]unallocatedMsg, error) {
	const q = `
		SELECT ua.message_id, t.agent_name, t.timestamp
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method IN ('unallocated','sweep_skipped')
		  AND t.session_id = ?
		ORDER BY t.timestamp`
	rows, err := r.db.QueryContext(ctx, q, sessionID)
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

// applyRemoteFloor re-attributes one reclaimable message to the temporally
// correlated entity and records provenance in synthesis_evidence.
func (r *Runner) applyRemoteFloor(ctx context.Context, m unallocatedMsg, sessionID string, mapping *SynthesisMapping, now time.Time) error {
	var intervalID uint64
	if id, ok, err := r.intervalAt(ctx, mapping.EntityType, mapping.EntityID, m.ts); err != nil {
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
		mapping.EntityType, mapping.EntityID, remoteFloorMethod, intervalID, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO synthesis_evidence
			(message_id, entity_type, entity_id, session_id, confidence, evidence_excerpt, mapping_source, recovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type      = VALUES(entity_type),
			entity_id        = VALUES(entity_id),
			session_id       = VALUES(session_id),
			confidence       = VALUES(confidence),
			evidence_excerpt = VALUES(evidence_excerpt),
			mapping_source   = VALUES(mapping_source),
			recovered_at     = VALUES(recovered_at)`,
		m.messageID, mapping.EntityType, mapping.EntityID, sessionID,
		mapping.Confidence, mapping.EvidenceExcerpt, "temporal_correlation", now); err != nil {
		return fmt.Errorf("insert synthesis evidence: %w", err)
	}

	return tx.Commit()
}

// UnsynthesizeRemoteFloor reverses a B2 pass: deletes every
// method='synthesized_remote_floor' attribution and its evidence, returning
// those messages to the unallocated bucket. Scoped strictly to the remote
// floor method.
func (r *Runner) UnsynthesizeRemoteFloor(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		DELETE se FROM synthesis_evidence se
		JOIN usage_attribution ua ON ua.message_id = se.message_id
		WHERE ua.method = ?`, remoteFloorMethod); err != nil {
		return 0, fmt.Errorf("delete synthesis evidence: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = ?`, remoteFloorMethod)
	if err != nil {
		return 0, fmt.Errorf("delete remote floor attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
