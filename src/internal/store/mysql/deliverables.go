package mysql

import (
	"context"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// InsertDeliverable appends one row to the append-only wms_deliverables
// table (migration v69, dispatch-package). Redelivery — multiple rows for
// the same entity — is expected and allowed; ListDeliverables orders oldest
// first so a caller wanting "the" deliverable takes the LAST element.
func (s *Store) InsertDeliverable(ctx context.Context, d wms.Deliverable) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO wms_deliverables
			(entity_type, entity_id, agent_id, session_id, summary, result, artifact_paths, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.EntityType, d.EntityID, d.AgentID, d.SessionID, d.Summary, d.Result, d.ArtifactPaths, nowUTC(),
	)
	return err
}

// ListDeliverables returns an entity's deliverables, oldest first.
func (s *Store) ListDeliverables(ctx context.Context, entityType, entityID string, limit int) ([]wms.Deliverable, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, agent_id, session_id, summary, result,
		       COALESCE(artifact_paths, ''), created_at
		FROM wms_deliverables
		WHERE entity_type = ? AND entity_id = ?
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, entityType, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]wms.Deliverable, 0)
	for rows.Next() {
		var d wms.Deliverable
		if err := rows.Scan(
			&d.ID, &d.EntityType, &d.EntityID, &d.AgentID, &d.SessionID,
			&d.Summary, &d.Result, &d.ArtifactPaths, &d.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
