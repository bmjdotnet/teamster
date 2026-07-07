// RoleAllowed, journal entries, tag vocabulary (wms.Reader/Writer halves),
// and event records (wms_intervals kind='state' rows) — ported from
// internal/store/mysql/store.go. See that file for the behavioral contract;
// dialect notes below mark every place this file diverges from it.
//
// Dialect notes:
//   - MySQL's "INSERT ... ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)"
//     trick (recovering an existing row's id via Result.LastInsertId() even
//     on the UPDATE branch) has no SQLite equivalent; TagEntity/DefineTag
//     instead do "INSERT ... ON CONFLICT ... DO NOTHING/DO UPDATE" followed
//     by a plain SELECT to resolve the id. Outcome-equivalent, not
//     mechanism-identical.
//   - DeleteEntityTag's MySQL multi-table "DELETE et FROM entity_tags et JOIN
//     tags t ..." has no SQLite equivalent (SQLite DELETE has no JOIN); it
//     becomes a DELETE ... WHERE tag_id IN (subquery).
//   - TransitionEventRecord/OpenEventRecord drop MySQL's GET_LOCK-based
//     withStateLock advisory lock and the SELECT ... FOR UPDATE row lock:
//     with the Store's connection pool pinned to one connection (store.go's
//     New), a BeginTx here already holds the only connection for its
//     duration, so any concurrent caller simply blocks in the pool until
//     Commit/Rollback — the same "no lost updates, no duplicate opens"
//     outcome MySQL gets from FOR UPDATE + the named lock, via coarser
//     whole-connection serialization instead of row-level locking
//     (07-conformance.md: contract is the outcome, not the mechanism).
//   - closeOpenStateIntervals translates TIMESTAMPDIFF(MICROSECOND, a, b)/1000
//     to CAST((julianday(b) - julianday(a)) * 86400000 AS INTEGER).
//   - SearchTags' LIKE clauses add "ESCAPE '\'": MySQL's LIKE defaults to '\'
//     as its escape character, but SQLite's LIKE has NO escape character
//     unless one is declared — the pattern strings here are pre-escaped with
//     backslash (searchLikePattern/SearchTags' own esc replacer), so the
//     clause must say so explicitly or the backslashes leak through as
//     literal characters instead of escaping % and _.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// --- RoleAllowed ---

