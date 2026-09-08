package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// descendantWalkReader is the minimal read surface outcomeHasLiveDescendant
// (and its isLive/isSweepParked helpers) needs to walk an Outcome's full
// descendant subtree. It is a purpose-built subset of wms.Reader — not
// wms.Reader itself — so a caller with a narrower store interface (wms
// gc's closeStaleEntities/countStaleEntities, which otherwise only need
// ListOutcomes/ListWorkUnits/status-write methods) can satisfy it by adding
// just these three methods instead of adopting the whole wms.Reader surface.
// Any wms.Reader (or a full store.Store) already implements this trivially.
type descendantWalkReader interface {
	GetOutcome(ctx context.Context, id string) (*wms.Outcome, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error)
	GetOutcomeChildren(ctx context.Context, outcomeID string) ([]string, error)
	GetJournalEntries(ctx context.Context, entityType, entityID string, limit int) ([]wms.JournalEntry, error)
}

// errOpBudgetExceeded and errJournalWindowTruncated are the two backstops
// outcomeHasLiveDescendant/isSweepParked fail closed on when they cannot
// finish verifying safety — never silently indistinguishable from a
// genuine "found nothing" answer (NOTE-E, round-4-addendum-2 MAJOR).
var (
	errOpBudgetExceeded       = errors.New("outcomeHasLiveDescendant: operation budget exceeded")
	errJournalWindowTruncated = errors.New("isSweepParked: exhausted the paging hard cap without finding a status row or the end of the entity's journal history")
)

// outcomeHasLiveDescendant reports whether the subtree rooted at rootID
// contains a live entity anywhere below the root — a non-terminal WorkUnit,
// or a descendant Outcome whose OWN status is non-terminal, independent of
// whether that Outcome holds any WorkUnits at all (MAJOR-B: an Outcome can
// be non-terminal and childless, and a walk that inspects only WorkUnit
// status is blind to it) — where "live" excludes a sweep-parked on_hold
// node at either level (round 4: a parent whose only non-terminal
// descendants are entities the sweep itself already parked may still be
// parked; a human-parked descendant still counts as live, via isLive/
// isSweepParked below).
//
// Termination rests on the visited-set alone (a cycle A->B->A visits A,
// visits B, and the third pop of A is already visited and skipped, so
// `next` empties on its own) — no depth cap, which would fail open (return
// "safe" for a subtree the walk had not finished inspecting) for no actual
// safety benefit (MAJOR-A). The operation-budget counter below is a
// corrupted-data backstop only, not a termination mechanism.
//
// Fails closed on every path: an error, and the operation-budget backstop,
// both return true (not false) — this is a safety gate whose false answer
// licenses an irreversible outcome downstream (Sweep Stage 2, wms gc phase
// 4), so "the walk could not finish" must never be indistinguishable from
// "the walk finished and found nothing."
func outcomeHasLiveDescendant(ctx context.Context, r descendantWalkReader, rootID string) (bool, error) {
	visited := map[string]bool{}
	queue := []string{rootID}
	// opBudget is a backstop against a corrupted DAG, not a termination
	// requirement — orders of magnitude beyond any real DAG's node count.
	const opBudget = 100000
	ops := 0
	for len(queue) > 0 {
		var next []string
		for _, id := range queue {
			if visited[id] {
				continue
			}
			visited[id] = true
			ops++
			if ops > opBudget {
				return true, errOpBudgetExceeded
			}

			if id != rootID { // the root's own status is the candidacy filter's concern, not this walk's
				o, err := r.GetOutcome(ctx, id)
				if err != nil {
					return true, err // fail closed
				}
				live, err := isLive(ctx, r, wms.EntityOutcome, id, o.Status)
				if err != nil {
					return true, err // fail closed
				}
				if live {
					return true, nil // MAJOR-B: the descendant Outcome itself is live
				}
			}

			wus, err := r.ListWorkUnits(ctx, id)
			if err != nil {
				return true, err // fail closed
			}
			for _, wu := range wus {
				live, err := isLive(ctx, r, wms.EntityWorkUnit, wu.ID, wu.Status)
				if err != nil {
					return true, err // fail closed
				}
				if live {
					return true, nil // found a live leaf — short-circuit
				}
			}

			children, err := r.GetOutcomeChildren(ctx, id)
			if err != nil {
				return true, err // fail closed
			}
			next = append(next, children...)
		}
		queue = next
	}
	return false, nil
}

