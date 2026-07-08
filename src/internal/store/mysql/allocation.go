package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.AllocationStore = (*Store)(nil)

// entitySpecificityCase is the SQL CASE expression ranking WMS entity types
// from most to least specific, mirroring internal/rollup's entitySpecificity
// map. Both the v1 hierarchy (workitem > task > goal > project) and the v3
// attribution spine (workunit > outcome) are ranked; an unranked type sorts
// last (0), matching the Go map's zero-value-for-missing-key behavior.
const entitySpecificityCase = `CASE entity_type
	WHEN 'workunit' THEN 4 WHEN 'workitem' THEN 4
	WHEN 'task' THEN 3
	WHEN 'outcome' THEN 2 WHEN 'goal' THEN 2
	WHEN 'project' THEN 1
	ELSE 0 END`

// UnattributedMessages implements store.AllocationStore. limit <= 0 means no
// limit (matches today's Allocate, which loads the whole pending set).
func (s *Store) UnattributedMessages(ctx context.Context, limit int) ([]store.LedgerMessage, error) {
	q := `
		SELECT t.message_id, t.session_id, t.agent_name, t.host, t.username, t.timestamp, t.cost_usd
		FROM token_ledger t
		LEFT JOIN usage_attribution ua ON ua.message_id = t.message_id
		WHERE ua.message_id IS NULL`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.LedgerMessage
	for rows.Next() {
		var m store.LedgerMessage
		if err := rows.Scan(&m.MessageID, &m.SessionID, &m.AgentName, &m.Host, &m.Username, &m.Timestamp, &m.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FocusEntityAt implements store.AllocationStore: the most-specific entity
// agentName was focused on, in sessionID, at ts. Excludes brief_directive
// intervals (a focus-less teammate's INTENDED focus, recovered separately and
// reversibly by RecoverDirective) — letting this consume them would write
// indistinguishable, non-reversible temporal_join rows. The TRIM(LEADING
// '@'...) column-side normalization stays here (backend SQL), matching
// agentName already Go-normalized (bare, no '@') by the caller.
func (s *Store) FocusEntityAt(ctx context.Context, sessionID, agentName string, at time.Time) (store.EntityRef, bool, error) {
	const q = `
		SELECT entity_type, entity_id
		FROM wms_intervals
		WHERE kind = 'focus'
		  AND identity_source <> 'brief_directive'
		  AND session_id = ?
		  AND TRIM(LEADING '@' FROM agent_name) = ?
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)
		  AND (` + entitySpecificityCase + `) > 0
		ORDER BY ` + entitySpecificityCase + ` DESC
		LIMIT 1`
	var ref store.EntityRef
	err := s.db.QueryRowContext(ctx, q, sessionID, strings.TrimPrefix(agentName, "@"), at, at).
		Scan(&ref.EntityType, &ref.EntityID)
	if err != nil {
		if err == sql.ErrNoRows {
			return store.EntityRef{}, false, nil
		}
		return store.EntityRef{}, false, err
	}
	return ref, true, nil
}

// FocusEntityInSession implements store.AllocationStore: the entity the
// SESSION had focus on at ts, across ALL agents — the P1a lead-session
// fallback source. Prefers the strategic tier (outcome/goal/project) over an
// arbitrary child workunit/task (the lead's role is cross-cutting
// coordination), falling back to the overall most-specific entity only when
// no strategic interval covers ts. Ties broken by most-recently-started.
func (s *Store) FocusEntityInSession(ctx context.Context, sessionID string, at time.Time) (store.EntityRef, bool, error) {
	const strategicQ = `
		SELECT entity_type, entity_id
		FROM wms_intervals
		WHERE kind = 'focus'
		  AND identity_source <> 'brief_directive'
		  AND session_id = ?
		  AND entity_type IN ('outcome','goal','project')
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)
		ORDER BY ` + entitySpecificityCase + ` DESC, started_at DESC
		LIMIT 1`
	var ref store.EntityRef
	err := s.db.QueryRowContext(ctx, strategicQ, sessionID, at, at).Scan(&ref.EntityType, &ref.EntityID)
	if err == nil {
		return ref, true, nil
	}
	if err != sql.ErrNoRows {
		return store.EntityRef{}, false, err
	}

	const anyQ = `
		SELECT entity_type, entity_id
		FROM wms_intervals
		WHERE kind = 'focus'
		  AND identity_source <> 'brief_directive'
		  AND session_id = ?
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)
		  AND (` + entitySpecificityCase + `) > 0
		ORDER BY ` + entitySpecificityCase + ` DESC, started_at DESC
		LIMIT 1`
	err = s.db.QueryRowContext(ctx, anyQ, sessionID, at, at).Scan(&ref.EntityType, &ref.EntityID)
	if err != nil {
		if err == sql.ErrNoRows {
			return store.EntityRef{}, false, nil
		}
		return store.EntityRef{}, false, err
	}
	return ref, true, nil
}

// StateIntervalAt implements store.AllocationStore: the wms_intervals
// (kind='state') interval covering ts for the given entity, deliberately NOT
// scoped by agent_name (SB-2: the agent who opened the interval is not
// necessarily the one incurring the cost message at ts).
func (s *Store) StateIntervalAt(ctx context.Context, entityType, entityID string, at time.Time) (int64, bool, error) {
	const q = `
		SELECT id
		FROM wms_intervals
		WHERE kind = 'state'
		  AND entity_type = ?
		  AND entity_id = ?
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)
		ORDER BY started_at DESC
		LIMIT 1`
	var id int64
	if err := s.db.QueryRowContext(ctx, q, entityType, entityID, at, at).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	return id, true, nil
}

// ApplyAttribution implements store.AllocationStore: one atomic upsert of a
// usage_attribution row. intervalID nil means "no covering interval" (stored
// as 0, the existing sentinel).
func (s *Store) ApplyAttribution(ctx context.Context, messageID, method string, entity store.EntityRef, intervalID *int64) error {
	var ivl int64
	if intervalID != nil {
		ivl = *intervalID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_attribution
			(message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
		VALUES (?, ?, ?, 1.00000, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type = VALUES(entity_type),
			entity_id   = VALUES(entity_id),
			weight      = VALUES(weight),
			method      = VALUES(method),
			computed_at = VALUES(computed_at),
			interval_id = VALUES(interval_id)`,
		messageID, entity.EntityType, entity.EntityID, method, time.Now().UTC(), ivl)
	return err
}

// ClearUnallocatedAttribution implements store.AllocationStore: deletes every
// usage_attribution row not allocated to an entity (entity_type=''), the
// complete not-yet-really-attributed set (method='unallocated' plus the
// 'sweep_skipped' give-up marker, both entity_type=''). Predicated on
// entity_type rather than method so a row carrying a REAL entity is never
// deleted; index idx_ua_entity(entity_type, entity_id) covers the scan.
func (s *Store) ClearUnallocatedAttribution(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM usage_attribution WHERE entity_type = ''`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// BuildCostRollup implements store.AllocationStore, using AtomicReplace (04)
// instead of the non-atomic TRUNCATE-in-tx it replaces (TRUNCATE
// auto-commits in InnoDB, so that transaction wrapper was never real) — a
// correctness fix (R8), not just a port.
func (s *Store) BuildCostRollup(ctx context.Context) error {
	return s.AtomicReplace(ctx, "cost_rollup", func(ctx context.Context, into string) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO `+into+`
				(bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd)
			SELECT
				DATE(t.timestamp)                              AS bucket_day,
				DATE_FORMAT(t.timestamp, '%Y-%m-%d %H:00:00')  AS bucket_hour,
				ua.entity_type                                 AS entity_type,
				ua.entity_id                                   AS entity_id,
				t.agent_name                                   AS agent_name,
				t.model                                        AS model,
				ROUND(SUM(t.total_input * ua.weight))          AS tokens,
				SUM(t.cost_usd * ua.weight)                    AS cost_usd
			FROM token_ledger t
			JOIN usage_attribution ua ON ua.message_id = t.message_id
			GROUP BY bucket_day, bucket_hour, ua.entity_type, ua.entity_id, t.agent_name, t.model`)
		return err
	})
}

// BuildOutcomeCostRollup implements store.AllocationStore, using AtomicReplace
// per the same rationale as BuildCostRollup.
func (s *Store) BuildOutcomeCostRollup(ctx context.Context) error {
	return s.AtomicReplace(ctx, "outcome_cost_rollup", func(ctx context.Context, into string) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO `+into+`
				(bucket_day, bucket_hour, outcome_id, source_type, source_id, model, agent_name, tokens, cost_usd)
			SELECT
				cr.bucket_day,
				cr.bucket_hour,
				cr.entity_id                AS outcome_id,
				'direct'                    AS source_type,
				''                          AS source_id,
				cr.model,
				cr.agent_name,
				SUM(cr.tokens)              AS tokens,
				SUM(cr.cost_usd)            AS cost_usd
			FROM cost_rollup cr
			WHERE cr.entity_type = 'outcome'
			GROUP BY cr.bucket_day, cr.bucket_hour, cr.entity_id, cr.model, cr.agent_name

			UNION ALL

			SELECT
				cr.bucket_day,
				cr.bucket_hour,
				w.outcome_id                AS outcome_id,
				'workunit'                  AS source_type,
				cr.entity_id                AS source_id,
				cr.model,
				cr.agent_name,
				SUM(cr.tokens)              AS tokens,
				SUM(cr.cost_usd)            AS cost_usd
			FROM cost_rollup cr
			JOIN workunits w ON w.id = cr.entity_id
			WHERE cr.entity_type = 'workunit'
			GROUP BY cr.bucket_day, cr.bucket_hour, w.outcome_id, cr.entity_id, cr.model, cr.agent_name`)
		return err
	})
}

// Reconcile implements store.AllocationStore. otelCosts is the caller's
// already-fetched Prometheus session-cost map (MySQL cannot reach Prometheus
// itself) — see store.go's doc comment on this signature deviation from
// 01-interfaces.md. Preserves the GREATEST-guarded upsert: a session absent
// from the current OTel result (series aged out of retention) never has its
// previously recorded non-zero otel_cost_usd overwritten with 0.
func (s *Store) Reconcile(ctx context.Context, otelCosts map[string]float64) (int64, error) {
	ledger := map[string]float64{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, SUM(cost_usd) FROM token_ledger GROUP BY session_id`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var sid string
		var cost float64
		if err := rows.Scan(&sid, &cost); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		ledger[sid] = cost
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sessions := map[string]struct{}{}
	for sid := range otelCosts {
		sessions[sid] = struct{}{}
	}
	for sid := range ledger {
		sessions[sid] = struct{}{}
	}

	now := time.Now().UTC()
	var n int64
	for sid := range sessions {
		o := otelCosts[sid]
		l := ledger[sid]
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO session_reconciliation
				(session_id, otel_cost_usd, ledger_cost_usd, divergence_usd, computed_at)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				otel_cost_usd   = GREATEST(otel_cost_usd, VALUES(otel_cost_usd)),
				ledger_cost_usd = VALUES(ledger_cost_usd),
				divergence_usd  = GREATEST(otel_cost_usd, VALUES(otel_cost_usd)) - VALUES(ledger_cost_usd),
				computed_at     = VALUES(computed_at)`,
			sid, o, l, o-l, now,
		); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// AssembleIntervalCost implements store.AllocationStore. Truly idempotent
// (SB-3): clears every previously-assembled cost back to NULL first, then
// re-derives from source, so an interval that dropped out of the source loses
// its stale cost rather than keeping it (a plain UPDATE...JOIN would leave it
// behind).
func (s *Store) AssembleIntervalCost(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET cost_usd = NULL, cost_tokens = NULL, assembled_at = NULL
		WHERE kind = 'state'
		  AND (cost_usd IS NOT NULL OR cost_tokens IS NOT NULL OR assembled_at IS NOT NULL)`); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals wi
		JOIN (
			SELECT ua.interval_id AS interval_id,
			       SUM(t.cost_usd    * ua.weight) AS cost_usd,
			       SUM(t.total_input * ua.weight) AS cost_tokens
			FROM usage_attribution ua
			JOIN token_ledger t ON t.message_id = ua.message_id
			WHERE ua.interval_id <> 0
			GROUP BY ua.interval_id
		) x ON wi.id = x.interval_id AND wi.kind = 'state'
		SET wi.cost_usd    = x.cost_usd,
		    wi.cost_tokens = ROUND(x.cost_tokens),
		    wi.assembled_at = UTC_TIMESTAMP(6)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ReassembleIntervals implements store.AllocationStore: the opt-in historical
// backfill for cost-by-phase. Re-resolves interval_id for every
// already-attributed row that has a real entity but interval_id = 0 — a
// single set-based UPDATE with a correlated subquery doing the same
// most-recently-started covering-interval lookup as StateIntervalAt, rather
// than a Go-side per-row loop, since the resolution rule has no decision
// logic beyond that deterministic lookup. Then rebuilds interval cost.
func (s *Store) ReassembleIntervals(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		SET ua.interval_id = (
			SELECT wi.id FROM wms_intervals wi
			WHERE wi.kind = 'state' AND wi.entity_type = ua.entity_type AND wi.entity_id = ua.entity_id
			  AND wi.started_at <= t.timestamp AND (wi.ended_at IS NULL OR wi.ended_at > t.timestamp)
			ORDER BY wi.started_at DESC LIMIT 1
		)
		WHERE ua.interval_id = 0 AND ua.entity_type <> ''
		  AND EXISTS (
			SELECT 1 FROM wms_intervals wi2
			WHERE wi2.kind = 'state' AND wi2.entity_type = ua.entity_type AND wi2.entity_id = ua.entity_id
			  AND wi2.started_at <= t.timestamp AND (wi2.ended_at IS NULL OR wi2.ended_at > t.timestamp)
		  )`)
	if err != nil {
		return 0, err
	}
	updated, _ := res.RowsAffected()

	if _, err := s.AssembleIntervalCost(ctx); err != nil {
		return updated, err
	}
	return updated, nil
}
