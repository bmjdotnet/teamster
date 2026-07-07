package rollup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// RepairStats summarizes one focus-interval repair pass.
type RepairStats struct {
	Inverted int // negative-width focus intervals found
	Repaired int // intervals whose ended_at was recomputed
	Reopened int // intervals left open (no valid successor) — ended_at set NULL
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
	ms := r.maint

	inverted, err := ms.InvertedFocusIntervals(ctx)
	if err != nil {
		return RepairStats{}, fmt.Errorf("list inverted intervals: %w", err)
	}

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
		successor, ok, err := ms.EarliestIntervalStart(ctx, iv.SessionID, iv.AgentName, "focus", iv.StartedAt)
		if err != nil {
			return stats, fmt.Errorf("interval %d: find successor: %w", iv.ID, err)
		}

		var newEnd time.Time
		if ok && successor.After(iv.StartedAt) {
			newEnd = successor // positive-width: close at the next focus's start
		}
		// else: leave open (newEnd stays zero) — Reopened.

		var priorEnd time.Time
		if iv.EndedAt != nil {
			priorEnd = *iv.EndedAt
		}

		if dryRun {
			if !newEnd.IsZero() {
				stats.Repaired++
			} else {
				stats.Reopened++
			}
			r.log.Info("repair-focus-intervals (dry-run): would fix inverted interval",
				"interval_id", iv.ID, "session_id", iv.SessionID, "agent_name", iv.AgentName,
				"started_at", iv.StartedAt, "bad_ended_at", priorEnd,
				"new_ended_at", newEnd, "reopened", newEnd.IsZero())
			continue
		}

		if err := r.applyFocusRepair(ctx, ms, iv, newEnd); err != nil {
			return stats, fmt.Errorf("interval %d: apply repair: %w", iv.ID, err)
		}
		affected[iv.SessionID] = struct{}{}
		if !newEnd.IsZero() {
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
			if _, err := r.rec.ReleaseSessionAttribution(ctx, sid, []string{"unallocated", "sweep_skipped"}); err != nil {
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

// applyFocusRepair clamps one inverted focus interval via MaintenanceStore.
// RepairInterval, falling back to CollapseIntervalToZeroWidth on a uq_open
// collision (e.g. a dual-writer sibling already occupies the computed
// ended_at) — collapsing to zero-width is harmless and idempotent regardless
// of which writer produced the colliding row.
func (r *Runner) applyFocusRepair(ctx context.Context, ms store.MaintenanceStore, iv store.Interval, newEnd time.Time) error {
	err := ms.RepairInterval(ctx, iv.ID, iv.StartedAt, newEnd, "focus")
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrConflict) {
		r.log.Info("repair-focus-intervals: collapsed to zero-width after uq_open collision",
			"interval_id", iv.ID, "session_id", iv.SessionID, "agent_name", iv.AgentName)
		return ms.CollapseIntervalToZeroWidth(ctx, iv.ID, "focus")
	}
	return err
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
	ms := r.maint

	inverted, err := ms.InvertedStateIntervals(ctx)
	if err != nil {
		return RepairStateStats{}, fmt.Errorf("list inverted state intervals: %w", err)
	}

	var stats RepairStateStats
	stats.Inverted = len(inverted)

	for _, iv := range inverted {
		successor, ok, err := ms.EarliestIntervalStart(ctx, iv.EntityType, iv.EntityID, "state", iv.StartedAt)
		if err != nil {
			return stats, fmt.Errorf("state interval %d: find successor: %w", iv.ID, err)
		}

		var newEnd time.Time
		collapsed := false
		if ok && successor.After(iv.StartedAt) {
			newEnd = successor
		} else {
			newEnd = iv.StartedAt // zero-width: ended_at = started_at
			collapsed = true
		}

		var badEndedAt time.Time
		if iv.EndedAt != nil {
			badEndedAt = *iv.EndedAt
		}

		if dryRun {
			if collapsed {
				stats.Collapsed++
			} else {
				stats.Repaired++
			}
			r.log.Info("repair-state-intervals (dry-run): would fix inverted interval",
				"interval_id", iv.ID, "entity_type", iv.EntityType, "entity_id", iv.EntityID,
				"started_at", iv.StartedAt, "bad_ended_at", badEndedAt,
				"new_ended_at", newEnd, "collapsed", collapsed)
			continue
		}

		if err := ms.RepairInterval(ctx, iv.ID, iv.StartedAt, newEnd, "state"); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return stats, fmt.Errorf("state interval %d: repair: %w", iv.ID, err)
			}
			// uq_open collision: a valid row already occupies the computed
			// ended_at for this entity. The inverted row is corrupted/
			// zero-information — delete it (state intervals carry no undo table).
			if err := ms.CollapseIntervalToZeroWidth(ctx, iv.ID, "state"); err != nil {
				return stats, fmt.Errorf("state interval %d: delete after collision: %w", iv.ID, err)
			}
			r.log.Info("repair-state-intervals: deleted inverted interval after uq_open collision",
				"interval_id", iv.ID, "entity_type", iv.EntityType, "entity_id", iv.EntityID,
				"started_at", iv.StartedAt)
			stats.Deleted++
			continue
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
	n, err := r.maint.UnrepairIntervals(ctx)
	return int(n), err
}