// isLive reports whether an entity counts as "live" for interlock/
// descendant-safety purposes (round 4): non-terminal, AND not a
// sweep-parked on_hold. A human-set on_hold entity still counts as live —
// per the operator's ruling, only the sweep's own parking is exempted, so
// a parent of a sweep-parked child may still be parked itself, while a
// parent of a human-parked child never is.
func isLive(ctx context.Context, r descendantWalkReader, entityType, entityID, status string) (bool, error) {
	if wms.IsTerminal(entityType, status) {
		return false, nil
	}
	if status != wms.StatusOnHold {
		return true, nil // non-terminal, not on_hold at all — ordinarily live
	}
	parked, err := isSweepParked(ctx, r, entityType, entityID)
	if err != nil {
		return true, err // fail closed: can't tell who parked it, treat as live
	}
	return !parked, nil // sweep-parked -> not live; human-parked -> live
}

// isSweepParked reports whether entityID's CURRENT on_hold status was set
// by this sweep itself (Operator Decision 9's agent_id discriminator).
// Pages the journal (50, doubling) rather than using a fixed window: a
// short fetch (fewer rows than requested) proves the entity's entire
// journal history was seen, so (false, nil) is a definite answer at any
// history length; a full fetch doubles and tries again, up to a
// corrupted-data backstop, rather than ever guessing "not sweep-parked"
// when the window might have been truncated (round 5 — this design's own
// round-4-addendum-2 fixed limit=90+sentinel version had exactly that
// failure mode once enough non-status rows stacked above the parking row).
// The scan below skips ANY row whose field isn't "status", not just this
// sweep's own sweep_evaluated skip-tracking rows — wh2-required-tags-inherit
// added a second kind of non-status noise (field="deliverable", written on
// redelivery-in-review), and more could exist in the future; the field
// check is already generic, so no code change was needed for that, only
// this comment and the fixtures below correcting a too-narrow description.
// Mirrors the SQL discriminator's own `ORDER BY created_at DESC, id DESC
// LIMIT 1` filtered-to-field='status' semantics exactly, via
// wms.Reader.GetJournalEntries's own tiebreak (commit 4c050d6).
func isSweepParked(ctx context.Context, r descendantWalkReader, entityType, entityID string) (bool, error) {
	const hardCap = 100000
	for limit := 50; ; limit *= 2 {
		if limit > hardCap {
			return false, errJournalWindowTruncated
		}
		entries, err := r.GetJournalEntries(ctx, entityType, entityID, limit)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if e.Field == "status" {
				return e.AgentID == sweepAgentID, nil
			}
		}
		if len(entries) < limit {
			return false, nil // fetched this entity's ENTIRE history, no status row anywhere -> genuinely never parked
		}
	}
}

// walkErrorReason renders walkErr as the one-clause reason text used both
// in an operator-facing stderr log and a durable journal skip row, by every
// caller of outcomeHasLiveDescendant (wms review-sweep, wms gc) — one
// wording, not a copy per caller, per §8 NOTE-E's three-way distinction: a
// genuine live descendant is not an error at all (handled by the caller's
// `live` check); this only ever renders the other two cases, distinguishably,
// since an operator needs to tell "genuinely blocked" apart from "could not
// verify" — they call for different follow-up.
func walkErrorReason(err error) string {
	if errors.Is(err, errOpBudgetExceeded) {
		return "could not verify safety, operation budget exceeded"
	}
	return fmt.Sprintf("could not verify safety: %v", err)
}
