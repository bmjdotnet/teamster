package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/store"
)

// NewFromDB wraps an already-open, already-migrated *sql.DB as a *Store,
// skipping the connect/migrate steps New performs. Temporary migration
// bridge (Phase 04-12 only) for callers that hold a raw *sql.DB from an
// earlier phase (e.g. internal/rollup.Runner) and need MaintenanceStore
// methods before their own constructor threads a proper
// store.MaintenanceStore through.
func NewFromDB(db *sql.DB) *Store {
	return &Store{db: db}
}

// classifyConflict maps a MySQL 1062 duplicate-key error (the uq_open
// collisions these interval writes can hit) onto store.ErrConflict, so
// callers branch via errors.Is instead of matching driver text.
func classifyConflict(op string, err error) error {
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1062 {
		return store.Conflict(op, err)
	}
	return err
}

// OrphanIntervals returns every wms_intervals row with no session_id — the
// backfill target set. Ported from cmd/teamster/wms_backfill.go's
// loadOrphanedIntervals.
func (s *Store) OrphanIntervals(ctx context.Context) ([]store.Interval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, entity_type, entity_id, state, session_id, agent_name,
		       started_at, ended_at
		FROM wms_intervals
		WHERE session_id = '' OR session_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Interval
	for rows.Next() {
		var iv store.Interval
		var endedAt sql.NullTime
		if err := rows.Scan(&iv.ID, &iv.Kind, &iv.EntityType, &iv.EntityID, &iv.State,
			&iv.SessionID, &iv.AgentName, &iv.StartedAt, &endedAt); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			t := endedAt.Time
			iv.EndedAt = &t
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// BackfillInterval sets id's session_id/agent_name (and, when endedAt is
// non-nil, its ended_at/duration_ms) via a single UPDATE attempt. Ported from
// wms_backfill.go's applyBackfill; the retry-on-conflict loop stays in the
// caller, which nudges endedAt and retries on ErrConflict.
func (s *Store) BackfillInterval(ctx context.Context, id int64, sessionID, agentName string, endedAt *time.Time, durationMs *int64) error {
	if endedAt == nil {
		_, err := s.db.ExecContext(ctx,
			`UPDATE wms_intervals SET session_id = ?, agent_name = ?, identity_source = 'backfill' WHERE id = ?`,
			sessionID, agentName, id)
		return err
	}
	var dur int64
	if durationMs != nil {
		dur = *durationMs
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE wms_intervals
		 SET session_id = ?, agent_name = ?, ended_at = ?, duration_ms = ?, identity_source = 'backfill'
		 WHERE id = ?`,
		sessionID, agentName, *endedAt, dur, id)
	if err != nil {
		return classifyConflict("BackfillInterval", err)
	}
	return nil
}

// InvertedFocusIntervals returns kind='focus' rows with ended_at < started_at
// — the negative-width rows the dual-writer/async race produced.
func (s *Store) InvertedFocusIntervals(ctx context.Context) ([]store.Interval, error) {
	return s.invertedIntervals(ctx, "focus")
}

// InvertedStateIntervals is the kind='state' counterpart of InvertedFocusIntervals.
func (s *Store) InvertedStateIntervals(ctx context.Context) ([]store.Interval, error) {
	return s.invertedIntervals(ctx, "state")
}

func (s *Store) invertedIntervals(ctx context.Context, kind string) ([]store.Interval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, agent_name, entity_type, entity_id, started_at, ended_at
		FROM wms_intervals
		WHERE kind = ? AND ended_at IS NOT NULL AND ended_at < started_at
		ORDER BY session_id, agent_name, entity_type, entity_id, started_at`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Interval
	for rows.Next() {
		var iv store.Interval
		var endedAt sql.NullTime
		if err := rows.Scan(&iv.ID, &iv.SessionID, &iv.AgentName, &iv.EntityType, &iv.EntityID,
			&iv.StartedAt, &endedAt); err != nil {
			return nil, err
		}
		iv.Kind = kind
		if endedAt.Valid {
			t := endedAt.Time
			iv.EndedAt = &t
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// EarliestIntervalStart returns the earliest started_at strictly after
// `after`, scoped by kind: kind="focus" scopes by (session_id, agent_name)
// in (scopeA, scopeB); kind="state" scopes by (entity_type, entity_id) in
// the same two positions — mirroring wms_intervals' two lookup-index
// families (idx_focus_lookup vs idx_kind_entity).
func (s *Store) EarliestIntervalStart(ctx context.Context, scopeA, scopeB, kind string, after time.Time) (time.Time, bool, error) {
	var query string
	switch kind {
	case "focus":
		query = `SELECT MIN(started_at) FROM wms_intervals
		          WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND started_at > ?`
	case "state":
		query = `SELECT MIN(started_at) FROM wms_intervals
		          WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND started_at > ?`
	default:
		return time.Time{}, false, fmt.Errorf("EarliestIntervalStart: unknown kind %q", kind)
	}
	var start sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, scopeA, scopeB, after).Scan(&start); err != nil {
		return time.Time{}, false, err
	}
	if !start.Valid {
		return time.Time{}, false, nil
	}
	return start.Time.UTC(), true, nil
}

// RepairInterval clamps interval id's ended_at to newEnd (deriving
// duration_ms from newStart..newEnd) in one tx; a zero-value newEnd means
// reopen (ended_at/duration_ms → NULL — mode="focus" only). mode="focus"
// also records reversible evidence in focus_interval_repair; mode="state"
// does not (state intervals carry no undo table). Scoped to a row that is
// still inverted so a concurrent fix makes this a 0-row no-op. Returns
// ErrConflict on a uq_open collision.
func (s *Store) RepairInterval(ctx context.Context, id int64, newStart, newEnd time.Time, mode string) error {
	var kind string
	var recordEvidence bool
	switch mode {
	case "focus":
		kind, recordEvidence = "focus", true
	case "state":
		kind, recordEvidence = "state", false
	default:
		return fmt.Errorf("RepairInterval: unknown mode %q", mode)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var priorEnded sql.NullTime
	if recordEvidence {
		if err := tx.QueryRowContext(ctx,
			`SELECT ended_at FROM wms_intervals WHERE id = ?`, id,
		).Scan(&priorEnded); err != nil {
			return fmt.Errorf("RepairInterval: read prior state: %w", err)
		}
	}

	var res sql.Result
	if newEnd.IsZero() {
		res, err = tx.ExecContext(ctx, `
			UPDATE wms_intervals
			SET ended_at = NULL, duration_ms = NULL
			WHERE id = ? AND kind = ? AND started_at = ? AND ended_at IS NOT NULL AND ended_at < started_at`,
			id, kind, newStart)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE wms_intervals
			SET ended_at = ?, duration_ms = TIMESTAMPDIFF(MICROSECOND, ?, ?) / 1000
			WHERE id = ? AND kind = ? AND started_at = ? AND ended_at IS NOT NULL AND ended_at < started_at`,
			newEnd, newStart, newEnd, id, kind, newStart)
	}
	if err != nil {
		return classifyConflict("RepairInterval", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit() // already repaired by a concurrent pass — no-op
	}

	if recordEvidence {
		var newEndedAt sql.NullTime
		if !newEnd.IsZero() {
			newEndedAt = sql.NullTime{Time: newEnd, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO focus_interval_repair (interval_id, prior_ended_at, new_ended_at, repaired_at)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				prior_ended_at = VALUES(prior_ended_at),
				new_ended_at   = VALUES(new_ended_at),
				repaired_at    = VALUES(repaired_at)`,
			id, priorEnded, newEndedAt, nowUTC()); err != nil {
			return fmt.Errorf("RepairInterval: record evidence: %w", err)
		}
	}

	return tx.Commit()
}

// CollapseIntervalToZeroWidth resolves interval id to a harmless terminal
// state after a uq_open collision: mode="focus" sets ended_at=started_at and
// records evidence (reversible via UnrepairIntervals); mode="state" deletes
// the corrupted row outright (state intervals carry no undo table — the row
// is zero-information once a valid sibling already occupies its slot).
func (s *Store) CollapseIntervalToZeroWidth(ctx context.Context, id int64, mode string) error {
	switch mode {
	case "focus":
		return s.collapseFocusToZeroWidth(ctx, id)
	case "state":
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM wms_intervals WHERE id = ? AND kind = 'state' AND ended_at IS NOT NULL AND ended_at < started_at`,
			id)
		return err
	default:
		return fmt.Errorf("CollapseIntervalToZeroWidth: unknown mode %q", mode)
	}
}

func (s *Store) collapseFocusToZeroWidth(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var startedAt time.Time
	var priorEnded sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT started_at, ended_at FROM wms_intervals WHERE id = ?`, id,
	).Scan(&startedAt, &priorEnded); err != nil {
		return fmt.Errorf("CollapseIntervalToZeroWidth: read row: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?, duration_ms = 0
		WHERE id = ? AND kind = 'focus' AND ended_at IS NOT NULL AND ended_at < started_at`,
		startedAt, id); err != nil {
		return fmt.Errorf("CollapseIntervalToZeroWidth: %w", err)
	}

	zeroEnd := sql.NullTime{Time: startedAt, Valid: true}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO focus_interval_repair (interval_id, prior_ended_at, new_ended_at, repaired_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			prior_ended_at = VALUES(prior_ended_at),
			new_ended_at   = VALUES(new_ended_at),
			repaired_at    = VALUES(repaired_at)`,
		id, priorEnded, zeroEnd, nowUTC()); err != nil {
		return fmt.Errorf("CollapseIntervalToZeroWidth: record evidence: %w", err)
	}

	return tx.Commit()
}

// UnrepairIntervals reverses every recorded focus-interval repair, restoring
// each row's prior ended_at from focus_interval_repair and clearing the
// evidence. Ported from repair_focus.go's UnrepairFocusIntervals.
func (s *Store) UnrepairIntervals(ctx context.Context) (int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT interval_id, prior_ended_at FROM focus_interval_repair`)
	if err != nil {
		return 0, fmt.Errorf("UnrepairIntervals: list repairs: %w", err)
	}
	type rep struct {
		id    int64
		prior sql.NullTime
	}
	var reps []rep
	for rows.Next() {
		var rp rep
		if err := rows.Scan(&rp.id, &rp.prior); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		reps = append(reps, rp)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return 0, err
	}
	rows.Close() //nolint:errcheck

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	var n int64
	for _, rp := range reps {
		if rp.prior.Valid {
			if _, err := tx.ExecContext(ctx, `
				UPDATE wms_intervals
				SET ended_at = ?, duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
				WHERE id = ? AND kind = 'focus'`,
				rp.prior.Time, rp.prior.Time, rp.id); err != nil {
				return 0, fmt.Errorf("UnrepairIntervals: restore interval %d: %w", rp.id, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE wms_intervals SET ended_at = NULL, duration_ms = NULL
				WHERE id = ? AND kind = 'focus'`, rp.id); err != nil {
				return 0, fmt.Errorf("UnrepairIntervals: restore interval %d: %w", rp.id, err)
			}
		}
		n++
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM focus_interval_repair`); err != nil {
		return 0, fmt.Errorf("UnrepairIntervals: clear evidence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
