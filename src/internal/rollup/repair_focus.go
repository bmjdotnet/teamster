package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RepairStats summarizes one focus-interval repair pass.
type RepairStats struct {
	Inverted int // negative-width focus intervals found
	Repaired int // intervals whose ended_at was recomputed
	Reopened int // intervals left open (no valid successor) — ended_at set NULL
}

// invertedInterval is one negative-width focus interval needing repair, with the
// chain context to recompute its ended_at.
type invertedInterval struct {
	id        uint64
	sessionID string
	agentName string
	startedAt time.Time
	priorEnd  sql.NullTime
}

// RepairFocusIntervals fixes focus intervals whose ended_at < started_at — the
// negative-width rows produced by the dual-writer / async-goroutine race before
// the focus-interval-dual-writer fix (a close stamped at a timestamp earlier than
// the interval's own start). Such a row covers an empty window, so focusAt's
// `started_at <= ts AND ended_at > ts` never matches it and the session's cost
// silently falls to unallocated.
//
// REPAIR RULE: within each (session, agent) focus chain ordered by started_at,
// an interval's correct ended_at is the NEXT interval's started_at (the last
// interval stays open / NULL) — exactly the contiguous timeline a single correct
// writer would have produced. We recompute ONLY the inverted rows from their
// successor's started_at; if the successor would still yield a non-positive width
// (no later interval, or a later interval that also starts at/before this one),
// the row is left OPEN (ended_at NULL), which is the conservative choice (an
// open-ended focus is covered from started_at onward by focusAt, never dropped).
//
// IDEMPOTENT: a re-run finds no inverted rows (every repaired row now has
// ended_at >= started_at or NULL), so it is a no-op.
//
// REVERSIBLE: each repaired row's prior (bad) ended_at is recorded in
// focus_interval_repair; UnrepairFocusIntervals restores it. After repair the
// derived aggregates are rebuilt and a normal allocate re-attributes the cost the
// now-valid intervals cover.
//
// DryRun performs ZERO writes (logs the plan + counts only).
func (r *Runner) RepairFocusIntervals(ctx context.Context, dryRun bool) (RepairStats, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, session_id, agent_name, started_at, ended_at
		FROM wms_intervals
		WHERE kind = 'focus' AND ended_at IS NOT NULL AND ended_at < started_at
		ORDER BY session_id, agent_name, started_at`)
	if err != nil {
		return RepairStats{}, fmt.Errorf("list inverted intervals: %w", err)
	}
	var inverted []invertedInterval
	for rows.Next() {
		var iv invertedInterval
		if err := rows.Scan(&iv.id, &iv.sessionID, &iv.agentName, &iv.startedAt, &iv.priorEnd); err != nil {
			rows.Close() //nolint:errcheck
			return RepairStats{}, fmt.Errorf("scan inverted interval: %w", err)
		}
		inverted = append(inverted, iv)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return RepairStats{}, err
	}
	rows.Close() //nolint:errcheck

	now := time.Now().UTC()
	var stats RepairStats
	stats.Inverted = len(inverted)

	// Sessions whose focus timeline we repaired: their previously-dropped cost
	// (unallocated, or sweep_skipped — the LLM sweep gave up on cost the broken
	// interval hid) must be RELEASED so the reallocate re-derives it against the
	// now-valid interval. Scoped to these sessions so unrelated skips are untouched.
	affected := map[string]struct{}{}

	for _, iv := range inverted {
		// The correct ended_at is the earliest focus interval in the same
		// (session, agent) chain that STARTS strictly after this one — the next
		// focus. A miss (no later focus) means this was the chain's last focus:
		// leave it open. We query each row's successor rather than precomputing
		// the chain so a concurrent writer's new interval is naturally respected.
		var successor sql.NullTime
		err := r.db.QueryRowContext(ctx, `
			SELECT MIN(started_at)
			FROM wms_intervals
			WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
			  AND started_at > ?`,
			iv.sessionID, iv.agentName, iv.startedAt).Scan(&successor)
		if err != nil && err != sql.ErrNoRows {
			return stats, fmt.Errorf("interval %d: find successor: %w", iv.id, err)
		}

		var newEnd sql.NullTime
		if successor.Valid && successor.Time.After(iv.startedAt) {
			newEnd = successor // positive-width: close at the next focus's start
		}
		// else: leave open (newEnd stays invalid/NULL) — Reopened.

		if dryRun {
			if newEnd.Valid {
				stats.Repaired++
			} else {
				stats.Reopened++
			}
			r.log.Info("repair-focus-intervals (dry-run): would fix inverted interval",
				"interval_id", iv.id, "session_id", iv.sessionID, "agent_name", iv.agentName,
				"started_at", iv.startedAt, "bad_ended_at", iv.priorEnd.Time,
				"new_ended_at", newEnd, "reopened", !newEnd.Valid)
			continue
		}

		if err := r.applyRepair(ctx, iv, newEnd, now); err != nil {
			return stats, fmt.Errorf("interval %d: apply repair: %w", iv.id, err)
		}
		affected[iv.sessionID] = struct{}{}
		if newEnd.Valid {
			stats.Repaired++
		} else {
			stats.Reopened++
		}
	}

	if !dryRun && (stats.Repaired > 0 || stats.Reopened > 0) {
		// Release the dropped cost on the repaired sessions (unallocated +
		// sweep_skipped) so the reallocate re-derives it against the now-valid
		// intervals. Scoped to the affected sessions — never disturbs skips/
		// attributions on unrelated sessions. CONSERVATION: this only DELETES
		// not-yet-attributed rows; Allocate re-creates exactly one row per message.
		for sid := range affected {
			if _, err := r.db.ExecContext(ctx, `
				DELETE ua FROM usage_attribution ua
				JOIN token_ledger t ON t.message_id = ua.message_id
				WHERE t.session_id = ? AND ua.method IN ('unallocated','sweep_skipped')`,
				sid); err != nil {
				return stats, fmt.Errorf("repair-focus-intervals: release session %s: %w", sid, err)
			}
		}
		// The repaired intervals now cover real windows; reallocate re-attributes
		// the released cost and rebuilds the derived aggregates. Reallocate also
		// clears any remaining unallocated rows globally (harmless — they re-derive).
		if err := r.Run(ctx, true); err != nil {
			return stats, fmt.Errorf("repair-focus-intervals: reallocate: %w", err)
		}
	}

	r.log.Info("repair-focus-intervals pass complete",
		"inverted", stats.Inverted, "repaired", stats.Repaired, "reopened", stats.Reopened,
		"dry_run", dryRun)
	return stats, nil
}

// applyRepair recomputes one inverted interval's ended_at (and duration_ms) and
// records the prior value for reversibility — both in one transaction.
//
// Dual-writer handling: when both the direct (hub MCP) and remote_scraper
// writers create a focus interval for the same entity at the same instant, the
// direct interval can get inverted (the Class A bug) while the remote_scraper
// sibling is correct. Setting a real ended_at on the inverted row would collide
// with the sibling on uq_open. When a non-inverted sibling exists within 5s,
// the inverted row is collapsed to zero-width (ended_at = started_at) instead —
// it becomes harmless and focusAt matches the correct sibling.
func (r *Runner) applyRepair(ctx context.Context, iv invertedInterval, newEnd sql.NullTime, now time.Time) error {
	// Check for a non-inverted sibling from a different writer covering the
	// same entity at approximately the same instant (dual-writer case).
	if newEnd.Valid {
		var siblingID uint64
		err := r.db.QueryRowContext(ctx, `
			SELECT id FROM wms_intervals
			WHERE kind = 'focus'
			  AND session_id = ?
			  AND entity_type = (SELECT entity_type FROM wms_intervals WHERE id = ?)
			  AND entity_id   = (SELECT entity_id   FROM wms_intervals WHERE id = ?)
			  AND id <> ?
			  AND ABS(TIMESTAMPDIFF(SECOND, started_at, ?)) <= 5
			  AND (ended_at IS NULL OR ended_at >= started_at)
			LIMIT 1`,
			iv.sessionID, iv.id, iv.id, iv.id, iv.startedAt).Scan(&siblingID)
		if err == nil {
			r.log.Info("repair-focus-intervals: collapsed dual-writer duplicate",
				"interval_id", iv.id, "sibling_id", siblingID,
				"session_id", iv.sessionID, "agent_name", iv.agentName)
			newEnd = sql.NullTime{Time: iv.startedAt, Valid: true}
		}
		// sql.ErrNoRows = no sibling, proceed with the computed newEnd
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Scoped to a row that is still inverted so a concurrent fix makes this a
	// 0-row no-op rather than clobbering a corrected value.
	if newEnd.Valid {
		_, err := tx.ExecContext(ctx, `
			UPDATE wms_intervals
			SET ended_at = ?, duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
			WHERE id = ? AND kind = 'focus' AND ended_at IS NOT NULL AND ended_at < started_at`,
			newEnd.Time, newEnd.Time, iv.id)
		if err != nil {
			if isDuplicateKeyError(err) {
				// Belt-and-suspenders: the computed ended_at collides with a
				// sibling on uq_open that we didn't detect above. Collapse to
				// zero-width instead of aborting the pass.
				tx.Rollback() //nolint:errcheck
				return r.collapseToZeroWidth(ctx, iv, now)
			}
			return fmt.Errorf("update ended_at: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE wms_intervals
			SET ended_at = NULL, duration_ms = NULL
			WHERE id = ? AND kind = 'focus' AND ended_at IS NOT NULL AND ended_at < started_at`,
			iv.id); err != nil {
			return fmt.Errorf("reopen interval: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO focus_interval_repair (interval_id, prior_ended_at, new_ended_at, repaired_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			prior_ended_at = VALUES(prior_ended_at),
			new_ended_at   = VALUES(new_ended_at),
			repaired_at    = VALUES(repaired_at)`,
		iv.id, iv.priorEnd, newEnd, now); err != nil {
		return fmt.Errorf("record repair evidence: %w", err)
	}

	return tx.Commit()
}

// collapseToZeroWidth sets an inverted interval's ended_at = started_at,
// making it zero-width and harmless. Used as a fallback when the computed
// ended_at would collide with a sibling on uq_open.
func (r *Runner) collapseToZeroWidth(ctx context.Context, iv invertedInterval, now time.Time) error {
	r.log.Info("repair-focus-intervals: collapsed to zero-width after duplicate key",
		"interval_id", iv.id, "session_id", iv.sessionID, "agent_name", iv.agentName)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	zeroEnd := sql.NullTime{Time: iv.startedAt, Valid: true}
	if _, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?, duration_ms = 0
		WHERE id = ? AND kind = 'focus' AND ended_at IS NOT NULL AND ended_at < started_at`,
		iv.startedAt, iv.id); err != nil {
		return fmt.Errorf("collapse to zero-width: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO focus_interval_repair (interval_id, prior_ended_at, new_ended_at, repaired_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			prior_ended_at = VALUES(prior_ended_at),
			new_ended_at   = VALUES(new_ended_at),
			repaired_at    = VALUES(repaired_at)`,
		iv.id, iv.priorEnd, zeroEnd, now); err != nil {
		return fmt.Errorf("record repair evidence: %w", err)
	}

	return tx.Commit()
}

func isDuplicateKeyError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Duplicate entry") || strings.Contains(s, "1062")
}

// RepairStateStats summarizes one state-interval repair pass.
type RepairStateStats struct {
	Inverted  int // negative-width state intervals found
	Repaired  int // intervals whose ended_at was set to successor's started_at
	Collapsed int // intervals collapsed to zero-width (no valid successor)
	Deleted   int // intervals deleted because zero-width collapse collided on uq_open
}

// RepairStateIntervals fixes state intervals whose ended_at < started_at.
// These are produced when closeOpenStateIntervals was called without the
// started_at <= ? guard (before the ordering-safe fix). State intervals track
// entity status durations and do not affect cost attribution, so no audit table
// or cost reattribution is needed — repair is applied in-place.
//
// REPAIR RULE: for each inverted interval, the correct ended_at is the next
// state interval's started_at for the same entity (entity_type + entity_id).
// When no later interval exists, the row is collapsed to zero-width
// (ended_at = started_at) — conservative and unambiguous.
//
// IDEMPOTENT: a re-run finds no inverted rows (all repaired rows now have
// ended_at >= started_at), so it is a no-op.
//
// DryRun performs ZERO writes (logs the plan + counts only).
func (r *Runner) RepairStateIntervals(ctx context.Context, dryRun bool) (RepairStateStats, error) {
	type invertedState struct {
		id         uint64
		entityType string
		entityID   string
		startedAt  time.Time
		badEndedAt time.Time
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, started_at, ended_at
		FROM wms_intervals
		WHERE kind = 'state' AND ended_at IS NOT NULL AND ended_at < started_at
		ORDER BY entity_type, entity_id, started_at`)
	if err != nil {
		return RepairStateStats{}, fmt.Errorf("list inverted state intervals: %w", err)
	}
	var inverted []invertedState
	for rows.Next() {
		var iv invertedState
		if err := rows.Scan(&iv.id, &iv.entityType, &iv.entityID, &iv.startedAt, &iv.badEndedAt); err != nil {
			rows.Close() //nolint:errcheck
			return RepairStateStats{}, fmt.Errorf("scan inverted state interval: %w", err)
		}
		inverted = append(inverted, iv)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return RepairStateStats{}, err
	}
	rows.Close() //nolint:errcheck

	var stats RepairStateStats
	stats.Inverted = len(inverted)

	for _, iv := range inverted {
		var successor sql.NullTime
		err := r.db.QueryRowContext(ctx, `
			SELECT MIN(started_at)
			FROM wms_intervals
			WHERE kind = 'state' AND entity_type = ? AND entity_id = ?
			  AND started_at > ?`,
			iv.entityType, iv.entityID, iv.startedAt).Scan(&successor)
		if err != nil && err != sql.ErrNoRows {
			return stats, fmt.Errorf("state interval %d: find successor: %w", iv.id, err)
		}

		var newEnd time.Time
		collapsed := false
		if successor.Valid && successor.Time.After(iv.startedAt) {
			newEnd = successor.Time
		} else {
			newEnd = iv.startedAt // zero-width: ended_at = started_at
			collapsed = true
		}

		if dryRun {
			if collapsed {
				stats.Collapsed++
			} else {
				stats.Repaired++
			}
			r.log.Info("repair-state-intervals (dry-run): would fix inverted interval",
				"interval_id", iv.id, "entity_type", iv.entityType, "entity_id", iv.entityID,
				"started_at", iv.startedAt, "bad_ended_at", iv.badEndedAt,
				"new_ended_at", newEnd, "collapsed", collapsed)
			continue
		}

		var durationMS *int64
		if !collapsed {
			d := successor.Time.Sub(iv.startedAt).Microseconds() / 1000
			durationMS = &d
		}
		if durationMS != nil {
			_, err := r.db.ExecContext(ctx, `
				UPDATE wms_intervals
				SET ended_at = ?, duration_ms = ?
				WHERE id = ? AND kind = 'state' AND ended_at IS NOT NULL AND ended_at < started_at`,
				newEnd, *durationMS, iv.id)
			if err != nil {
				if !isDuplicateKeyError(err) {
					return stats, fmt.Errorf("state interval %d: repair: %w", iv.id, err)
				}
				// uq_open collision: a valid closed row already exists at successor.started_at.
				// The inverted row is corrupted — delete it.
				if _, err := r.db.ExecContext(ctx,
					`DELETE FROM wms_intervals WHERE id = ? AND kind = 'state' AND ended_at < started_at`,
					iv.id); err != nil {
					return stats, fmt.Errorf("state interval %d: delete after collision: %w", iv.id, err)
				}
				r.log.Info("repair-state-intervals: deleted inverted interval after uq_open collision",
					"interval_id", iv.id, "entity_type", iv.entityType, "entity_id", iv.entityID,
					"started_at", iv.startedAt)
				stats.Deleted++
				continue
			}
		} else {
			_, err := r.db.ExecContext(ctx, `
				UPDATE wms_intervals
				SET ended_at = ?, duration_ms = 0
				WHERE id = ? AND kind = 'state' AND ended_at IS NOT NULL AND ended_at < started_at`,
				newEnd, iv.id)
			if err != nil {
				if !isDuplicateKeyError(err) {
					return stats, fmt.Errorf("state interval %d: collapse: %w", iv.id, err)
				}
				// uq_open collision: a valid row already exists at ended_at=started_at for
				// this entity. The inverted row is zero-information — delete it.
				if _, err := r.db.ExecContext(ctx,
					`DELETE FROM wms_intervals WHERE id = ? AND kind = 'state' AND ended_at < started_at`,
					iv.id); err != nil {
					return stats, fmt.Errorf("state interval %d: delete after collision: %w", iv.id, err)
				}
				r.log.Info("repair-state-intervals: deleted inverted interval after uq_open collision",
					"interval_id", iv.id, "entity_type", iv.entityType, "entity_id", iv.entityID,
					"started_at", iv.startedAt)
				stats.Deleted++
				continue
			}
		}
		if collapsed {
			stats.Collapsed++
		} else {
			stats.Repaired++
		}
	}

	r.log.Info("repair-state-intervals pass complete",
		"inverted", stats.Inverted, "repaired", stats.Repaired, "collapsed", stats.Collapsed,
		"deleted", stats.Deleted, "dry_run", dryRun)
	return stats, nil
}

// UnrepairFocusIntervals reverses a repair pass: restores each repaired interval's
// prior ended_at from focus_interval_repair and clears the evidence. Returns the
// number of intervals reverted. After unrepair the rows are inverted again (the
// pre-fix state), so it is a true undo of the data change.
func (r *Runner) UnrepairFocusIntervals(ctx context.Context) (int, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT interval_id, prior_ended_at FROM focus_interval_repair`)
	if err != nil {
		return 0, fmt.Errorf("list repairs: %w", err)
	}
	type rep struct {
		id    uint64
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

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	n := 0
	for _, rp := range reps {
		if rp.prior.Valid {
			if _, err := tx.ExecContext(ctx, `
				UPDATE wms_intervals
				SET ended_at = ?, duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
				WHERE id = ? AND kind = 'focus'`,
				rp.prior.Time, rp.prior.Time, rp.id); err != nil {
				return 0, fmt.Errorf("restore interval %d: %w", rp.id, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE wms_intervals SET ended_at = NULL, duration_ms = NULL
				WHERE id = ? AND kind = 'focus'`, rp.id); err != nil {
				return 0, fmt.Errorf("restore interval %d: %w", rp.id, err)
			}
		}
		n++
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM focus_interval_repair`); err != nil {
		return 0, fmt.Errorf("clear repair evidence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
