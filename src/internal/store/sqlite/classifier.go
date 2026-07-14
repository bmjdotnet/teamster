// ClassifierStore — the B4 phase/work-type classifier's read/write surface,
// ported verbatim from internal/store/mysql/store.go's classifier section.
// No dialect changes: every query here is standard SQL (correlated
// subqueries, NOT EXISTS, CROSS JOIN, tuple IN-lists) supported identically
// by SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// MarkIntervalAssembled stamps phase_assembled_at on an interval WITHOUT
// setting a phase. Scoped to non-declared rows so a declared phase is never
// disturbed.
func (s *Store) MarkIntervalAssembled(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET phase_assembled_at = ?
		WHERE id = ? AND kind = 'state' AND phase_source <> 'declared'`, nowUTC(), id)
	return err
}

// RecordJobHeartbeat upserts jobName's last-completed-run timestamp,
// independent of whether the run produced any other write.
func (s *Store) RecordJobHeartbeat(ctx context.Context, jobName string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_heartbeats (job_name, last_run_at)
		VALUES (?, ?)
		ON CONFLICT(job_name) DO UPDATE SET last_run_at = excluded.last_run_at`,
		jobName, at.UTC())
	return err
}

// ListIntervalsNeedingPhase returns closed intervals whose phase is not yet
// derived (or is stale) and is not a declared override. limit <= 0 defaults
// to 500.
func (s *Store) ListIntervalsNeedingPhase(ctx context.Context, limit int) ([]wms.EventRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, state, started_at, ended_at,
		       duration_ms, session_id, agent_name, host, phase, phase_source
		FROM wms_intervals
		WHERE kind = 'state'
		  AND ended_at IS NOT NULL
		  AND phase_source <> 'declared'
		  AND (phase_assembled_at IS NULL OR phase_assembled_at < ended_at)
		ORDER BY ended_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wms.EventRecord
	for rows.Next() {
		var r wms.EventRecord
		var endedAt sql.NullTime
		var durationMs sql.NullInt64
		var phase sql.NullString
		if err := rows.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.State, &r.StartedAt,
			&endedAt, &durationMs, &r.SessionID, &r.AgentName, &r.Host, &phase, &r.PhaseSource); err != nil {
			return nil, err
		}
		r.StartedAt = r.StartedAt.UTC()
		if endedAt.Valid {
			t := endedAt.Time.UTC()
			r.EndedAt = &t
		}
		if durationMs.Valid {
			r.DurationMs = &durationMs.Int64
		}
		if phase.Valid {
			p := phase.String
			r.Phase = &p
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearClassifierPhases resets the phase-assembly state of every interval
// the classifier has touched. Returns the number of rows reset.
func (s *Store) ClearClassifierPhases(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET phase = NULL, phase_source = '', phase_assembled_at = NULL
		WHERE kind = 'state'
		  AND (phase_source = 'classifier'
		   OR (phase_source = '' AND phase IS NULL AND phase_assembled_at IS NOT NULL))`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EarliestClosureByEntity returns, for each (entity_type, entity_id) pair in
// keys, the earliest ended_at among that entity's CLOSED review/done
// intervals. keys is the batch's distinct entities as
// [entity_type, entity_id] pairs; an empty keys returns an empty map.
func (s *Store) EarliestClosureByEntity(ctx context.Context, keys [][2]string) (map[[2]string]time.Time, error) {
	out := map[[2]string]time.Time{}
	if len(keys) == 0 {
		return out, nil
	}
	var sb strings.Builder
	sb.WriteString(`
		SELECT entity_type, entity_id, MIN(ended_at)
		FROM wms_intervals
		WHERE kind = 'state'
		  AND state IN ('review','done')
		  AND ended_at IS NOT NULL
		  AND (entity_type, entity_id) IN (`)
	args := make([]any, 0, len(keys)*2)
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?)")
		args = append(args, k[0], k[1])
	}
	sb.WriteString(`)
		GROUP BY entity_type, entity_id`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var etype, eid string
		// MIN(ended_at) is an aggregate expression, not a plain column
		// reference, so the driver cannot sniff its DATETIME decltype (see
		// sqltime.go) — scan via aggTime instead of time.Time directly.
		var firstEnd aggTime
		if err := rows.Scan(&etype, &eid, &firstEnd); err != nil {
			return nil, err
		}
		if firstEnd.Valid {
			out[[2]string{etype, eid}] = firstEnd.Time
		}
	}
	return out, rows.Err()
}

// ListWorkUnitsNeedingWorkType returns the distinct workunit ids that need a
// work-type (re)classification pass — see the mysql implementation's doc
// comment for the full watermark rationale (GH #13 follow-up).
func (s *Store) ListWorkUnitsNeedingWorkType(ctx context.Context, jobName string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wa.entity_id
		FROM (
		  SELECT entity_id, MAX(ended_at) AS latest_closed
		  FROM wms_intervals
		  WHERE kind = 'state' AND entity_type = ?
		  GROUP BY entity_id
		) wa
		WHERE NOT EXISTS (
		  SELECT 1 FROM entity_tags et
		  JOIN tags t ON t.id = et.tag_id
		  WHERE et.entity_type = 'workunit' AND et.entity_id = wa.entity_id
		    AND t.tag_key = 'work-type' AND et.source = 'manual'
		)
		AND (
		  (
		    NOT EXISTS (
		      SELECT 1 FROM entity_tags et2
		      JOIN tags t2 ON t2.id = et2.tag_id
		      WHERE et2.entity_type = 'workunit' AND et2.entity_id = wa.entity_id
		        AND t2.tag_key = 'work-type'
		    )
		    AND (
		      (SELECT last_run_at FROM job_heartbeats WHERE job_name = ?) IS NULL
		      OR (wa.latest_closed IS NOT NULL
		          AND wa.latest_closed > (SELECT last_run_at FROM job_heartbeats WHERE job_name = ?))
		    )
		  )
		  OR EXISTS (
		    SELECT 1 FROM entity_tags et3
		    JOIN tags t3 ON t3.id = et3.tag_id
		    WHERE et3.entity_type = 'workunit' AND et3.entity_id = wa.entity_id
		      AND t3.tag_key = 'work-type'
		      AND wa.latest_closed IS NOT NULL AND et3.applied_at < wa.latest_closed
		  )
		)
		ORDER BY wa.entity_id ASC`, wms.EntityWorkUnit, jobName, jobName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListOutcomesNeedingPhase returns [outcomeID, workType] pairs for outcomes
// that have no phase tag and no child workunits.
func (s *Store) ListOutcomesNeedingPhase(ctx context.Context) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id,
		  COALESCE(
		    (SELECT t.tag_value FROM entity_tags et
		     JOIN tags t ON t.id = et.tag_id
		     WHERE et.entity_type = 'outcome' AND et.entity_id = o.id
		       AND t.tag_key = 'work-type'
		     LIMIT 1),
		    ''
		  ) AS work_type
		FROM outcomes o
		WHERE NOT EXISTS (
		  SELECT 1 FROM entity_tags et
		  JOIN tags t ON t.id = et.tag_id AND t.tag_key = 'phase'
		  WHERE et.entity_type = 'outcome' AND et.entity_id = o.id
		)
		AND NOT EXISTS (
		  SELECT 1 FROM workunits wu WHERE wu.outcome_id = o.id
		)
		ORDER BY o.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}

// ListWorkUnitsNeedingLifecycleTags returns [workunitID, missingKey,
// existingWorkType] triples for work units missing at least one required
// lifecycle tag.
func (s *Store) ListWorkUnitsNeedingLifecycleTags(ctx context.Context) ([][3]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wu.id, rk.tag_key,
		  COALESCE(
		    (SELECT t2.tag_value
		     FROM entity_tags et2
		     JOIN tags t2 ON t2.id = et2.tag_id
		     WHERE et2.entity_type = 'workunit' AND et2.entity_id = wu.id
		       AND t2.tag_key = 'work-type'
		     LIMIT 1),
		    ''
		  ) AS work_type
		FROM workunits wu
		CROSS JOIN (
		  SELECT DISTINCT tag_key
		  FROM tags
		  WHERE required = 1 AND category = 'lifecycle' AND retired = 0
		) rk
		WHERE NOT EXISTS (
		  SELECT 1 FROM entity_tags et
		  JOIN tags t ON t.id = et.tag_id
		  WHERE et.entity_type = 'workunit'
		    AND et.entity_id = wu.id
		    AND t.tag_key = rk.tag_key
		)
		ORDER BY wu.id, rk.tag_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var triple [3]string
		if err := rows.Scan(&triple[0], &triple[1], &triple[2]); err != nil {
			return nil, err
		}
		out = append(out, triple)
	}
	return out, rows.Err()
}
