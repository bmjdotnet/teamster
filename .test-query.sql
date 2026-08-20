SELECT
    CASE
        WHEN cr.entity_type = '' THEN '(unallocated)'
        WHEN cr.entity_type = 'outcome' THEN COALESCE(o.title, cr.entity_id)
        WHEN cr.entity_type = 'workunit' THEN COALESCE(wu.title, cr.entity_id)
        ELSE CONCAT('(legacy ', cr.entity_type, ') ', cr.entity_id)
    END AS Entity,
    CASE WHEN cr.entity_type = '' THEN '-' ELSE cr.entity_type END AS Type,
    CASE
        WHEN cr.entity_type = 'outcome' THEN o.status
        WHEN cr.entity_type = 'workunit' THEN wu.status
        ELSE '-'
    END AS Status,
    CASE
        WHEN cr.entity_type = 'outcome' THEN
            (SELECT COALESCE(SUM(otcr.cost_usd), 0)
             FROM outcome_true_cost_rollup otcr
             WHERE otcr.outcome_id = cr.entity_id
               AND otcr.bucket_hour BETWEEN NOW() - INTERVAL 30 DAY AND NOW())
        ELSE SUM(cr.cost_usd)
    END AS Cost_USD,
    CASE
        WHEN cr.entity_type = 'outcome' THEN
            (SELECT COALESCE(SUM(otcr.tokens), 0)
             FROM outcome_true_cost_rollup otcr
             WHERE otcr.outcome_id = cr.entity_id
               AND otcr.bucket_hour BETWEEN NOW() - INTERVAL 30 DAY AND NOW())
        ELSE SUM(cr.tokens)
    END AS Tokens,
    COALESCE(
      (SELECT wi.phase FROM wms_intervals wi
       WHERE wi.entity_type = cr.entity_type AND wi.entity_id = cr.entity_id
         AND wi.kind = 'state' AND wi.phase IS NOT NULL
       ORDER BY wi.started_at DESC LIMIT 1),
      (SELECT t.tag_value FROM entity_tags et JOIN tags t ON t.id = et.tag_id
       WHERE et.entity_type = cr.entity_type AND et.entity_id = cr.entity_id
         AND t.tag_key = 'phase' AND t.retired = 0
       ORDER BY et.applied_at DESC LIMIT 1)
    ) AS Phase,
    (SELECT ROUND(SUM(wi2.duration_ms) / 3600000.0, 1)
     FROM wms_intervals wi2
     WHERE wi2.entity_type = cr.entity_type AND wi2.entity_id = cr.entity_id
       AND wi2.kind = 'state' AND wi2.ended_at IS NOT NULL) AS Duration_Hours,
    (SELECT cf2.agent_name FROM cost_facts cf2
     WHERE cf2.entity_type = cr.entity_type AND cf2.entity_id = cr.entity_id
       AND cf2.agent_name != ''
     GROUP BY cf2.agent_name ORDER BY SUM(cf2.cost_usd) DESC LIMIT 1) AS Primary_Agent,
    COALESCE(
      (SELECT GROUP_CONCAT(CONCAT(t.tag_key, ':', t.tag_value) ORDER BY t.tag_key, t.tag_value SEPARATOR ', ')
       FROM entity_tags et JOIN tags t ON t.id = et.tag_id
       WHERE et.entity_type = cr.entity_type AND et.entity_id = cr.entity_id
         AND t.retired = 0
         AND t.tag_key NOT IN ('git.branch','git.repo','lifecycle','team')),
      '(untagged)'
    ) AS Tags,
    (SELECT CONCAT(orl.kind, ' ',
        CASE
            WHEN orl.to_type = 'outcome' THEN COALESCE(ot2.title, orl.to_id)
            WHEN orl.to_type = 'workunit' THEN COALESCE(wu2.title, orl.to_id)
            ELSE orl.to_id
        END)
     FROM outcome_relations orl
     LEFT JOIN outcomes ot2 ON ot2.id = orl.to_id AND orl.to_type = 'outcome'
     LEFT JOIN workunits wu2 ON wu2.id = orl.to_id AND orl.to_type = 'workunit'
     WHERE orl.from_type = cr.entity_type AND orl.from_id = cr.entity_id
     ORDER BY orl.created_at DESC LIMIT 1) AS Relates_To
FROM cost_facts cr
LEFT JOIN outcomes o ON o.id = cr.entity_id AND cr.entity_type = 'outcome'
LEFT JOIN workunits wu ON wu.id = cr.entity_id AND cr.entity_type = 'workunit'
WHERE cr.timestamp >= NOW() - INTERVAL 30 DAY
  AND (1=1 OR cr.entity_type = '$entity_type')
GROUP BY cr.entity_type, cr.entity_id
ORDER BY Cost_USD DESC
LIMIT 50