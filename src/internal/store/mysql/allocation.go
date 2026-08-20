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

// BuildOutcomeTrueCostRollup implements store.AllocationStore (WP3 stage 2:
// see docs/../WP3-relations-reporting.md §5.3). It rebuilds
// outcome_true_cost_rollup via AtomicReplace, same as BuildOutcomeCostRollup.
//
// Stage-2 form: anchor leg (every outcome is its own root) + leg 1 (DAG
// descent over outcome_edges, parent→child) + leg 2 (rework: anything that
// transitively reworked anything already in the closure, over
// outcome_relations joined to relation_kinds so only taxable,
// entity-resolvable kinds traverse — non-taxable kinds like
// discovered-during and duplicate-of never extend the closure).
//
// The recursive CTE's non-numeric anchor columns (reason, rel_kind, miss,
// path) MUST be explicit CASTs — MySQL types a recursive CTE column from the
// anchor row alone, so an un-CASTed 'direct' (6 chars) types the column
// CHAR(6) and the recursive legs' wider values ('child_outcome' at 13 chars,
// a rel_kind like 'addresses-limitation' at 21) then overflow with error
// 1406. Reproduced, not theorized — see §8.1(a).
//
// Cycle guard is the accumulated path breadcrumb + NOT LIKE, not NOT EXISTS —
// MySQL forbids referencing a recursive CTE inside a subquery (§8.1(b)). The
// depth < 20 cap bounds the blast radius if path ever truncated. Leg 2 has
// its own independent path check (r.from_id against the same breadcrumb) so
// a rework cycle reachable through the closure is bounded the same way the
// DAG leg is.
//
// Both legs' NOT LIKE patterns carry an explicit COLLATE utf8mb4_general_ci:
// path's CAST(... AS CHAR(4000)) takes the connection's default collation (a
// MySQL CAST-to-CHAR quirk, distinct from §8.1's width bug), which does not
// necessarily match outcome_edges.child_id's / outcome_relations.from_id's
// own column collation — MariaDB and MySQL 8.4 default utf8mb4 columns to
// different collations (utf8mb4_general_ci vs utf8mb4_0900_ai_ci), and this
// repo installs either (see --store-engine). Reproduced on the mysql-8.4
// test harness: "Illegal mix of collations" on the bare comparison. An
// explicit COLLATE on one side always wins over the other's implicit
// collation, so this is safe regardless of which collation either table
// carries — we are doing plain substring containment on ASCII kebab-case
// entity IDs, where general_ci vs 0900_ai_ci make no matching difference.
//
// The `nodes` CTE collapses to one row per (anchor, node): an outcome
// reachable via multiple paths (DAG diamond, rework fan-in, or a mix of the
// two) is still counted once. This is the double-counting defence — see
// §8.2 (measured 2x error without it). is_self is tested first in the final
// CASE, so an anchor's own direct/workunit rows (source_type/source_id
// passed through from outcome_cost_rollup) survive unrelabeled even if some
// other path also reaches the anchor. A non-anchor node reachable BOTH as a
// DAG descendant and via a rework edge is labelled child_outcome, not
// rework — via_rework is guarded with `AND is_child = 0` in the final CASE
// — matching §5.3's prose and §11's mixed-diamond acceptance test: the
// conservative direction under-reports the tax rather than inflating it,
// same reasoning as the is_self priority. rel_kind/miss are aggregated via
// SUBSTRING_INDEX(GROUP_CONCAT(... ORDER BY depth), ',', 1): GROUP_CONCAT
// skips NULLs (the CASE inside it yields NULL for non-rework closure rows),
// so this picks the taxable kind/miss_class from the lowest-depth rework hop
// that reached this node, and both aggregates come back NULL for a node
// reached only via the anchor/DAG legs — COALESCE'd to '' in the final
// SELECT, matching stage 1's empty-column behavior when no rework rows
// apply to a given node.
//
// The final SELECT's computed columns are aliased out_source_type/
// out_source_id, not source_type/source_id — outcome_cost_rollup (joined as
// ocr) already has REAL columns named source_type/source_id, and MySQL's
// GROUP BY name resolution prefers a same-named FROM-list column over a
// SELECT-list alias. Grouping by "source_type" therefore silently grouped by
// ocr.source_type (the pre-CASE, per-child raw value) instead of the
// intended post-CASE collapse, and MySQL's own only_full_group_by check then
// correctly rejected the query over the resulting non-functionally-dependent
// n.is_self reference. Reproduced against mysql-8.4, not theorized — the
// alias collision is present verbatim in WP3-relations-reporting.md §5.3's
// own reference query too. relation_kind/miss_class need no such rename:
// neither `nodes` nor outcome_cost_rollup has a real column by either name,
// so the GROUP BY can reference the SELECT-list alias directly.
func (s *Store) BuildOutcomeTrueCostRollup(ctx context.Context) error {
	return s.AtomicReplace(ctx, "outcome_true_cost_rollup", func(ctx context.Context, into string) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO `+into+`
				(bucket_day, bucket_hour, outcome_id, source_type, source_id,
				 relation_kind, miss_class, depth, model, agent_name, tokens, cost_usd)
			WITH RECURSIVE closure (anchor, node, reason, rel_kind, miss, depth, path) AS (
				SELECT o.id,
				       o.id,
				       CAST('direct' AS CHAR(16)),
				       CAST('' AS CHAR(32)),
				       CAST('' AS CHAR(16)),
				       0,
				       CAST(CONCAT('/', o.id, '/') AS CHAR(4000))
				  FROM outcomes o
				UNION ALL
				SELECT c.anchor, oe.child_id,
				       CAST('child_outcome' AS CHAR(16)), c.rel_kind, c.miss,
				       c.depth + 1, CONCAT(c.path, oe.child_id, '/')
				  FROM closure c
				  JOIN outcome_edges oe ON oe.parent_id = c.node
				 WHERE c.depth < 20
				   AND c.path NOT LIKE CONCAT('%/', oe.child_id, '/%') COLLATE utf8mb4_general_ci
				UNION ALL
				SELECT c.anchor, r.from_id,
				       CAST('rework' AS CHAR(16)), r.kind, k.miss_class,
				       c.depth + 1, CONCAT(c.path, r.from_id, '/')
				  FROM closure c
				  JOIN outcome_relations r
				    ON r.to_type = 'outcome' AND r.to_id = c.node AND r.from_type = 'outcome'
				  JOIN relation_kinds k ON k.kind = r.kind AND k.taxable = 1
				 WHERE c.depth < 20
				   AND c.path NOT LIKE CONCAT('%/', r.from_id, '/%') COLLATE utf8mb4_general_ci
			),
			nodes AS (
				SELECT anchor, node,
				       MIN(depth) AS depth,
				       MAX(reason = 'direct')        AS is_self,
				       MAX(reason = 'child_outcome') AS is_child,
				       MAX(reason = 'rework')        AS via_rework,
				       SUBSTRING_INDEX(GROUP_CONCAT(CASE WHEN reason = 'rework' THEN rel_kind END
				                       ORDER BY depth), ',', 1) AS rel_kind,
				       SUBSTRING_INDEX(GROUP_CONCAT(CASE WHEN reason = 'rework' THEN miss END
				                       ORDER BY depth), ',', 1) AS miss
				  FROM closure
				 GROUP BY anchor, node
			)
			SELECT
				ocr.bucket_day, ocr.bucket_hour,
				n.anchor AS outcome_id,
				CASE WHEN n.is_self = 1                       THEN ocr.source_type
				     WHEN n.via_rework = 1 AND n.is_child = 0 THEN 'rework'
				     ELSE 'child_outcome' END                                        AS out_source_type,
				CASE WHEN n.is_self = 1 THEN ocr.source_id ELSE n.node END           AS out_source_id,
				COALESCE(n.rel_kind, '')                                             AS relation_kind,
				COALESCE(n.miss, '')                                                 AS miss_class,
				n.depth,
				ocr.model, ocr.agent_name,
				SUM(ocr.tokens), SUM(ocr.cost_usd)
			FROM nodes n
			JOIN outcome_cost_rollup ocr ON ocr.outcome_id = n.node
			GROUP BY ocr.bucket_day, ocr.bucket_hour, n.anchor, out_source_type, out_source_id,
			         relation_kind, miss_class, n.depth, ocr.model, ocr.agent_name`)
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