// RoleAllowed checks whether role may make the transition entityType:oldStatus→newStatus.
// If transition_rules is empty, all transitions are allowed (backward-compatible).
// Otherwise, the role must match an explicit row or a wildcard ('*') row.
func (s *Store) RoleAllowed(ctx context.Context, entityType, oldStatus, newStatus, role string) (bool, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transition_rules`).Scan(&total); err != nil {
		return false, err
	}
	if total == 0 {
		return true, nil
	}

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM transition_rules
		WHERE entity_type = ? AND old_status = ? AND new_status = ?
		  AND (required_role = ? OR required_role = '*')`,
		entityType, oldStatus, newStatus, role,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// --- Journal ---

func (s *Store) GetJournalEntries(ctx context.Context, entityType, entityID string, limit int) ([]wms.JournalEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, field,
		       COALESCE(old_value, ''), COALESCE(new_value, ''),
		       COALESCE(agent_id, ''), COALESCE(host, ''),
		       COALESCE(session_id, ''), COALESCE(notes, ''),
		       created_at
		FROM wms_journal
		WHERE entity_type = ? AND entity_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, entityType, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wms.JournalEntry
	for rows.Next() {
		var e wms.JournalEntry
		if err := rows.Scan(
			&e.ID, &e.EntityType, &e.EntityID, &e.Field,
			&e.OldValue, &e.NewValue,
			&e.AgentID, &e.Host, &e.SessionID, &e.Notes,
			&e.CreatedAt,
		); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) WriteJournalEntry(ctx context.Context, entry wms.JournalEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO wms_journal
			(entity_type, entity_id, field, old_value, new_value,
			 agent_id, host, session_id, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.EntityType, entry.EntityID, entry.Field,
		entry.OldValue, entry.NewValue,
		entry.AgentID, entry.Host, entry.SessionID, entry.Notes,
	)
	return err
}

// --- Event Records ---

// OpenEventRecord inserts a fresh open kind='state' interval. MySQL wraps
// this in withStateLock (a GET_LOCK advisory lock) before the tx; on SQLite
// the pinned single connection already serializes concurrent callers at the
// BeginTx step, so no separate lock is needed (see file doc comment).
func (s *Store) OpenEventRecord(ctx context.Context, entityType, entityID, state, sessionID, agentName, host string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// now captured after BeginTx — see interval.go's OpenFocusInterval doc
	// comment for why this ordering matters under contention.
	now := nowUTC()
	if err := openStateInterval(ctx, tx, entityType, entityID, state, sessionID, agentName, host, now); err != nil {
		return err
	}
	return tx.Commit()
}

// openStateInterval inserts a kind='state' wms_intervals row for an EventRecord
// open. identity_source is "direct" when identity is present, otherwise empty.
func openStateInterval(ctx context.Context, tx *sql.Tx, entityType, entityID, state, sessionID, agentName, host string, at time.Time) error {
	idSource := ""
	if sessionID != "" {
		idSource = "direct"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, started_at, session_id, agent_name, host, identity_source)
		VALUES ('state', ?, ?, ?, ?, ?, ?, ?, ?)`,
		entityType, entityID, state, at, sessionID, agentName, host, idSource)
	return err
}

func (s *Store) GetOpenEventRecord(ctx context.Context, entityType, entityID string) (*wms.EventRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, entity_type, entity_id, state, started_at, ended_at,
		       duration_ms, session_id, agent_name, host, phase, phase_source
		FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		ORDER BY started_at DESC
		LIMIT 1`, entityType, entityID)
	var r wms.EventRecord
	var endedAt sql.NullTime
	var durationMs sql.NullInt64
	var phase sql.NullString
	if err := row.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.State, &r.StartedAt,
		&endedAt, &durationMs, &r.SessionID, &r.AgentName, &r.Host, &phase, &r.PhaseSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFound("GetOpenEventRecord", entityType, entityID)
		}
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
	return &r, nil
}

func (s *Store) ListEventRecords(ctx context.Context, entityType, entityID string, limit int) ([]wms.EventRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, state, started_at, ended_at,
		       duration_ms, session_id, agent_name, host, phase, phase_source
		FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ?
		ORDER BY started_at DESC
		LIMIT ?`, entityType, entityID, limit)
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

// TransitionEventRecord closes the currently-open kind='state' interval(s)
// for the entity and opens a new one in the target state, keeping the
// status-cache column on outcomes/workunits in lockstep — all in one tx.
// MySQL wraps this in withStateLock + SELECT ... FOR UPDATE; on SQLite the
// tx itself (against the pinned single connection) provides the same
// serialization (see file doc comment), so both are simply dropped.
func (s *Store) TransitionEventRecord(ctx context.Context, entityType, entityID, newState, sessionID, agentName, host string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	now := nowUTC()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, state, started_at FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		ORDER BY started_at DESC`, entityType, entityID)
	if err != nil {
		return fmt.Errorf("read open records: %w", err)
	}

	type openRec struct {
		id        int64
		state     string
		startedAt time.Time
	}
	var open []openRec
	for rows.Next() {
		var r openRec
		if err := rows.Scan(&r.id, &r.state, &r.startedAt); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		open = append(open, r)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return err
	}

	if len(open) == 0 {
		if err := openStateInterval(ctx, tx, entityType, entityID, newState, sessionID, agentName, host, now); err != nil {
			return fmt.Errorf("open state interval (no prior): %w", err)
		}
		table, tErr := statusTableName(entityType)
		if tErr != nil {
			return tErr
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET status = ?, updated_at = ? WHERE id = ?`,
			newState, now, entityID); err != nil {
			return fmt.Errorf("update status cache: %w", err)
		}
		return tx.Commit()
	}

	if len(open) > 1 {
		slog.Warn("wms: double-open detected",
			"entity_type", entityType, "entity_id", entityID, "count", len(open))
		for i := 1; i < len(open); i++ {
			closeAt := open[0].startedAt
			dur := closeAt.Sub(open[i].startedAt).Milliseconds()
			// closeAt can coincide with another row's existing ended_at for this
			// entity (two concurrent transitions closing within the same
			// instant), colliding on uq_open (entity_type, entity_id, kind,
			// ended_at). Map it to store.ErrConflict like every other uq_open
			// collision so a caller can retry rather than see a raw driver error.
			if _, err := tx.ExecContext(ctx,
				`UPDATE wms_intervals SET ended_at = ?, duration_ms = ? WHERE id = ?`,
				closeAt, dur, open[i].id); err != nil {
				return classifyConflict("TransitionEventRecord", fmt.Errorf("close stale record %d: %w", open[i].id, err))
			}
		}
	}

	current := open[0]

	if current.state == newState {
		return tx.Commit()
	}

	// Close the current open row and open the new one. closeOpenStateIntervals
	// closes EVERY remaining open kind='state' row for this entity at `now`.
	if err := closeOpenStateIntervals(ctx, tx, entityType, entityID, now); err != nil {
		return fmt.Errorf("close state intervals: %w", err)
	}
	if err := openStateInterval(ctx, tx, entityType, entityID, newState, sessionID, agentName, host, now); err != nil {
		return fmt.Errorf("open state interval: %w", err)
	}

	table, tErr := statusTableName(entityType)
	if tErr != nil {
		return tErr
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET status = ?, updated_at = ? WHERE id = ?`,
		newState, now, entityID); err != nil {
		return fmt.Errorf("update status cache: %w", err)
	}

	return tx.Commit()
}

