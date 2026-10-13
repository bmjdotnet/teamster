package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// AddRelation mirrors the mysql backend's implementation (relations.go) —
// see that file's comment for the full validation rationale
// (WP3-relations-reporting.md §4.3). Dialect differences only: INSERT IGNORE
// -> INSERT OR IGNORE; the WITH RECURSIVE queries are byte-identical (same
// precedent as AddOutcomeEdge/AddEntityDependency in store_v2.go).
func (s *Store) AddRelation(ctx context.Context, kind, fromType, fromID, toType, toID, createdBy, source, note string) error {
	toID = strings.TrimSpace(toID)

	rk, err := s.getRelationKind(ctx, kind)
	if err != nil {
		return fmt.Errorf("looking up relation kind: %w", err)
	}
	if rk == nil {
		kinds, kerr := s.ListRelationKinds(ctx)
		if kerr != nil {
			return fmt.Errorf("unknown relation kind %q", kind)
		}
		names := make([]string, 0, len(kinds))
		for _, k := range kinds {
			names = append(names, k.Kind)
		}
		return fmt.Errorf("unknown relation kind %q — valid kinds: %s", kind, strings.Join(names, ", "))
	}

	if fromType == toType && fromID == toID {
		return fmt.Errorf("self-loop: %s %s cannot relate to itself", fromType, fromID)
	}

	fromExists, err := s.entityExists(ctx, fromType, fromID)
	if err != nil {
		return fmt.Errorf("checking from entity %s %s: %w", fromType, fromID, err)
	}
	if !fromExists {
		return fmt.Errorf("from entity %s %s does not exist", fromType, fromID)
	}
	if toType == "external" {
		if toID == "" {
			return fmt.Errorf("toID must be non-empty (after trimming) when toType is \"external\"")
		}
	} else {
		toExists, err := s.entityExists(ctx, toType, toID)
		if err != nil {
			return fmt.Errorf("checking to entity %s %s: %w", toType, toID, err)
		}
		if !toExists {
			return fmt.Errorf("to entity %s %s does not exist", toType, toID)
		}
	}

	if rk.Lineage {
		cyclic, err := s.relationWouldCycle(ctx, fromType, fromID, toType, toID)
		if err != nil {
			return fmt.Errorf("cycle check: %w", err)
		}
		if cyclic {
			return fmt.Errorf("adding relation %s %s %s %s %s would create a cycle", fromType, fromID, kind, toType, toID)
		}
	}

	if rk.Taxable && fromType == wms.EntityOutcome && toType == wms.EntityOutcome {
		connected, err := s.outcomeDAGConnected(ctx, fromID, toID)
		if err != nil {
			return fmt.Errorf("parent-DAG collision check: %w", err)
		}
		if connected {
			return fmt.Errorf("taxable relation %s %s -> %s %s rejected: endpoints are already connected in the outcome decomposition DAG (rework of your own parent/child is pre-delivery iteration, not post-delivery rework)", fromType, fromID, toType, toID)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO outcome_relations
			(kind, from_type, from_id, to_type, to_id, created_at, created_by, source, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, fromType, fromID, toType, toID, nowUTC(), createdBy, source, note,
	)
	return err
}

// RemoveRelation is a hard delete keyed on the same five identity fields
// AddRelation used. Idempotent: removing a relation that does not exist
// succeeds as a no-op.
func (s *Store) RemoveRelation(ctx context.Context, kind, fromType, fromID, toType, toID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM outcome_relations
		WHERE kind = ? AND from_type = ? AND from_id = ? AND to_type = ? AND to_id = ?`,
		kind, fromType, fromID, toType, toID,
	)
	return err
}

// ListRelations returns relations touching (entityType, entityID). direction
// is "from" (entity is the new work), "to" (entity is the prior work), or
// "both"/"" (default). kind, if non-empty, filters to that kind.
func (s *Store) ListRelations(ctx context.Context, entityType, entityID, direction, kind string) ([]store.Relation, error) {
	var where string
	var args []any
	switch direction {
	case "from":
		where = `from_type = ? AND from_id = ?`
		args = append(args, entityType, entityID)
	case "to":
		where = `to_type = ? AND to_id = ?`
		args = append(args, entityType, entityID)
	default:
		where = `(from_type = ? AND from_id = ?) OR (to_type = ? AND to_id = ?)`
		args = append(args, entityType, entityID, entityType, entityID)
	}

	var sb strings.Builder
	sb.WriteString(`SELECT id, kind, from_type, from_id, to_type, to_id, created_at, created_by, source, note
		FROM outcome_relations WHERE (`)
	sb.WriteString(where)
	sb.WriteString(`)`)
	if kind != "" {
		sb.WriteString(` AND kind = ?`)
		args = append(args, kind)
	}
	sb.WriteString(` ORDER BY created_at DESC, id DESC`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.Relation, 0)
	for rows.Next() {
		var r store.Relation
		if err := rows.Scan(&r.ID, &r.Kind, &r.FromType, &r.FromID, &r.ToType, &r.ToID,
			&r.CreatedAt, &r.CreatedBy, &r.Source, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRelationKinds returns the seeded relation_kinds vocabulary.
func (s *Store) ListRelationKinds(ctx context.Context) ([]store.RelationKind, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, taxable, miss_class, lineage, is_seed, description
		FROM relation_kinds ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.RelationKind, 0)
	for rows.Next() {
		rk, taxable, lineage, isSeed := store.RelationKind{}, 0, 0, 0
		if err := rows.Scan(&rk.Kind, &taxable, &rk.MissClass, &lineage, &isSeed, &rk.Description); err != nil {
			return nil, err
		}
		rk.Taxable = taxable != 0
		rk.Lineage = lineage != 0
		rk.IsSeed = isSeed != 0
		out = append(out, rk)
	}
	return out, rows.Err()
}

// getRelationKind fetches one relation_kinds row, or (nil, nil) if kind is unknown.
func (s *Store) getRelationKind(ctx context.Context, kind string) (*store.RelationKind, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT kind, taxable, miss_class, lineage, is_seed, description
		FROM relation_kinds WHERE kind = ?`, kind)
	var rk store.RelationKind
	var taxable, lineage, isSeed int
	if err := row.Scan(&rk.Kind, &taxable, &rk.MissClass, &lineage, &isSeed, &rk.Description); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	rk.Taxable = taxable != 0
	rk.Lineage = lineage != 0
	rk.IsSeed = isSeed != 0
	return &rk, nil
}

// relationWouldCycle reports whether adding fromType/fromID <kind> toType/toID
// would close a cycle, walking only lineage=1 kinds. See the mysql backend's
// relations.go for the full explanation of the query shape.
func (s *Store) relationWouldCycle(ctx context.Context, fromType, fromID, toType, toID string) (bool, error) {
	row := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE reachable (t, id) AS (
			SELECT r.to_type, r.to_id FROM outcome_relations r
			  JOIN relation_kinds k ON k.kind = r.kind AND k.lineage = 1
			 WHERE r.from_type = ? AND r.from_id = ?
			UNION
			SELECT r.to_type, r.to_id FROM outcome_relations r
			  JOIN relation_kinds k ON k.kind = r.kind AND k.lineage = 1
			  JOIN reachable c ON r.from_type = c.t AND r.from_id = c.id
		)
		SELECT COUNT(*) FROM reachable WHERE t = ? AND id = ?`,
		toType, toID, fromType, fromID)
	var count int
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// outcomeDAGConnected reports whether aID and bID are connected in
// outcome_edges, in either direction (ancestor-descendant at any distance).
func (s *Store) outcomeDAGConnected(ctx context.Context, aID, bID string) (bool, error) {
	connected, err := s.isOutcomeAncestor(ctx, bID, aID) // is aID an ancestor of bID?
	if err != nil || connected {
		return connected, err
	}
	return s.isOutcomeAncestor(ctx, aID, bID) // is bID an ancestor of aID?
}

// isOutcomeAncestor reports whether ancestorID is an ancestor of nodeID in
// outcome_edges (parent_id -> child_id), walking parent_id upward from
// nodeID. Mirrors AddOutcomeEdge's own cycle-detection query (store_v2.go).
func (s *Store) isOutcomeAncestor(ctx context.Context, nodeID, ancestorID string) (bool, error) {
	row := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE ancestors AS (
			SELECT parent_id FROM outcome_edges WHERE child_id = ?
			UNION ALL
			SELECT oe.parent_id FROM outcome_edges oe JOIN ancestors a ON oe.child_id = a.parent_id
		)
		SELECT COUNT(*) FROM ancestors WHERE parent_id = ?`, nodeID, ancestorID)
	var count int
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// entityTable maps a WMS entity type to its backing table name.
func entityTable(entityType string) (string, error) {
	switch entityType {
	case wms.EntityOutcome:
		return "outcomes", nil
	case wms.EntityWorkUnit:
		return "workunits", nil
	default:
		return "", fmt.Errorf("unknown entity type %q", entityType)
	}
}

// entityExists reports whether an outcome or workunit with the given id exists.
func (s *Store) entityExists(ctx context.Context, entityType, entityID string) (bool, error) {
	table, err := entityTable(entityType)
	if err != nil {
		return false, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE id = ?", entityID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
