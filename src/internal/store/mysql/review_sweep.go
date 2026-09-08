package mysql

import (
	"context"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// sweepAgentID is the fixed, non-live-session identity every wms_journal row
// this sweep writes carries as agent_id (WP3-DESIGN.md Operator Decision 1)
// — the exact value every query below binds as a parameter and matches
// against. A positive match on this identity, never a "not empty" test:
// wms_gc.go stamps "wms-gc" and wms_close.go stamps "wms-close (<user>)",
// both non-empty and both NOT this sweep, so a not-empty test would misfire
// on their actions (§2b). Sourced from wms.ReviewSweepAgentID — the single
// definition cmd/teamster/wms_review_sweep.go's own const and hookd's
// warnMissingRequiredTags guard also read, rather than a duplicated literal
// (wh2-sweep-warning-queue).
const sweepAgentID = wms.ReviewSweepAgentID

// latestStatusRowField returns the correlated scalar subquery that reads one
// column off an entity's own latest field="status" wms_journal row — the
// parking event's writer (column="agent_id") or timestamp (column="created_at").
// Used by every query below that needs "who parked this, and when" (Operator
// Decision 9's discriminator). Same ORDER BY as wms.Reader.GetJournalEntries
// (mysql/store.go's own GetJournalEntries), tiebreak from commit 4c050d6 —
// written once here so all call sites share the identical predicate instead
// of four independently-typed copies that could drift (§2b/§2c: "one rule
// expressed... not two independently-derived implementations").
//
// alias, entityType and column are always Go string literals fixed at the
// call site below, never externally supplied — not a SQL-injection surface.
func latestStatusRowField(alias, entityType, column string) string {
	return `(SELECT j.` + column + ` FROM wms_journal j
		WHERE j.entity_type = '` + entityType + `' AND j.entity_id = ` + alias + `.id AND j.field = 'status'
		ORDER BY j.created_at DESC, j.id DESC LIMIT 1)`
}

// ListReviewSweepCandidates backs WP3-DESIGN.md §2a — review-status
// WorkUnits idle past threshold, Sweep Stage 1's WorkUnit population.
func (s *Store) ListReviewSweepCandidates(ctx context.Context, threshold time.Time) ([]*wms.WorkUnit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+workUnitColumns+`
		FROM workunits
		WHERE status = 'review' AND updated_at < ?
		ORDER BY updated_at ASC`, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*wms.WorkUnit, 0)
	for rows.Next() {
		var wu wms.WorkUnit
		if err := scanWorkUnit(rows, &wu); err != nil {
			return nil, err
		}
		out = append(out, &wu)
	}
	return out, rows.Err()
}