// closeOpenStateIntervals closes every open kind='state' wms_intervals row for
// the entity at `at`, computing duration_ms from started_at. Dialect note:
// TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000 becomes the julianday-based
// expression below (§ SQLite dialect translation rules).
//
// ORDERING-SAFE: the AND started_at <= ? guard ensures a close whose timestamp
// predates an interval's own start is ignored — preventing negative-width
// state intervals.
func closeOpenStateIntervals(ctx context.Context, tx *sql.Tx, entityType, entityID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		at, at, entityType, entityID, at)
	return err
}

// UpdateEventRecordPhase sets the phase classification on one interval row,
// enforcing declared-wins precedence in the WHERE clause. No dialect changes
// from MySQL.
func (s *Store) UpdateEventRecordPhase(ctx context.Context, id int64, phase, source string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET phase = ?, phase_source = ?, phase_assembled_at = ?
		WHERE id = ? AND kind = 'state' AND (phase_source <> 'declared' OR ? = 'declared')`,
		phase, source, nowUTC(), id, source)
	return err
}

// --- Tags ---

// TagEntity applies a key:value tag to an entity — see mysql's TagEntity doc
// comment for the full behavioral contract (cardinality guard, description
// ordering, category/cardinality inheritance). Dialect note: the tags upsert
// uses INSERT ... ON CONFLICT ... DO NOTHING followed by a SELECT to resolve
// the id, since SQLite has no equivalent of MySQL's
// "ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)" trick.
func (s *Store) TagEntity(ctx context.Context, entityType, entityID, tagKey, tagValue, source, description string) error {
	if err := validTagEntityType(entityType); err != nil {
		return err
	}
	if tagKey == "" || tagValue == "" {
		return fmt.Errorf("tagKey and tagValue are required")
	}
	// Reject a 'phase' tag on an interval: phase lives in the wms_intervals
	// column (set via UpdateEventRecordPhase), not the tag vocabulary.
	if entityType == wms.EntityInterval && tagKey == "phase" {
		return fmt.Errorf("phase on an interval is column-only; use wms_setPhase, not a phase tag")
	}
	if err := checkTagDescriptionLen(description); err != nil {
		return err
	}
	if source == "" {
		source = "manual"
	}
	// Resolve the key's cardinality at KEY grain BEFORE upserting the value row
	// (see mysql's TagEntity doc comment for why).
	cardinality := "multi"
	var found string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT cardinality FROM tags WHERE tag_key = ? AND cardinality = 'single' LIMIT 1`, tagKey,
	).Scan(&found); {
	case err == nil:
		cardinality = "single"
	case errors.Is(err, sql.ErrNoRows):
		// key is multi-value (or new) — leave cardinality as 'multi'
	default:
		return err
	}
	// Resolve the key's category at KEY grain BEFORE upserting the value row.
	category := "context"
	var foundCat string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT category FROM tags WHERE tag_key = ? AND category != 'context' LIMIT 1`, tagKey,
	).Scan(&foundCat); {
	case err == nil:
		category = foundCat
	case errors.Is(err, sql.ErrNoRows):
		// key is new or all-context — default 'context'
	default:
		return err
	}
	// Upsert the value row (non-seed when newly created), stamping it with the
	// key's cardinality and category. DO NOTHING leaves an existing tag's
	// category/cardinality/is_seed untouched, mirroring MySQL's ON DUPLICATE
	// KEY UPDATE id = LAST_INSERT_ID(id) (a no-op update solely to recover id).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES (?, ?, 0, ?, ?, ?)
		 ON CONFLICT(tag_key, tag_value) DO NOTHING`,
		tagKey, tagValue, category, cardinality, description,
	); err != nil {
		return err
	}
	var tagID int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE tag_key = ? AND tag_value = ?`, tagKey, tagValue,
	).Scan(&tagID); err != nil {
		return err
	}
	// Description ordering (§4): backfill a description onto an existing tag
	// only when the caller supplies one AND the stored value is still empty.
	if description != "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET description = ? WHERE id = ? AND (description IS NULL OR description = '')`,
			description, tagID,
		); err != nil {
			return err
		}
	}

	if cardinality == "single" {
		// Single-value: replace any other value of this key on the entity, then
		// bind the new value — one transaction so the key is never left without
		// a value.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity_tags
			 WHERE entity_type = ? AND entity_id = ? AND tag_id IN (
			     SELECT id FROM tags WHERE tag_key = ? AND id <> ?
			 )`,
			entityType, entityID, tagKey, tagID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(entity_type, entity_id, tag_id) DO UPDATE SET source = excluded.source, applied_at = excluded.applied_at`,
			entityType, entityID, tagID, source, nowUTC(),
		); err != nil {
			return err
		}
		return tx.Commit()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(entity_type, entity_id, tag_id) DO UPDATE SET source = excluded.source, applied_at = excluded.applied_at`,
		entityType, entityID, tagID, source, nowUTC(),
	)
	return err
}

// ListTags returns all known tags ordered by key then value.
func (s *Store) ListTags(ctx context.Context) ([]wms.Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_key, tag_value, is_seed, category, cardinality, description, retired, required, scope, exclusion_group, auto_extract, interview, facet_source FROM tags ORDER BY tag_key, tag_value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []wms.Tag
	for rows.Next() {
		var t wms.Tag
		var isSeed, retired, required int
		if err := rows.Scan(&t.Key, &t.Value, &isSeed, &t.Category, &t.Cardinality, &t.Description, &retired, &required, &t.Scope, &t.ExclusionGroup, &t.AutoExtract, &t.Interview, &t.FacetSource); err != nil {
			return nil, err
		}
		t.IsSeed = isSeed != 0
		t.Retired = retired != 0
		t.Required = required != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// SearchTags returns non-retired tags matching the given filters. Dialect
// note: LIKE clauses add "ESCAPE '\'" — see file doc comment.
func (s *Store) SearchTags(ctx context.Context, tagKey, query string) ([]wms.Tag, error) {
	q := `SELECT tag_key, tag_value, is_seed, category, cardinality, description, retired, required, scope, exclusion_group, auto_extract, interview, facet_source FROM tags WHERE retired = 0`
	var args []interface{}
	if tagKey != "" {
		q += ` AND tag_key = ?`
		args = append(args, tagKey)
	}
	if query != "" {
		q += ` AND (tag_value LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`
		esc := strings.NewReplacer("%", `\%`, "_", `\_`)
		pattern := "%" + esc.Replace(query) + "%"
		args = append(args, pattern, pattern)
	}
	q += ` ORDER BY tag_key, tag_value`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := make([]wms.Tag, 0)
	for rows.Next() {
		var t wms.Tag
		var isSeed, retired, required int
		if err := rows.Scan(&t.Key, &t.Value, &isSeed, &t.Category, &t.Cardinality, &t.Description, &retired, &required, &t.Scope, &t.ExclusionGroup, &t.AutoExtract, &t.Interview, &t.FacetSource); err != nil {
			return nil, err
		}
		t.IsSeed = isSeed != 0
		t.Retired = retired != 0
		t.Required = required != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListRequiredTagKeys returns the distinct, non-retired tag keys marked
// required=1.
func (s *Store) ListRequiredTagKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT tag_key FROM tags WHERE required = 1 AND retired = 0 ORDER BY tag_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RetireTagValue marks a single tag value as retired (retired=1).
func (s *Store) RetireTagValue(ctx context.Context, tagKey, tagValue string) error {
	if tagKey == "" || tagValue == "" {
		return fmt.Errorf("RetireTagValue: tagKey and tagValue are required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tags SET retired = 1 WHERE tag_key = ? AND tag_value = ?`, tagKey, tagValue)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.NotFound("RetireTagValue", "tag", tagKey+":"+tagValue)
	}
	return nil
}

