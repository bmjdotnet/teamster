// IntervalStore — focus interval open/close/write, ported from
// internal/store/mysql/store.go's interval section.
//
// Dialect notes:
//   - MySQL's withFocusLock (a GET_LOCK advisory lock) plus SELECT ... FOR
//     UPDATE is dropped entirely: with the Store's connection pool pinned to
//     one connection, a BeginTx here already holds the only connection for
//     its duration, so a concurrent caller simply blocks in the pool until
//     Commit/Rollback — the same "no lost updates, no duplicate opens"
//     outcome via coarser whole-connection serialization instead of MySQL's
//     row lock + named lock pair (07-conformance.md: contract is the
//     outcome, not the mechanism).
//   - closeOpenFocusIntervals translates TIMESTAMPDIFF(MICROSECOND, a, b)/1000
//     to CAST((julianday(b) - julianday(a)) * 86400000 AS INTEGER).
//   - CloseIntervalsOnTerminalEntities/CloseIntervalsForClosedSessions/
//     CloseIntervalsForStaleSessions replace MySQL's multi-table
//     "UPDATE wms_intervals i JOIN <table> ... SET i.ended_at = DATE_ADD(NOW(6),
//     INTERVAL i.id MICROSECOND) ..." (SQLite UPDATE has no JOIN) with a
//     SELECT (JOIN is fine in a plain SELECT) to find the candidate interval
//     ids, then a per-row UPDATE keyed by id. The DATE_ADD(...MICROSECOND)
//     per-row offset is preserved (as a Go-computed unique timestamp per
//     row, base-time-plus-interval-id-microseconds) because it is
//     load-bearing: multiple open rows for the SAME (entity_type, entity_id,
//     kind) collide on uq_open if given the identical ended_at, and MySQL's
//     single UPDATE avoids that by making every row's ended_at unique via
//     its own id. INSERT IGNORE -> INSERT OR IGNORE is the other dialect
//     change (WriteFocusInterval/WriteBriefDirectiveInterval).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// OpenFocusInterval closes the currently-open focus interval for (session,
// agent) and opens a new one for entityType/entityID. Same-entity guard: if
// the current open interval is already this exact (entityType, entityID),
// it is a no-op.
func (s *Store) OpenFocusInterval(ctx context.Context, key store.SessionKey, entityType, entityID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// now is captured AFTER BeginTx succeeds, not before: BeginTx is where
	// contention is actually resolved on this backend's pinned single
	// connection (the sqlite analog of MySQL's withFocusLock, which acquires
	// its GET_LOCK before computing "now"). Capturing it earlier would let a
	// goroutine that blocked waiting for the connection use a stale "now" —
	// breaking closeOpenFocusIntervals' `started_at <= at` ordering guard
	// against rows other goroutines already inserted with later timestamps
	// while this one was queued, and leaving more than one row open.
	now := nowUTC()

	var curType, curID string
	err = tx.QueryRowContext(ctx,
		`SELECT entity_type, entity_id FROM wms_intervals
		 WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		 ORDER BY started_at DESC LIMIT 1`,
		key.SessionID, key.AgentName,
	).Scan(&curType, &curID)
	if err == nil && curType == entityType && curID == entityID {
		return tx.Commit() // already focused on this exact entity
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Close the open focus interval then open the new one. Focus rows carry
	// identity directly (it's always present on the focus path), so
	// identity_source='direct'.
	if err := closeOpenFocusIntervals(ctx, tx, key.SessionID, key.AgentName, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, 'direct')`,
		entityType, entityID, key.SessionID, key.AgentName, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// HasAnyFocusInterval returns true when (session, agent) has any kind='focus'
// interval row, open or closed.
func (s *Store) HasAnyFocusInterval(ctx context.Context, key store.SessionKey) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM wms_intervals
		 WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		 LIMIT 1`,
		key.SessionID, key.AgentName,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// closeOpenFocusIntervals closes every open kind='focus' wms_intervals row
// for (session, agent) at `at`, computing duration_ms. ORDERING-SAFE: the
// `started_at <= at` guard means an out-of-order close never sets
// `ended_at < started_at`.
func closeOpenFocusIntervals(ctx context.Context, tx *sql.Tx, sessionID, agentName string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		at, at, sessionID, agentName, at)
	return err
}

// CloseFocusInterval ends the currently-open focus interval for (session,
// agent) without opening a new one.
func (s *Store) CloseFocusInterval(ctx context.Context, key store.SessionKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// now captured after BeginTx — see OpenFocusInterval's doc comment.
	now := nowUTC()
	if err := closeOpenFocusIntervals(ctx, tx, key.SessionID, key.AgentName, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseFocusIntervalForEntity is the entity-scoped close: it ends the
// agent's open focus interval ONLY when that interval's (entity_type,
// entity_id) is exactly (entityType, entityID). A 0-row no-op otherwise.
func (s *Store) CloseFocusIntervalForEntity(ctx context.Context, key store.SessionKey, entityType, entityID string) error {
	now := nowUTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		  AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		now, now, key.SessionID, key.AgentName, entityType, entityID, now)
	return err
}

// CloseSessionIntervals closes all open wms_intervals rows for the given
// session, computing duration_ms from started_at. When agentName is
// non-empty, only that agent's intervals are closed; when empty, ALL
// intervals for the session are closed. Returns the number of rows closed.
func (s *Store) CloseSessionIntervals(ctx context.Context, sessionID, agentName string, at time.Time) (int64, error) {
	if at.IsZero() {
		at = nowUTC()
	}
	query := `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
		WHERE session_id = ? AND ended_at IS NULL
		  AND started_at <= ?`
	args := []any{at, at, sessionID, at}
	if agentName != "" {
		query += ` AND agent_name = ?`
		args = append(args, agentName)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, classifyConflict("CloseSessionIntervals", err)
	}
	return res.RowsAffected()
}

// closeCandidate is one open wms_intervals row identified as a close
// candidate by a reaper's SELECT...JOIN read, before the per-row UPDATE
// that actually closes it.
type closeCandidate struct {
	id        int64
	startedAt time.Time
}

// closeCandidatesByID closes each candidate row with a distinct ended_at
// (base + candidate id, in microseconds) so multiple rows for the SAME
// (entity_type, entity_id, kind) — the double-open recovery case — never
// collide on uq_open (entity_type, entity_id, kind, ended_at). Mirrors
// MySQL's "DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND)" per-row offset,
// computed here in Go rather than as a per-row SQL expression since the
// write is now a per-id UPDATE rather than one JOIN-based bulk statement.
func closeCandidatesByID(ctx context.Context, db *sql.DB, base time.Time, candidates []closeCandidate) (int64, error) {
	var total int64
	for _, c := range candidates {
		endAt := base.Add(time.Duration(c.id) * time.Microsecond)
		dur := endAt.Sub(c.startedAt).Milliseconds()
		res, err := db.ExecContext(ctx,
			`UPDATE wms_intervals SET ended_at = ?, duration_ms = ? WHERE id = ? AND ended_at IS NULL`,
			endAt, dur, c.id)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// CloseIntervalsOnTerminalEntities closes open intervals whose entity has
// reached a terminal status (done). Phase 1 of the reaper.
func (s *Store) CloseIntervalsOnTerminalEntities(ctx context.Context) (int64, error) {
	var total int64
	base := nowUTC()
	for _, tbl := range []struct{ table, entityType string }{
		{"outcomes", "outcome"},
		{"workunits", "workunit"},
	} {
		rows, err := s.db.QueryContext(ctx, `
			SELECT i.id, i.started_at
			FROM wms_intervals i
			JOIN `+tbl.table+` e ON e.id = i.entity_id AND e.status = 'done'
			WHERE i.entity_type = ? AND i.ended_at IS NULL`, tbl.entityType)
		if err != nil {
			return total, err
		}
		var candidates []closeCandidate
		for rows.Next() {
			var c closeCandidate
			if err := rows.Scan(&c.id, &c.startedAt); err != nil {
				rows.Close() //nolint:errcheck
				return total, err
			}
			candidates = append(candidates, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return total, err
		}
		rows.Close() //nolint:errcheck

		n, err := closeCandidatesByID(ctx, s.db, base, candidates)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// CloseIntervalsForClosedSessions closes open intervals belonging to
// sessions marked closed. Phase 2 of the reaper.
func (s *Store) CloseIntervalsForClosedSessions(ctx context.Context) (int64, error) {
	base := nowUTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.started_at
		FROM wms_intervals i
		JOIN sessions se ON se.session_id = i.session_id
		                AND se.agent_name = i.agent_name
		                AND se.status = 'closed'
		WHERE i.ended_at IS NULL`)
	if err != nil {
		return 0, err
	}
	var candidates []closeCandidate
	for rows.Next() {
		var c closeCandidate
		if err := rows.Scan(&c.id, &c.startedAt); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return 0, err
	}
	rows.Close() //nolint:errcheck

	return closeCandidatesByID(ctx, s.db, base, candidates)
}

// CloseIntervalsForStaleSessions closes open intervals for sessions whose
// last_seen is older than staleThreshold and that are not already closed.
// Phase 3 of the reaper (guarded, disabled by default).
func (s *Store) CloseIntervalsForStaleSessions(ctx context.Context, staleThreshold time.Time) (int64, error) {
	base := nowUTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.started_at
		FROM wms_intervals i
		JOIN sessions se ON se.session_id = i.session_id
		                AND se.agent_name = i.agent_name
		WHERE i.ended_at IS NULL
		  AND se.last_seen < ?
		  AND se.status <> 'closed'`, staleThreshold)
	if err != nil {
		return 0, err
	}
	var candidates []closeCandidate
	for rows.Next() {
		var c closeCandidate
		if err := rows.Scan(&c.id, &c.startedAt); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return 0, err
	}
	rows.Close() //nolint:errcheck

	return closeCandidatesByID(ctx, s.db, base, candidates)
}

// WriteFocusInterval is the remote_scraper path: atomically closes the open
// focus interval for (session, agent) and opens a new one at `at`, stamping
// identity_source='remote_scraper'.
func (s *Store) WriteFocusInterval(ctx context.Context, sessionID, agentName, entityType, entityID string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var curType, curID string
	err = tx.QueryRowContext(ctx, `
		SELECT entity_type, entity_id FROM wms_intervals
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1`,
		sessionID, agentName).Scan(&curType, &curID)
	if err == nil && curType == entityType && curID == entityID {
		return tx.Commit() // already focused on this exact entity
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Close any open focus interval for this (session, agent). Reuses the
	// same ordering-safe `started_at <= at` guard as OpenFocusInterval.
	if err := closeOpenFocusIntervals(ctx, tx, sessionID, agentName, at); err != nil {
		return err
	}

	// INSERT OR IGNORE handles dedup: if the same
	// (session_id, agent_name, entity_type, entity_id, started_at) already
	// exists via the uq_open unique index, the INSERT is a no-op.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, 'remote_scraper')`,
		entityType, entityID, sessionID, agentName, at); err != nil {
		return err
	}

	return tx.Commit()
}

// WriteBriefDirectiveInterval materializes a focus-less teammate's INTENDED
// focus as a subordinate, open-ended focus interval — but ONLY when (a)
// (session, agent) has no focus interval of ANY source yet, and (b) the
// named entity actually exists in WMS.
func (s *Store) WriteBriefDirectiveInterval(ctx context.Context, sessionID, agentName, entityType, entityID, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Subordinate gate: do nothing if ANY focus interval already exists for
	// this (session, agent) — a real setFocus, or a directive we already
	// wrote.
	var exists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM wms_intervals
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		LIMIT 1`,
		sessionID, agentName).Scan(&exists)
	if err == nil {
		// A focus interval already exists — leave it; directive is subordinate.
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		return store.Precondition("WriteBriefDirectiveInterval", "session", sessionID+"/"+agentName)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Validate the named entity exists in WMS before materializing an
	// interval. Deliberately NOT s.GetWorkUnit/s.GetOutcome (which query via
	// s.db, not tx): with the pool pinned to one connection, a nested s.db
	// call while tx holds that connection would self-deadlock (tx waits
	// forever for the connection it already holds to free up). The existence
	// check is instead run against tx directly.
	var existsTable string
	switch entityType {
	case "workunit":
		existsTable = "workunits"
	case "outcome":
		existsTable = "outcomes"
	default:
		return store.NotFound("WriteBriefDirectiveInterval", entityType, entityID)
	}
	var entityExists int
	if err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM "+existsTable+" WHERE id = ? LIMIT 1", entityID,
	).Scan(&entityExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFound("WriteBriefDirectiveInterval", entityType, entityID)
		}
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, ?)`,
		entityType, entityID, sessionID, agentName, nowUTC(), source); err != nil {
		return err
	}

	return tx.Commit()
}
