package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// scanOutcome scans a single Outcome row from the given scanner.
func scanOutcome(sc interface {
	Scan(...any) error
}, o *wms.Outcome) error {
	return sc.Scan(
		&o.ID, &o.Title, &o.Description, &o.Status, &o.PriorStatus,
		&o.Focus, &o.OriginHost, &o.OriginSession, &o.OriginAgent,
		&o.CreatedAt, &o.UpdatedAt,
	)
}

const outcomeColumns = `id, title, description, status, prior_status,
	focus, origin_host, origin_session, origin_agent, created_at, updated_at`

// scanWorkUnit scans a single WorkUnit row from the given scanner.
func scanWorkUnit(sc interface {
	Scan(...any) error
}, wu *wms.WorkUnit) error {
	return sc.Scan(
		&wu.ID, &wu.OutcomeID, &wu.Title, &wu.Description, &wu.Status, &wu.PriorStatus,
		&wu.AgentID, &wu.Focus, &wu.OriginHost, &wu.OriginSession, &wu.OriginAgent,
		&wu.CreatedAt, &wu.UpdatedAt,
	)
}

const workUnitColumns = `id, outcome_id, title, description, status, prior_status,
	agent_id, focus, origin_host, origin_session, origin_agent, created_at, updated_at`

// --- Outcome Reader ---

func (s *Store) GetOutcome(ctx context.Context, id string) (*wms.Outcome, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+outcomeColumns+`
		FROM outcomes WHERE id = ?`, id)
	var o wms.Outcome
	if err := scanOutcome(row, &o); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFound("GetOutcome", "outcome", id)
		}
		return nil, err
	}
	return &o, nil
}

func (s *Store) ListOutcomes(ctx context.Context, parentOutcomeID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error) {
	var sb strings.Builder
	args := []any{}

	sb.WriteString(`SELECT o.` + outcomeColumns + ` FROM outcomes o`)

	if parentOutcomeID == "" {
		sb.WriteString(` LEFT JOIN outcome_edges oe ON oe.child_id = o.id WHERE oe.parent_id IS NULL`)
	} else {
		sb.WriteString(` JOIN outcome_edges oe ON oe.child_id = o.id WHERE oe.parent_id = ?`)
		args = append(args, parentOutcomeID)
	}

	switch statusFilter {
	case "":
		// no filter
	case "open":
		sb.WriteString(` AND o.status != 'done'`)
	default:
		sb.WriteString(` AND o.status = ?`)
		args = append(args, statusFilter)
	}

	if query != "" {
		esc := strings.NewReplacer("%", `\%`, "_", `\_`)
		pattern := "%" + esc.Replace(query) + "%"
		sb.WriteString(` AND (o.title LIKE ? OR o.description LIKE ?)`)
		args = append(args, pattern, pattern)
	}

	if len(tagFilters) > 0 {
		sb.WriteString(` AND o.id IN (
			SELECT et.entity_id FROM entity_tags et
			JOIN tags t ON t.id = et.tag_id
			WHERE et.entity_type = 'outcome'
			AND (`)
		i := 0
		for k, v := range tagFilters {
			if i > 0 {
				sb.WriteString(` OR `)
			}
			sb.WriteString(`(t.tag_key = ? AND t.tag_value = ?)`)
			args = append(args, k, v)
			i++
		}
		sb.WriteString(fmt.Sprintf(`) GROUP BY et.entity_id HAVING COUNT(DISTINCT et.tag_id) = %d)`, len(tagFilters)))
	}

	sb.WriteString(` ORDER BY o.created_at ASC, o.id ASC`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*wms.Outcome, 0)
	for rows.Next() {
		var o wms.Outcome
		if err := scanOutcome(rows, &o); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (s *Store) GetOutcomeParents(ctx context.Context, outcomeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT parent_id FROM outcome_edges WHERE child_id = ? ORDER BY parent_id`, outcomeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) GetOutcomeChildren(ctx context.Context, outcomeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT child_id FROM outcome_edges WHERE parent_id = ? ORDER BY child_id`, outcomeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- WorkUnit Reader ---

func (s *Store) GetWorkUnit(ctx context.Context, id string) (*wms.WorkUnit, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+workUnitColumns+`
		FROM workunits WHERE id = ?`, id)
	var wu wms.WorkUnit
	if err := scanWorkUnit(row, &wu); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFound("GetWorkUnit", "workunit", id)
		}
		return nil, err
	}
	return &wu, nil
}

func (s *Store) ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+workUnitColumns+`
		FROM workunits WHERE outcome_id = ? ORDER BY created_at ASC, id ASC`, outcomeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*wms.WorkUnit
	for rows.Next() {
		var wu wms.WorkUnit
		if err := scanWorkUnit(rows, &wu); err != nil {
			return nil, err
		}
		out = append(out, &wu)
	}
	return out, rows.Err()
}