// UpdateTagValueDescription overwrites the description on ONE (tag_key,
// tag_value) row. Not-found correctness: like MySQL, RowsAffected counts
// CHANGED rows, so writing the same description back is a 0-row no-op
// indistinguishable from a missing row; after a 0-row update we
// existence-check the (key,value) to disambiguate.
func (s *Store) UpdateTagValueDescription(ctx context.Context, tagKey, tagValue, description string) error {
	if tagKey == "" || tagValue == "" {
		return fmt.Errorf("UpdateTagValueDescription: tagKey and tagValue are required")
	}
	if err := checkTagDescriptionLen(description); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tags SET description = ? WHERE tag_key = ? AND tag_value = ?`,
		description, tagKey, tagValue)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM tags WHERE tag_key = ? AND tag_value = ? LIMIT 1`, tagKey, tagValue,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFound("UpdateTagValueDescription", "tag", tagKey+":"+tagValue)
		}
		return err
	}
	return nil
}

// GetEntityTags returns the tags directly bound to one entity.
func (s *Store) GetEntityTags(ctx context.Context, entityType, entityID string) ([]wms.EntityTag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.tag_key, t.tag_value, t.category, et.source, t.description, et.applied_at
		FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		WHERE et.entity_type = ? AND et.entity_id = ?
		ORDER BY t.tag_key, t.tag_value`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []wms.EntityTag
	for rows.Next() {
		var et wms.EntityTag
		if err := rows.Scan(&et.TagKey, &et.TagValue, &et.Category, &et.Source, &et.Description, &et.AppliedAt); err != nil {
			return nil, err
		}
		et.AppliedAt = et.AppliedAt.UTC()
		out = append(out, et)
	}
	return out, rows.Err()
}

// DeleteEntityTag removes one (tagKey, tagValue) binding from an entity.
// Dialect note: MySQL's multi-table "DELETE et FROM entity_tags et JOIN tags
// t ..." has no SQLite equivalent (SQLite DELETE supports no JOIN); rewritten
// as a DELETE with a tag_id IN (subquery) filter. Idempotent, same as MySQL.
func (s *Store) DeleteEntityTag(ctx context.Context, entityType, entityID, tagKey, tagValue string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM entity_tags
		WHERE entity_type = ? AND entity_id = ?
		  AND tag_id IN (SELECT id FROM tags WHERE tag_key = ? AND tag_value = ?)`,
		entityType, entityID, tagKey, tagValue)
	return err
}

