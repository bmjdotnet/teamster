package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// costFlowNode is a node in the Sankey diagram.
type costFlowNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
}

// costFlowLink is a directed edge in the Sankey diagram.
type costFlowLink struct {
	Source string  `json:"source"`
	Target string  `json:"target"`
	Value  float64 `json:"value"`
}

// costFlowResponse is the JSON envelope returned by /wms/api/cost-flow.
type costFlowResponse struct {
	Nodes    []costFlowNode    `json:"nodes"`
	Links    []costFlowLink    `json:"links"`
	Metadata costFlowMetadata  `json:"metadata"`
}

type costFlowMetadata struct {
	From         string  `json:"from"`
	To           string  `json:"to"`
	View         string  `json:"view"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// HandleCostFlowPage serves the Sankey HTML page.
func HandleCostFlowPage(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("cost_flow.html")
	if err != nil {
		http.Error(w, "cost_flow page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

// HandleCostFlowAPI returns an http.HandlerFunc that queries cost-flow data
// and returns Sankey-format JSON.
func HandleCostFlowAPI(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, `{"error":"WMS store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		q := r.URL.Query()
		from := q.Get("from")
		to := q.Get("to")
		view := q.Get("view")

		now := time.Now()
		if from == "" {
			from = now.AddDate(0, 0, -30).Format("2006-01-02")
		}
		if to == "" {
			to = now.Format("2006-01-02")
		}
		if view == "" {
			view = "project-phase-agent"
		}

		var (
			nodes []costFlowNode
			links []costFlowLink
			err   error
		)

		switch view {
		case "project-worktype-model":
			nodes, links, err = queryProjectWorktypeModel(r, db, from, to)
		case "entity-agent":
			nodes, links, err = queryEntityAgent(r, db, from, to)
		default:
			view = "project-phase-agent"
			nodes, links, err = queryProjectPhaseAgent(r, db, from, to)
		}

		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}

		// Compute total from links that originate at the "total" node.
		total := 0.0
		for _, l := range links {
			if l.Source == "total" {
				total += l.Value
			}
		}

		resp := costFlowResponse{
			Nodes: nodes,
			Links: links,
			Metadata: costFlowMetadata{
				From:         from,
				To:           to,
				View:         view,
				TotalCostUSD: total,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

// queryProjectPhaseAgent builds Sankey data: Total → Project → Phase → Agent.
func queryProjectPhaseAgent(r *http.Request, db *sql.DB, from, to string) ([]costFlowNode, []costFlowLink, error) {
	ctx := r.Context()

	const q = `
SELECT
    'total' AS source_id,
    'total' AS source_label,
    'total' AS source_group,
    CONCAT('project:', COALESCE(tp.tag_value, 'untagged')) AS target_id,
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
    CONCAT('project:', COALESCE(tp.tag_value, 'untagged')) AS source_id,
    COALESCE(tp.tag_value, 'untagged') AS source_label,
    'project' AS source_group,
    CONCAT('phase:', COALESCE(tph.tag_value, 'unclassified')) AS target_id,
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
    CONCAT('phase:', COALESCE(tph.tag_value, 'unclassified')) AS source_id,
    COALESCE(tph.tag_value, 'unclassified') AS source_label,
    'phase' AS source_group,
    CONCAT('agent:', COALESCE(NULLIF(cr.agent_name, ''), 'lead')) AS target_id,
    COALESCE(NULLIF(cr.agent_name, ''), 'lead') AS target_label,
    'agent' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etph ON etph.entity_type = cr.entity_type AND etph.entity_id = cr.entity_id
LEFT JOIN tags tph ON tph.id = etph.tag_id AND tph.tag_key = 'phase'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group
`
	rows, err := db.QueryContext(ctx, q, from, to, from, to, from, to)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	return buildSankeyFromRows(rows)
}

// queryProjectWorktypeModel builds Sankey data: Project → Work-Type → Model.
func queryProjectWorktypeModel(r *http.Request, db *sql.DB, from, to string) ([]costFlowNode, []costFlowLink, error) {
	ctx := r.Context()

	const q = `
SELECT
    CONCAT('project:', COALESCE(tp.tag_value, 'untagged')) AS source_id,
    COALESCE(tp.tag_value, 'untagged') AS source_label,
    'project' AS source_group,
    CONCAT('worktype:', COALESCE(tw.tag_value, 'untyped')) AS target_id,
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
    CONCAT('worktype:', COALESCE(tw.tag_value, 'untyped')) AS source_id,
    COALESCE(tw.tag_value, 'untyped') AS source_label,
    'worktype' AS source_group,
    CONCAT('model:', cr.model) AS target_id,
    cr.model AS target_label,
    'model' AS target_group,
    SUM(cr.cost_usd) AS value
FROM cost_rollup cr
LEFT JOIN entity_tags etw ON etw.entity_type = cr.entity_type AND etw.entity_id = cr.entity_id
LEFT JOIN tags tw ON tw.id = etw.tag_id AND tw.tag_key = 'work-type'
WHERE cr.bucket_day >= ? AND cr.bucket_day <= ?
GROUP BY source_id, source_label, source_group, target_id, target_label, target_group
`
	rows, err := db.QueryContext(ctx, q, from, to, from, to)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	return buildSankeyFromRows(rows)
}

// queryEntityAgent builds Sankey data: Entity → Agent.
func queryEntityAgent(r *http.Request, db *sql.DB, from, to string) ([]costFlowNode, []costFlowLink, error) {
	ctx := r.Context()

	const q = `
SELECT
    CONCAT('etype:', cr.entity_type) AS source_id,
    cr.entity_type AS source_label,
    'entitytype' AS source_group,
    CONCAT('entity:', cr.entity_id) AS target_id,
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
    CONCAT('entity:', cr.entity_id) AS source_id,
    COALESCE(o.title, wu.title, cr.entity_id) AS source_label,
    'entity' AS source_group,
    CONCAT('agent:', COALESCE(NULLIF(cr.agent_name, ''), 'lead')) AS target_id,
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
	rows, err := db.QueryContext(ctx, q, from, to, from, to)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	return buildSankeyFromRows(rows)
}

// buildSankeyFromRows reads rows of (source_id, source_label, source_group,
// target_id, target_label, target_group, value) and deduplicates nodes.
func buildSankeyFromRows(rows *sql.Rows) ([]costFlowNode, []costFlowLink, error) {
	nodeMap := map[string]costFlowNode{}
	var links []costFlowLink

	for rows.Next() {
		var (
			srcID, srcLabel, srcGroup string
			tgtID, tgtLabel, tgtGroup string
			value                     float64
		)
		if err := rows.Scan(&srcID, &srcLabel, &srcGroup, &tgtID, &tgtLabel, &tgtGroup, &value); err != nil {
			return nil, nil, err
		}
		if value <= 0 {
			continue
		}
		if _, ok := nodeMap[srcID]; !ok {
			nodeMap[srcID] = costFlowNode{ID: srcID, Label: srcLabel, Group: srcGroup}
		}
		if _, ok := nodeMap[tgtID]; !ok {
			nodeMap[tgtID] = costFlowNode{ID: tgtID, Label: tgtLabel, Group: tgtGroup}
		}
		links = append(links, costFlowLink{Source: srcID, Target: tgtID, Value: value})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	nodes := make([]costFlowNode, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}
	return nodes, links, nil
}