func (s *Store) ListReadyWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+workUnitColumns+`
		FROM workunits wu
		WHERE wu.outcome_id = ?
		  AND wu.status != 'done'
		  AND NOT EXISTS (
			SELECT 1 FROM entity_dependencies ed
			WHERE ed.blocked_type = 'workunit' AND ed.blocked_id = wu.id
			AND (
				(ed.blocker_type = 'workunit' AND EXISTS (
					SELECT 1 FROM workunits b WHERE b.id = ed.blocker_id AND b.status != 'done'
				))
				OR
				(ed.blocker_type = 'outcome' AND EXISTS (
					SELECT 1 FROM outcomes b WHERE b.id = ed.blocker_id AND b.status != 'done'
				))
			)
		  )
		ORDER BY wu.created_at ASC, wu.id ASC`, outcomeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*wms.WorkUnit
	for rows.Next() {
		var wu wms.WorkUnit
		if err := scanWorkUnit(rows, &wu); err != nil {
			return nil, err
		}
		out = append(out, &wu)
	}
	return out, rows.Err()
}

// --- EntityDependency Reader ---

func (s *Store) ListEntityDependencyBlockers(ctx context.Context, entityType, entityID string) ([]*wms.Dependency, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT blocker_type, blocker_id, blocked_type, blocked_id
		FROM entity_dependencies
		WHERE blocked_type = ? AND blocked_id = ?`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDependencies(rows)
}

func (s *Store) ListEntityDependencyDependents(ctx context.Context, entityType, entityID string) ([]*wms.Dependency, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT blocker_type, blocker_id, blocked_type, blocked_id
		FROM entity_dependencies
		WHERE blocker_type = ? AND blocker_id = ?`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDependencies(rows)
}

func scanDependencies(rows *sql.Rows) ([]*wms.Dependency, error) {
	var out []*wms.Dependency
	for rows.Next() {
		var d wms.Dependency
		if err := rows.Scan(&d.BlockerType, &d.BlockerID, &d.BlockedType, &d.BlockedID); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// --- Outcome Writer ---

func (s *Store) CreateOutcome(ctx context.Context, o *wms.Outcome) error {
	now := nowUTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status,
			focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.Title, o.Description, o.Status, o.PriorStatus,
		o.Focus, o.OriginHost, o.OriginSession, o.OriginAgent, now, now,
	)
	return err
}

func (s *Store) AddOutcomeEdge(ctx context.Context, parentID, childID string) error {
	if parentID == childID {
		return fmt.Errorf("self-loop: outcome %s cannot be its own parent", parentID)
	}
	// Cycle detection: if childID is an ancestor of parentID, this edge would create a cycle.
	row := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE ancestors AS (
			SELECT parent_id FROM outcome_edges WHERE child_id = ?
			UNION ALL
			SELECT oe.parent_id FROM outcome_edges oe JOIN ancestors a ON oe.child_id = a.parent_id
		)
		SELECT COUNT(*) FROM ancestors WHERE parent_id = ?`, parentID, childID)
	var count int
	if err := row.Scan(&count); err != nil {
		return fmt.Errorf("cycle check: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("adding edge %s→%s would create a cycle", parentID, childID)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO outcome_edges (parent_id, child_id) VALUES (?, ?)`, parentID, childID)
	return err
}

func (s *Store) RemoveOutcomeEdge(ctx context.Context, parentID, childID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM outcome_edges WHERE parent_id = ? AND child_id = ?`, parentID, childID)
	return err
}

func (s *Store) UpdateOutcomeStatus(ctx context.Context, id, status string) error {
	var query string
	if status == wms.StatusBlocked {
		query = `UPDATE outcomes SET prior_status = status, status = ?, updated_at = ? WHERE id = ?`
	} else {
		query = `UPDATE outcomes SET status = ?, updated_at = ? WHERE id = ?`
	}
	res, err := s.db.ExecContext(ctx, query, status, nowUTC(), id)
	if err != nil {
		return err
	}
	return requireOneRow(res, "UpdateOutcomeStatus", "outcome", id)
}

func (s *Store) UpdateOutcomeFocus(ctx context.Context, id, focus string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE outcomes SET focus = ?, updated_at = ? WHERE id = ?`, focus, nowUTC(), id)
	if err != nil {
		return err
	}
	return requireOneRow(res, "UpdateOutcomeFocus", "outcome", id)
}

// --- WorkUnit Writer ---

func (s *Store) CreateWorkUnit(ctx context.Context, wu *wms.WorkUnit) error {
	now := nowUTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workunits (id, outcome_id, title, description, status, prior_status,
			agent_id, focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wu.ID, wu.OutcomeID, wu.Title, wu.Description, wu.Status, wu.PriorStatus,
		wu.AgentID, wu.Focus, wu.OriginHost, wu.OriginSession, wu.OriginAgent, now, now,
	)
	return err
}

