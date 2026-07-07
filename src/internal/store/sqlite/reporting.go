package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.ReportingStore = (*Store)(nil)

// AttributionRate returns the total usage_attribution row count and the
// subset attributed to a real entity (method <> 'unallocated'). The
// attribution_collector divides these in Go; an empty table is a valid
// (0, 0) scrape, not an error.
func (s *Store) AttributionRate(ctx context.Context) (total, mapped int64, err error) {
	var totalN, mappedN sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(method <> 'unallocated')
		FROM usage_attribution`).Scan(&totalN, &mappedN); err != nil {
		return 0, 0, err
	}
	if totalN.Valid {
		total = int64(totalN.Float64)
	}
	if mappedN.Valid {
		mapped = int64(mappedN.Float64)
	}
	return total, mapped, nil
}

// UnattributedBacklogDepth counts token_ledger messages with no
// usage_attribution row yet — the allocator's anti-join depth.
func (s *Store) UnattributedBacklogDepth(ctx context.Context) (int64, error) {
	var depth int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM token_ledger t
		LEFT JOIN usage_attribution ua ON ua.message_id = t.message_id
		WHERE ua.message_id IS NULL`).Scan(&depth)
	return depth, err
}

// CostByEntityLast30Days sums cost_rollup per (entity_type, entity_id, model)
// over the trailing 30 days. CURDATE() - INTERVAL 30 DAY becomes SQLite's
// date('now', '-30 days').
func (s *Store) CostByEntityLast30Days(ctx context.Context) ([]store.CostRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT entity_type, entity_id, model, SUM(cost_usd)
		FROM cost_rollup
		WHERE bucket_day >= date('now', '-30 days')
		GROUP BY entity_type, entity_id, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.CostRow
	for rows.Next() {
		var r store.CostRow
		if err := rows.Scan(&r.EntityType, &r.EntityID, &r.Model, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DependencyCounts counts distinct blocker and blocked entities in
// entity_dependencies. MySQL's multi-column COUNT(DISTINCT a, b) has no
// SQLite equivalent (SQLite's COUNT(DISTINCT expr) takes exactly one
// expression) — rewritten as a COUNT(*) over a DISTINCT-projected subquery,
// which is dialect-portable and free of any separator-collision risk a
// concatenation-based rewrite would carry.
func (s *Store) DependencyCounts(ctx context.Context) (blockers, blocked int64, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM (SELECT DISTINCT blocker_type, blocker_id FROM entity_dependencies)),
			(SELECT COUNT(*) FROM (SELECT DISTINCT blocked_type, blocked_id FROM entity_dependencies))`).
		Scan(&blockers, &blocked)
	return blockers, blocked, err
}

// IntervalCostByPhase sums the conserved per-interval cost_usd on
// wms_intervals (kind='state'), grouped by (entity_type, phase). NULL phase
// collapses to "unclassified".
func (s *Store) IntervalCostByPhase(ctx context.Context) ([]store.PhaseCostRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT entity_type, COALESCE(phase,'unclassified'), SUM(cost_usd)
		FROM wms_intervals
		WHERE kind = 'state' AND cost_usd IS NOT NULL
		GROUP BY entity_type, phase`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.PhaseCostRow
	for rows.Next() {
		var r store.PhaseCostRow
		if err := rows.Scan(&r.EntityType, &r.Phase, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TagBindingCounts groups entity_tags joined to the tags vocabulary by
// (entity_type, tag_key, tag_value, category).
func (s *Store) TagBindingCounts(ctx context.Context) ([]store.TagCountRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT et.entity_type, t.tag_key, t.tag_value, t.category, COUNT(*)
		FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		GROUP BY et.entity_type, t.tag_key, t.tag_value, t.category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.TagCountRow
	for rows.Next() {
		var r store.TagCountRow
		if err := rows.Scan(&r.EntityType, &r.TagKey, &r.TagValue, &r.Category, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DailyTokenUsage aggregates token_ledger over the current UTC calendar day,
// plus a per-model token breakdown for the same window.
// DATE(CONVERT_TZ(timestamp,'UTC','UTC')) = CURDATE() becomes
// date(timestamp) = date('now') — CONVERT_TZ UTC->UTC was a no-op, and
// nowUTC() guarantees timestamp is always UTC, so date('now') (itself UTC)
// is the exact same comparison.
func (s *Store) DailyTokenUsage(ctx context.Context) (store.UsageSnapshot, error) {
	var snap store.UsageSnapshot

	row := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_write_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM token_ledger
		WHERE date(timestamp) = date('now')`)
	if err := row.Scan(
		&snap.DailyInputTokens,
		&snap.DailyOutputTokens,
		&snap.DailyCacheWrite,
		&snap.DailyCacheRead,
		&snap.DailyTotalTokens,
		&snap.DailyCostUSD,
	); err != nil {
		return store.UsageSnapshot{}, err
	}

	modelRows, err := s.db.QueryContext(ctx, `
		SELECT model,
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0)
		FROM token_ledger
		WHERE date(timestamp) = date('now')
		GROUP BY model`)
	if err != nil {
		return store.UsageSnapshot{}, err
	}
	defer modelRows.Close() //nolint:errcheck

	snap.ModelTokens = make(map[string]float64)
	for modelRows.Next() {
		var model string
		var tokens float64
		if err := modelRows.Scan(&model, &tokens); err != nil {
			return store.UsageSnapshot{}, err
		}
		snap.ModelTokens[model] = tokens
	}
	if err := modelRows.Err(); err != nil {
		return store.UsageSnapshot{}, err
	}

	return snap, nil
}

// AllTimeTokenTotals sums cost and tokens over the full token_ledger.
func (s *Store) AllTimeTokenTotals(ctx context.Context) (store.Totals, error) {
	var t store.Totals
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(cost_usd), 0),
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0)
		FROM token_ledger`).Scan(&t.CostUSD, &t.Tokens)
	return t, err
}

// TokenLedgerRows returns token_ledger rows with timestamp >= since, ordered
// ascending. This is an intentional raw-row range scan, not an aggregate —
// the usage_collector performs Go-side billing-block windowing over the
// ordered sequence.
func (s *Store) TokenLedgerRows(ctx context.Context, since time.Time) ([]store.LedgerRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT timestamp, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd
		FROM token_ledger
		WHERE timestamp >= ?
		ORDER BY timestamp ASC`,
		since.UTC(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.LedgerRow
	for rows.Next() {
		var r store.LedgerRow
		if err := rows.Scan(&r.Timestamp, &r.Input, &r.Output, &r.CacheRead, &r.CacheCreate, &r.CostUSD); err != nil {
			return nil, err
		}
		r.Timestamp = r.Timestamp.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// TagsWithEntityCounts returns every tag-vocabulary row (including retired
// ones) with its entity_tags binding count, ordered by (category, tag_key,
// tag_value).
func (s *Store) TagsWithEntityCounts(ctx context.Context) ([]store.TagWithCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.tag_key, t.tag_value, t.is_seed, t.category, t.cardinality,
		       COALESCE(t.description, ''), t.retired, t.required, t.scope,
		       t.exclusion_group, t.auto_extract, t.interview, t.facet_source,
		       COUNT(et.tag_id) AS entity_count
		FROM tags t
		LEFT JOIN entity_tags et ON et.tag_id = t.id
		GROUP BY t.id
		ORDER BY t.category, t.tag_key, t.tag_value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.TagWithCount
	for rows.Next() {
		var tc store.TagWithCount
		var isSeed, retired, required int
		if err := rows.Scan(
			&tc.Key, &tc.Value, &isSeed, &tc.Category, &tc.Cardinality,
			&tc.Description, &retired, &required, &tc.Scope,
			&tc.ExclusionGroup, &tc.AutoExtract, &tc.Interview, &tc.FacetSource,
			&tc.EntityCount,
		); err != nil {
			return nil, err
		}
		tc.IsSeed = isSeed != 0
		tc.Retired = retired != 0
		tc.Required = required != 0
		out = append(out, tc)
	}
	return out, rows.Err()
}

// costFlowSankeyRow is the common shape every CostFlowSankey view query
// projects: a directed, valued edge between a labeled/grouped source and
// target node.
type costFlowSankeyRow struct {
	srcID, srcLabel, srcGroup string
	tgtID, tgtLabel, tgtGroup string
	value                     float64
}

// CostFlowSankey builds the Sankey node/link graph for one cost-flow view
// over cost_rollup, bucketed by day between from and to (inclusive).
func (s *Store) CostFlowSankey(ctx context.Context, view string, from, to time.Time) (store.SankeyGraph, error) {
	fromStr := from.Format("2006-01-02")
	toStr := to.Format("2006-01-02")

	switch view {
	case "project-worktype-model":
		return s.costFlowProjectWorktypeModel(ctx, fromStr, toStr)
	case "entity-agent":
		return s.costFlowEntityAgent(ctx, fromStr, toStr)
	default:
		return s.costFlowProjectPhaseAgent(ctx, fromStr, toStr)
	}
}

// costFlowProjectPhaseAgent builds Sankey data: Total → Project → Phase → Agent.
// MySQL's CONCAT(a, b) becomes SQLite's a || b throughout.
func (s *Store) costFlowProjectPhaseAgent(ctx context.Context, from, to string) (store.SankeyGraph, error) {
	const q = `
SELECT
    'total' AS source_id,
    'total' AS source_label,
    'total' AS source_group,
    'project:' || COALESCE(tp.tag_value, 'untagged') AS target_id,
    COALESCE(tp.tag_value, 'untagged') AS target_label,
    'project' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etp ON etp.entity_type = cr.entity_type AND etp.entity_id = cr.entity_id
LEFT JOIN tags tp ON tp.id = etp.tag_id AND tp.tag_key = 'project'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY target_id, target_label, target_group

UNION ALL

SELECT
    'project:' || COALESCE(tp.tag_value, 'untagged') AS source_id,
    COALESCE(tp.tag_value, 'untagged') AS source_label,
    'project' AS source_group,
    'phase:' || COALESCE(tph.tag_value, 'unclassified') AS target_id,
    COALESCE(tph.tag_value, 'unclassified') AS target_label,
    'phase' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etp ON etp.entity_type = cr.entity_type AND etp.entity_id = cr.entity_id
LEFT JOIN tags tp ON tp.id = etp.tag_id AND tp.tag_key = 'project'
LEFT JOIN entity_tags etph ON etph.entity_type = cr.entity_type AND etph.entity_id = cr.entity_id
LEFT JOIN tags tph ON tph.id = etph.tag_id AND tph.tag_key = 'phase'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group

UNION ALL

SELECT
    'phase:' || COALESCE(tph.tag_value, 'unclassified') AS source_id,
    COALESCE(tph.tag_value, 'unclassified') AS source_label,
    'phase' AS source_group,
    'agent:' || COALESCE(NULLIF(cr.agent_name, ''), 'lead') AS target_id,
    COALESCE(NULLIF(cr.agent_name, ''), 'lead') AS target_label,
    'agent' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etph ON etph.entity_type = cr.entity_type AND etph.entity_id = cr.entity_id
LEFT JOIN tags tph ON tph.id = etph.tag_id AND tph.tag_key = 'phase'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group
`
	rows, err := s.db.QueryContext(ctx, q, from, to, from, to, from, to)
	if err != nil {
		return store.SankeyGraph{}, err
	}
	defer rows.Close() //nolint:errcheck
	return buildSankeyFromRows(rows)
}

// costFlowProjectWorktypeModel builds Sankey data: Project → Work-Type → Model.
func (s *Store) costFlowProjectWorktypeModel(ctx context.Context, from, to string) (store.SankeyGraph, error) {
	const q = `
SELECT
    'project:' || COALESCE(tp.tag_value, 'untagged') AS source_id,
    COALESCE(tp.tag_value, 'untagged') AS source_label,
    'project' AS source_group,
    'worktype:' || COALESCE(tw.tag_value, 'untyped') AS target_id,
    COALESCE(tw.tag_value, 'untyped') AS target_label,
    'worktype' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etp ON etp.entity_type = cr.entity_type AND etp.entity_id = cr.entity_id
LEFT JOIN tags tp ON tp.id = etp.tag_id AND tp.tag_key = 'project'
LEFT JOIN entity_tags etw ON etw.entity_type = cr.entity_type AND etw.entity_id = cr.entity_id
LEFT JOIN tags tw ON tw.id = etw.tag_id AND tw.tag_key = 'work-type'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group

UNION ALL

SELECT
    'worktype:' || COALESCE(tw.tag_value, 'untyped') AS source_id,
    COALESCE(tw.tag_value, 'untyped') AS source_label,
    'worktype' AS source_group,
    'model:' || cr.model AS target_id,
    cr.model AS target_label,
    'model' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etw ON etw.entity_type = cr.entity_type AND etw.entity_id = cr.entity_id
LEFT JOIN tags tw ON tw.id = etw.tag_id AND tw.tag_key = 'work-type'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group
`
	rows, err := s.db.QueryContext(ctx, q, from, to, from, to)
	if err != nil {
		return store.SankeyGraph{}, err
	}
	defer rows.Close() //nolint:errcheck
	return buildSankeyFromRows(rows)
}

// costFlowEntityAgent builds Sankey data: Entity → Agent.
func (s *Store) costFlowEntityAgent(ctx context.Context, from, to string) (store.SankeyGraph, error) {
	const q = `
SELECT
    'etype:' || cr.entity_type AS source_id,
    cr.entity_type AS source_label,
    'entitytype' AS source_group,
    'entity:' || cr.entity_id AS target_id,
    COALESCE(o.title, wu.title, cr.entity_id) AS target_label,
    'entity' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN outcomes o ON o.id = cr.entity_id AND cr.entity_type = 'outcome'
LEFT JOIN workunits wu ON wu.id = cr.entity_id AND cr.entity_type = 'workunit'
WHERE cr.entity_type != ''
  AND cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group

UNION ALL

SELECT
    'entity:' || cr.entity_id AS source_id,
    COALESCE(o.title, wu.title, cr.entity_id) AS source_label,
    'entity' AS source_group,
    'agent:' || COALESCE(NULLIF(cr.agent_name, ''), 'lead') AS target_id,
    COALESCE(NULLIF(cr.agent_name, ''), 'lead') AS target_label,
    'agent' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN outcomes o ON o.id = cr.entity_id AND cr.entity_type = 'outcome'
LEFT JOIN workunits wu ON wu.id = cr.entity_id AND cr.entity_type = 'workunit'
WHERE cr.entity_type != ''
  AND cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group
`
	rows, err := s.db.QueryContext(ctx, q, from, to, from, to)
	if err != nil {
		return store.SankeyGraph{}, err
	}
	defer rows.Close() //nolint:errcheck
	return buildSankeyFromRows(rows)
}

// buildSankeyFromRows reads rows of (source_id, source_label, source_group,
// target_id, target_label, target_group, value) and deduplicates nodes.
func buildSankeyFromRows(rows *sql.Rows) (store.SankeyGraph, error) {
	nodeMap := map[string]store.SankeyNode{}
	var links []store.SankeyLink

	for rows.Next() {
		var r costFlowSankeyRow
		if err := rows.Scan(&r.srcID, &r.srcLabel, &r.srcGroup, &r.tgtID, &r.tgtLabel, &r.tgtGroup, &r.value); err != nil {
			return store.SankeyGraph{}, err
		}
		if r.value <= 0 {
			continue
		}
		if _, ok := nodeMap[r.srcID]; !ok {
			nodeMap[r.srcID] = store.SankeyNode{ID: r.srcID, Label: r.srcLabel, Group: r.srcGroup}
		}
		if _, ok := nodeMap[r.tgtID]; !ok {
			nodeMap[r.tgtID] = store.SankeyNode{ID: r.tgtID, Label: r.tgtLabel, Group: r.tgtGroup}
		}
		links = append(links, store.SankeyLink{Source: r.srcID, Target: r.tgtID, Value: r.value})
	}
	if err := rows.Err(); err != nil {
		return store.SankeyGraph{}, err
	}

	nodes := make([]store.SankeyNode, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}
	return store.SankeyGraph{Nodes: nodes, Links: links}, nil
}

// WMSTree loads the outcome/workunit forest for the /wms dashboard. When
// rootOutcomeID is "", every dangling (parentless) outcome is a root;
// otherwise the tree is scoped to that single outcome's subtree. Returns a
// zero-value WMSTreeData (not an error) when the v2 tables don't exist yet.
func (s *Store) WMSTree(ctx context.Context, rootOutcomeID string) (store.WMSTreeData, error) {
	var (
		rootRows *sql.Rows
		err      error
	)
	if rootOutcomeID == "" {
		rootRows, err = s.db.QueryContext(ctx, `
			SELECT o.id, o.title, COALESCE(o.description,''), COALESCE(o.status,''),
			       COALESCE(o.focus,'')
			FROM outcomes o
			WHERE NOT EXISTS (SELECT 1 FROM outcome_edges oe WHERE oe.child_id = o.id)
			ORDER BY o.created_at ASC`)
	} else {
		rootRows, err = s.db.QueryContext(ctx, `
			SELECT o.id, o.title, COALESCE(o.description,''), COALESCE(o.status,''),
			       COALESCE(o.focus,'')
			FROM outcomes o
			WHERE o.id = ?`, rootOutcomeID)
	}
	if err != nil {
		// v2 tables may not exist on older installs — treat as empty.
		return store.WMSTreeData{}, nil //nolint:nilerr
	}
	defer rootRows.Close() //nolint:errcheck

	var roots []store.WMSTreeOutcome
	for rootRows.Next() {
		var o store.WMSTreeOutcome
		if err := rootRows.Scan(&o.ID, &o.Title, &o.Description, &o.Status, &o.Focus); err != nil {
			return store.WMSTreeData{}, err
		}
		roots = append(roots, o)
	}
	rootRows.Close()
	if err := rootRows.Err(); err != nil {
		return store.WMSTreeData{}, err
	}

	for i := range roots {
		if err := s.populateWMSOutcome(ctx, &roots[i]); err != nil {
			return store.WMSTreeData{}, err
		}
	}
	return store.WMSTreeData{Outcomes: roots}, nil
}

// populateWMSOutcome fills Children and WorkUnits for a single outcome node.
func (s *Store) populateWMSOutcome(ctx context.Context, o *store.WMSTreeOutcome) error {
	childRows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.title, COALESCE(o.description,''), COALESCE(o.status,''),
		       COALESCE(o.focus,'')
		FROM outcomes o
		JOIN outcome_edges oe ON oe.child_id = o.id
		WHERE oe.parent_id = ?
		ORDER BY o.created_at ASC`, o.ID)
	if err != nil {
		return fmt.Errorf("children of %s: %w", o.ID, err)
	}
	defer childRows.Close() //nolint:errcheck
	var children []store.WMSTreeOutcome
	for childRows.Next() {
		var c store.WMSTreeOutcome
		if err := childRows.Scan(&c.ID, &c.Title, &c.Description, &c.Status, &c.Focus); err != nil {
			return err
		}
		children = append(children, c)
	}
	childRows.Close()
	if err := childRows.Err(); err != nil {
		return err
	}
	for i := range children {
		if err := s.populateWMSOutcome(ctx, &children[i]); err != nil {
			return err
		}
	}
	o.Children = children

	wuRows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(outcome_id,''), title, COALESCE(description,''),
		       COALESCE(status,''), COALESCE(agent_id,''), COALESCE(focus,'')
		FROM workunits WHERE outcome_id = ? ORDER BY created_at ASC`, o.ID)
	if err != nil {
		return fmt.Errorf("workunits for %s: %w", o.ID, err)
	}
	defer wuRows.Close() //nolint:errcheck
	var wus []store.WMSTreeWorkUnit
	for wuRows.Next() {
		var wu store.WMSTreeWorkUnit
		if err := wuRows.Scan(&wu.ID, &wu.OutcomeID, &wu.Title, &wu.Description,
			&wu.Status, &wu.AgentID, &wu.Focus); err != nil {
			return err
		}
		wus = append(wus, wu)
	}
	wuRows.Close()
	if err := wuRows.Err(); err != nil {
		return err
	}
	o.WorkUnits = wus
	return nil
}