// systemManagedKeys is the DENY-LIST of writer-coupled lifecycle keys owned
// exclusively by migrations — identical contract to the mysql backend.
var systemManagedKeys = map[string]bool{
	"phase":      true,
	"work-type":  true,
	"resolution": true,
	"lifecycle":  true,
}

// ReconcileVocabulary brings the seed vocabulary in line with the declared
// specs. See mysql's ReconcileVocabulary doc comment for the full contract.
func (s *Store) ReconcileVocabulary(ctx context.Context, specs []wms.TagSpec) error {
	declared := map[string]bool{}
	for _, spec := range specs {
		if spec.Key == "" {
			continue
		}
		if systemManagedKeys[spec.Key] {
			slog.Warn("reconcile: ignoring system-managed key in tags config (owned by migrations)",
				"key", spec.Key)
			continue
		}
		declared[spec.Key] = true
		if err := s.DefineTag(ctx, spec); err != nil {
			return err
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT tag_key FROM tags WHERE is_seed = 1`)
	if err != nil {
		return err
	}
	var seedKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		seedKeys = append(seedKeys, k)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range seedKeys {
		if declared[key] || systemManagedKeys[key] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET is_seed = 0 WHERE tag_key = ?`, key,
		); err != nil {
			return err
		}
	}
	return nil
}