func (s *Store) UpdateWorkUnitStatus(ctx context.Context, id, status string) error {
	// Hard close-out enforcement (opt-in via WithRequireTagsOnDone): a workunit
	// may not reach 'done' while a required tag key is unset. Checked BEFORE the
	// UPDATE so the transition is rejected atomically. Outcomes are exempt — this
	// is UpdateWorkUnitStatus only. The MCP layer surfaces this error to the agent.
	if status == wms.StatusDone && s.requireTagsOnDone {
		missing, err := s.missingRequiredTags(ctx, id)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("workunit %s cannot be marked done: missing required tag(s): %s — apply them (e.g. wms_tagEntity) before retrying", id, strings.Join(missing, ", "))
		}
	}

	var query string
	if status == wms.StatusBlocked {
		query = `UPDATE workunits SET prior_status = status, status = ?, updated_at = ? WHERE id = ?`
	} else {
		query = `UPDATE workunits SET status = ?, updated_at = ? WHERE id = ?`
	}
	res, err := s.db.ExecContext(ctx, query, status, nowUTC(), id)
	if err != nil {
		return err
	}
	return requireOneRow(res, "UpdateWorkUnitStatus", "workunit", id)
}

// missingRequiredTags returns the required tag keys (tags.required=1, not
// retired) that have NO value bound to the given workunit. An empty result means
// every required key is satisfied. Used by the hard close-out gate.
func (s *Store) missingRequiredTags(ctx context.Context, workunitID string) ([]string, error) {
	required, err := s.ListRequiredTagKeys(ctx)
	if err != nil {
		return nil, err
	}
	if len(required) == 0 {
		return nil, nil
	}
	tags, err := s.GetEntityTags(ctx, "workunit", workunitID)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(tags))
	for _, t := range tags {
		if t.TagValue != "" {
			present[t.TagKey] = true
		}
	}
	var missing []string
	for _, key := range required {
		if !present[key] {
			missing = append(missing, key)
		}
	}
	return missing, nil
}

func (s *Store) UpdateWorkUnitFocus(ctx context.Context, id, focus string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE workunits SET focus = ?, updated_at = ? WHERE id = ?`, focus, nowUTC(), id)
	if err != nil {
		return err
	}
	return requireOneRow(res, "UpdateWorkUnitFocus", "workunit", id)
}

func (s *Store) AssignWorkUnit(ctx context.Context, id, agentID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE workunits SET agent_id = ?, updated_at = ? WHERE id = ?`, agentID, nowUTC(), id)
	if err != nil {
		return err
	}
	return requireOneRow(res, "AssignWorkUnit", "workunit", id)
}

// ClaimWorkUnit is a CAS-style guarded update (WHERE status = 'pending'): a
// zero-row result means the unit was gone or already claimed by a concurrent
// writer, not that it never existed — ErrPrecondition, not ErrNotFound.
func (s *Store) ClaimWorkUnit(ctx context.Context, id, agentID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE workunits SET agent_id = ?, status = 'active', updated_at = ?
		 WHERE id = ? AND status = 'pending'`, agentID, nowUTC(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.Precondition("ClaimWorkUnit", "workunit", id)
	}
	return nil
}

// --- EntityDependency Writer ---

func (s *Store) AddEntityDependency(ctx context.Context, dep *wms.Dependency) error {
	if dep.BlockerType == dep.BlockedType && dep.BlockerID == dep.BlockedID {
		return fmt.Errorf("self-loop: %s %s cannot block itself", dep.BlockerType, dep.BlockerID)
	}
	// Cycle detection: blocker→blocked means blocker must finish before blocked.
	// Adding it closes a cycle if blocked already (transitively) blocks blocker —
	// i.e. walking the "blocks" relation forward from blocked reaches blocker.
	// Mirrors AddOutcomeEdge; identity is the (type, id) pair on each side.
	row := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE reachable (t, id) AS (
			SELECT blocked_type, blocked_id FROM entity_dependencies
			WHERE blocker_type = ? AND blocker_id = ?
			UNION
			SELECT ed.blocked_type, ed.blocked_id FROM entity_dependencies ed
			JOIN reachable r ON ed.blocker_type = r.t AND ed.blocker_id = r.id
		)
		SELECT COUNT(*) FROM reachable WHERE t = ? AND id = ?`,
		dep.BlockedType, dep.BlockedID, dep.BlockerType, dep.BlockerID)
	var count int
	if err := row.Scan(&count); err != nil {
		return fmt.Errorf("cycle check: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("adding dependency %s %s→%s %s would create a cycle",
			dep.BlockerType, dep.BlockerID, dep.BlockedType, dep.BlockedID)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO entity_dependencies (blocker_type, blocker_id, blocked_type, blocked_id, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		dep.BlockerType, dep.BlockerID, dep.BlockedType, dep.BlockedID, nowUTC(),
	)
	return err
}

func (s *Store) RemoveEntityDependency(ctx context.Context, blockerType, blockerID, blockedType, blockedID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM entity_dependencies
		WHERE blocker_type = ? AND blocker_id = ? AND blocked_type = ? AND blocked_id = ?`,
		blockerType, blockerID, blockedType, blockedID,
	)
	return err
}