// ListStaleOutcomeCandidates backs WP3-DESIGN.md §2b's candidacy filter —
// non-terminal, non-on_hold Outcomes idle past threshold with no live direct
// WorkUnit or child Outcome. The sweep-parked exemption (round 4) applies to
// both interlock clauses: a human-on_hold child still blocks its parent, a
// sweep-parked one does not. NOT sufficient alone to park anything — the
// caller (wms_review_sweep.go) must also run the descendant-safety walk
// (outcomeHasLiveDescendant, composed from GetOutcomeChildren/GetOutcome/
// ListWorkUnits — §10 keeps that walk out of the Store interface on purpose,
// so there is exactly one implementation of it, not one per backend).
//
// Deliberately NOT built on ListOutcomes(ctx, "", ...) — an empty
// parentOutcomeID there means "root outcomes only" (store_v2.go's own
// LEFT JOIN ... WHERE oe.parent_id IS NULL), not "no filter". That is the
// exact bug WP6's kit text found the hard way.
func (s *Store) ListStaleOutcomeCandidates(ctx context.Context, threshold time.Time) ([]*wms.Outcome, error) {
	wuParkedBy := latestStatusRowField("w", "workunit", "agent_id")
	coParkedBy := latestStatusRowField("co", "outcome", "agent_id")
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.`+outcomeColumns+`
		FROM outcomes o
		WHERE o.status NOT IN ('done', 'abandoned', 'on_hold')
		  AND o.updated_at < ?
		  AND NOT EXISTS (
		        SELECT 1 FROM workunits w
		        WHERE w.outcome_id = o.id
		          AND w.status NOT IN ('done', 'abandoned')
		          AND NOT (w.status = 'on_hold' AND `+wuParkedBy+` = ?)
		      )
		  AND NOT EXISTS (
		        SELECT 1 FROM outcome_edges oe JOIN outcomes co ON co.id = oe.child_id
		        WHERE oe.parent_id = o.id
		          AND co.status NOT IN ('done', 'abandoned')
		          AND NOT (co.status = 'on_hold' AND `+coParkedBy+` = ?)
		      )
		ORDER BY o.updated_at ASC`,
		threshold, sweepAgentID, sweepAgentID)
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

// ListOnHoldAbandonCandidateWorkUnits backs WP3-DESIGN.md §2c — Sweep Stage
// 2's WorkUnit population: on_hold, sweep-parked, no non-sweep journal/
// interval/deliverable activity since the parking row's created_at (T), and
// T at least threshold in the past. A human-parked on_hold WorkUnit has no
// sweep-authored parking row, so it can never match the second condition —
// the ruling's central safety property (a human-set on_hold is never
// touched by Sweep Stage 2, at any idle duration) holds by construction.
func (s *Store) ListOnHoldAbandonCandidateWorkUnits(ctx context.Context, threshold time.Time) ([]*wms.WorkUnit, error) {
	parkedBy := latestStatusRowField("w", "workunit", "agent_id")
	parkedAt := latestStatusRowField("w", "workunit", "created_at")
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.`+workUnitColumns+`
		FROM workunits w
		WHERE w.status = 'on_hold'
		  AND `+parkedBy+` = ?
		  AND `+parkedAt+` < ?
		  AND NOT EXISTS (
		        SELECT 1 FROM wms_journal j2
		        WHERE j2.entity_type = 'workunit' AND j2.entity_id = w.id
		          AND COALESCE(j2.agent_id, '') != ?
		          AND j2.created_at > `+parkedAt+`
		      )
		  AND NOT EXISTS (
		        SELECT 1 FROM wms_intervals iv
		        WHERE iv.entity_type = 'workunit' AND iv.entity_id = w.id
		          AND iv.started_at > `+parkedAt+`
		      )
		  AND NOT EXISTS (
		        SELECT 1 FROM wms_deliverables d
		        WHERE d.entity_type = 'workunit' AND d.entity_id = w.id
		          AND d.created_at > `+parkedAt+`
		      )
		ORDER BY w.id ASC`,
		sweepAgentID, threshold, sweepAgentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*wms.WorkUnit, 0)
	for rows.Next() {
		var wu wms.WorkUnit
		if err := scanWorkUnit(rows, &wu); err != nil {
			return nil, err
		}
		out = append(out, &wu)
	}
	return out, rows.Err()
}

// ListOnHoldAbandonCandidateOutcomes mirrors ListOnHoldAbandonCandidateWorkUnits
// minus the wms_deliverables check — Outcomes never receive a deliverable
// (§14: wms_deliverables.entity_type has never once been 'outcome'). This
// method only narrows the candidate pool: the caller must re-run the
// candidacy filter's interlock and the descendant-safety walk against every
// returned row before abandoning it, since state can change during the
// rescue window (§2c's fifth step, "for Outcomes only").
func (s *Store) ListOnHoldAbandonCandidateOutcomes(ctx context.Context, threshold time.Time) ([]*wms.Outcome, error) {
	parkedBy := latestStatusRowField("o", "outcome", "agent_id")
	parkedAt := latestStatusRowField("o", "outcome", "created_at")
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.`+outcomeColumns+`
		FROM outcomes o
		WHERE o.status = 'on_hold'
		  AND `+parkedBy+` = ?
		  AND `+parkedAt+` < ?
		  AND NOT EXISTS (
		        SELECT 1 FROM wms_journal j2
		        WHERE j2.entity_type = 'outcome' AND j2.entity_id = o.id
		          AND COALESCE(j2.agent_id, '') != ?
		          AND j2.created_at > `+parkedAt+`
		      )
		  AND NOT EXISTS (
		        SELECT 1 FROM wms_intervals iv
		        WHERE iv.entity_type = 'outcome' AND iv.entity_id = o.id
		          AND iv.started_at > `+parkedAt+`
		      )
		ORDER BY o.id ASC`,
		sweepAgentID, threshold, sweepAgentID)
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