// DefineTag promotes a key into the seed vocabulary. Dialect note: MySQL's
// "ON DUPLICATE KEY UPDATE ... description = IF(description = '',
// VALUES(description), description)" becomes SQLite's upsert with a CASE
// expression; unqualified column names in the DO UPDATE SET clause refer to
// the pre-update (conflicting) row, "excluded.col" to the proposed insert —
// same semantics as MySQL's bare column name vs VALUES(col).
func (s *Store) DefineTag(ctx context.Context, spec wms.TagSpec) error {
	if spec.Key == "" {
		return fmt.Errorf("DefineTag: key is required")
	}
	if systemManagedKeys[spec.Key] {
		return fmt.Errorf("DefineTag: %q is a system-managed key and cannot be redefined", spec.Key)
	}
	if err := checkTagDescriptionLen(spec.Description); err != nil {
		return err
	}
	category := spec.Category
	if category == "" {
		category = "context"
	}
	cardinality := spec.Cardinality
	if cardinality == "" {
		cardinality = "multi"
	}
	values := spec.Values
	if len(values) == 0 {
		values = []string{""} // create-on-apply stub
	}
	for _, v := range values {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES (?, ?, 1, ?, ?, ?)
			 ON CONFLICT(tag_key, tag_value) DO UPDATE SET
			     is_seed     = 1,
			     category    = excluded.category,
			     cardinality = excluded.cardinality,
			     description = CASE WHEN description = '' THEN excluded.description ELSE description END`,
			spec.Key, v, category, cardinality, spec.Description,
		); err != nil {
			return err
		}
	}
	// Cardinality is per-key: keep every value of the key consistent (covers
	// rows minted by create-on-apply that predate this define).
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tags SET cardinality = ? WHERE tag_key = ?`, cardinality, spec.Key,
	); err != nil {
		return err
	}
	if spec.Required != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET required = ? WHERE tag_key = ?`, *spec.Required, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.Scope != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET scope = ? WHERE tag_key = ?`, *spec.Scope, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.ExclusionGroup != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET exclusion_group = ? WHERE tag_key = ?`, *spec.ExclusionGroup, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.AutoExtract != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET auto_extract = ? WHERE tag_key = ?`, *spec.AutoExtract, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.Interview != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET interview = ? WHERE tag_key = ?`, *spec.Interview, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.FacetSource != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET facet_source = ? WHERE tag_key = ?`, *spec.FacetSource, spec.Key,
		); err != nil {
			return err
		}
	}
	return nil
}

// RetireTag demotes a key from the seed vocabulary (is_seed=0).
func (s *Store) RetireTag(ctx context.Context, tagKey string) error {
	if tagKey == "" {
		return fmt.Errorf("RetireTag: tagKey is required")
	}
	if systemManagedKeys[tagKey] {
		return fmt.Errorf("RetireTag: %q is a system-managed key and cannot be retired", tagKey)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tags SET is_seed = 0 WHERE tag_key = ?`, tagKey)
	return err
}
