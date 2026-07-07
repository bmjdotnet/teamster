// Package rollup implements the attribution aggregation pipeline that runs
// out-of-process on a timer (see cmd/rollup). It reads the immutable
// token_ledger (Record) plus wms_intervals (kind='focus'), writes usage_attribution
// (Associate, weights summing to 1 per message), then builds cost_rollup
// (Aggregate). It also computes a per-session OTel↔ledger reconciliation that
// is persisted for dashboards. Every step is idempotent: re-running over the
// same data reproduces identical rows.
//
// Runner is backend-agnostic: it holds store.AllocationStore/RecoveryStore/
// SweepStore/MaintenanceStore + wms.Writer, never a raw *sql.DB. The
// row-at-a-time decision logic (which entity to attribute, ranking,
// evidence bookkeeping) lives here in Go so there is exactly one
// implementation of the attribution algorithm to test across backends; the
// set-based aggregations and candidate-selection reads stay behind the store
// interfaces (03-architecture/05-rollup.md).
package rollup

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// entitySpecificity orders WMS entity types from most to least specific. The
// allocator attributes a message to the most specific entity the agent was
// focused on at the message's timestamp; ancestor rollup is a read-time concern.
//
// Both the v1 hierarchy (workitem > task > goal > project) and the v3
// attribution spine (workunit > outcome) are ranked. A type absent from this
// map ranks 0 and is dropped by mostSpecific, so every type that can appear in
// wms_intervals (kind='focus') MUST be listed — an unranked v3 type silently
// routes all of its attribution to the unallocated bucket. WorkUnit is the bounded
// unit of work and is more specific than its Outcome, mirroring task > goal.
var entitySpecificity = map[string]int{
	"workunit": 4, "workitem": 4, // most specific (v3 unit, v1 leaf)
	"task":    3,
	"outcome": 2, "goal": 2, // tactical/strategic resolved at read time
	"project": 1,
}

// mostSpecific returns the most-specific candidate by entity type. ok is false
// when the slice is empty. Pure function so the selection rule is unit-tested
// without a database.
func mostSpecific(cands []store.EntityRef) (ref store.EntityRef, ok bool) {
	best := 0
	for _, c := range cands {
		if rank := entitySpecificity[c.EntityType]; rank > best {
			best, ref, ok = rank, c, true
		}
	}
	return ref, ok
}

// strategicCandidates keeps only the strategic-tier entities (the coordination
// surface the lead owns: v3 outcome, v1 goal/project) so the lead-session
// fallback prefers them over a teammate's narrow leaf workunit/task.
func strategicCandidates(cands []store.EntityRef) []store.EntityRef {
	var out []store.EntityRef
	for _, c := range cands {
		switch c.EntityType {
		case "outcome", "goal", "project":
			out = append(out, c)
		}
	}
	return out
}

// Runner holds the backend-agnostic store surfaces and an optional OTel cost
// source for reconciliation. OTelSource may be nil, in which case
// reconciliation is skipped (allocation and rollup still run). reader/writer
// are the narrow wms.Reader/wms.Writer slices resolveOutcome (GetOutcome/
// GetWorkUnit) and sweep_llm's outcome/tag creation need — narrower than the
// full store.Store, per the kit's "consumers depend on the narrowest
// sub-interface" discipline.
type Runner struct {
	alloc  store.AllocationStore
	rec    store.RecoveryStore
	sweep  store.SweepStore
	maint  store.MaintenanceStore
	reader wms.Reader
	writer wms.Writer
	otel   OTelSource
	log    *slog.Logger
}

// OTelSource returns the authoritative per-session total cost (USD) from OTel,
// keyed by session_id. Implementations query Prometheus; a nil OTelSource
// disables reconciliation.
type OTelSource interface {
	SessionCosts(ctx context.Context) (map[string]float64, error)
}

// NewRunner builds a Runner over the backend-agnostic store surfaces.
// otel may be nil to skip reconciliation.
func NewRunner(alloc store.AllocationStore, rec store.RecoveryStore, sweep store.SweepStore, maint store.MaintenanceStore, reader wms.Reader, writer wms.Writer, otel OTelSource, log *slog.Logger) *Runner {
	return &Runner{alloc: alloc, rec: rec, sweep: sweep, maint: maint, reader: reader, writer: writer, otel: otel, log: log}
}

// Run executes one full pass: allocate, then build cost_rollup, then reconcile.
// A failure in any phase is returned; phases that already wrote are idempotent
// so a retry is safe. When reallocate is true, the run first clears the
// unallocated attribution rows so Allocate re-derives them (see Reallocate) —
// used to recover attribution after a later phase rewrites agent identity.
func (r *Runner) Run(ctx context.Context, reallocate bool) error {
	if reallocate {
		cleared, err := r.Reallocate(ctx)
		if err != nil {
			return fmt.Errorf("reallocate: %w", err)
		}
		r.log.Info("reallocate: cleared unallocated rows for re-derivation", "cleared", cleared)
	}
	allocated, err := r.Allocate(ctx)
	if err != nil {
		return fmt.Errorf("allocate: %w", err)
	}
	if err := r.alloc.BuildCostRollup(ctx); err != nil {
		return fmt.Errorf("cost_rollup: %w", err)
	}
	// OD-1: interval-cost assembly rides inside this pass, AFTER BuildCostRollup,
	// reusing the usage_attribution it just refreshed. cost_rollup (entity/day
	// grain) is untouched; interval cost is an ADDITIONAL projection.
	intervals, err := r.alloc.AssembleIntervalCost(ctx)
	if err != nil {
		return fmt.Errorf("interval_cost: %w", err)
	}
	if err := r.alloc.BuildOutcomeCostRollup(ctx); err != nil {
		return fmt.Errorf("outcome_cost_rollup: %w", err)
	}
	r.log.Info("rollup pass complete", "messages_allocated", allocated, "intervals_costed", intervals)

	if r.otel != nil {
		if n, err := r.Reconcile(ctx); err != nil {
			// Reconciliation is a monitor; a Prometheus hiccup must not fail the
			// pass that already wrote allocation + rollup.
			r.log.Warn("reconcile failed (allocation/rollup already written)", "error", err)
		} else {
			r.log.Info("reconcile complete", "sessions", n)
		}
	}
	return nil
}

// Allocate writes one usage_attribution row per token_ledger message that does
// not yet have one. For each message it finds the agent's focus interval
// covering the message timestamp and attributes weight 1.0 to the most-specific
// entity; messages with no covering interval (including the unbridgeable
// legacy-* rows, which carry no agent) get a weight-1.0 unallocated row. This
// guarantees SUM(weight)=1 per message_id by construction — one row, weight 1.
//
// It is incremental: only message_ids absent from usage_attribution are
// processed, so re-running is cheap and idempotent.
func (r *Runner) Allocate(ctx context.Context) (int, error) {
	batch, err := r.alloc.UnattributedMessages(ctx, 0)
	if err != nil {
		return 0, err
	}

	n := 0
	var failed int
	var firstErr error
	for _, p := range batch {
		etype, eid, method := "", "", "unallocated"
		// intervalID is the covering wms_intervals (kind='state') row for the
		// resolved entity; nil = "no interval covers ts" (harmless — the cost is
		// still entity-attributed in cost_rollup, just not interval-/phase-costed).
		var intervalID *int64
		// The "unknown" sentinel can never have opened a focus interval, so skip
		// the per-message FocusEntityAt query and route straight to unallocated
		// (elides ~13K pointless queries on a large backfill, 84% of rows). The
		// empty agent IS the lead and DOES open ''-keyed focus intervals, so it
		// runs FocusEntityAt — see isAttributable.
		if isAttributable(p.AgentName) && p.SessionID != "" {
			ref, ok, err := r.alloc.FocusEntityAt(ctx, p.SessionID, p.AgentName, p.Timestamp)
			if err != nil {
				// Log-and-continue: one poison message must not starve the rest of
				// the batch. The anti-join re-picks any skipped message next run.
				r.log.Warn("focusAt failed; leaving message for next pass",
					"message_id", p.MessageID, "error", err)
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if ok {
				etype, eid, method = ref.EntityType, ref.EntityID, "temporal_join"
			} else if p.AgentName != "" {
				// P2 lead-focus fallback. The message's own agent had NO covering
				// focus interval at ts, but the agent is not the lead. An ephemeral
				// subagent (e.g. "@general-purpose", spawned in solo mode) never
				// opens a focus interval of its own and exits too fast to — yet it
				// runs under, and bills against, the lead's focused entity. Inherit
				// the lead's (agent_name='') focus in the SAME session so that cost
				// attributes to what the lead was working on rather than dropping to
				// unallocated.
				lref, lok, lerr := r.alloc.FocusEntityAt(ctx, p.SessionID, "", p.Timestamp)
				if lerr != nil {
					r.log.Warn("lead-fallback focusAt failed; leaving message for next pass",
						"message_id", p.MessageID, "error", lerr)
					failed++
					if firstErr == nil {
						firstErr = lerr
					}
					continue
				}
				if lok {
					etype, eid, method = lref.EntityType, lref.EntityID, "temporal_join_lead_fallback"
				}
			} else {
				// P1a lead-session fallback. The LEAD itself (agent_name="") had no
				// covering focus interval of its own — the dominant unallocated bucket
				// (~52% of unallocated cost): the lead coordinates between dispatches
				// and often never opens a strategic interval, yet it bills against
				// whatever the SESSION was working on.
				sref, sok, serr := r.alloc.FocusEntityInSession(ctx, p.SessionID, p.Timestamp)
				if serr != nil {
					r.log.Warn("lead-session-fallback focusInSession failed; leaving message for next pass",
						"message_id", p.MessageID, "error", serr)
					failed++
					if firstErr == nil {
						firstErr = serr
					}
					continue
				}
				if sok {
					etype, eid, method = sref.EntityType, sref.EntityID, "temporal_join_lead_session_fallback"
				}
			}
			if method != "unallocated" {
				// Resolve the interval on the RESOLVED ENTITY (not the agent) — the
				// agent who opened the interval is not necessarily the one incurring
				// this cost (SB-2). A miss leaves intervalID = nil, which is correct.
				id, iok, ierr := r.alloc.StateIntervalAt(ctx, etype, eid, p.Timestamp)
				if ierr != nil {
					r.log.Warn("intervalAt failed; leaving message for next pass",
						"message_id", p.MessageID, "error", ierr)
					failed++
					if firstErr == nil {
						firstErr = ierr
					}
					continue
				}
				if iok {
					intervalID = &id
				}
			}
		}
		if err := r.alloc.ApplyAttribution(ctx, p.MessageID, method, store.EntityRef{EntityType: etype, EntityID: eid}, intervalID); err != nil {
			r.log.Warn("attribution insert failed; leaving message for next pass",
				"message_id", p.MessageID, "error", err)
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	if failed > 0 {
		return n, fmt.Errorf("allocate: %d/%d messages failed (first: %w)", failed, len(batch), firstErr)
	}
	return n, nil
}

// isAttributable reports whether an agent name can possibly own a focus
// interval. The literal "unknown" sentinel never opens an interval (it is the
// legacy-backfill placeholder for messages whose agent could not be recovered),
// so a focus query for it is guaranteed to miss — it falls straight to
// unallocated, eliding ~13K pointless queries on a large backfill (84% of rows).
//
// The EMPTY agent, by contrast, IS the lead — in BOTH solo and team mode — and
// the lead DOES open focus intervals (keyed agent_name=''). Excluding "" here
// short-circuited every lead message to unallocated before FocusEntityAt ever
// ran, the exact twin of the B0 classifier bug. So "" must be attributable:
// let it query the ''-keyed focus intervals it opened.
func isAttributable(agentName string) bool {
	return agentName != "unknown"
}

// Reallocate deletes the usage_attribution rows that landed in the unallocated
// bucket so the next Allocate re-derives them. It is scoped to
// method='unallocated' ONLY — a correctly attributed (temporal_join) row is
// never touched, so this cannot disturb good attribution or introduce double-
// counting.
func (r *Runner) Reallocate(ctx context.Context) (int, error) {
	n, err := r.alloc.DeleteAttributionByMethod(ctx, "unallocated")
	return int(n), err
}

// BuildCostRollup rebuilds the cost_rollup fact table. The int return is
// always 0 (the pre-port RowsAffected count is no longer available once
// BuildCostRollup uses AtomicReplace instead of TRUNCATE+INSERT — see
// AllocationStore.BuildCostRollup) but is kept as a return value so existing
// callers/tests that discard it via `_` keep compiling.
func (r *Runner) BuildCostRollup(ctx context.Context) (int, error) {
	return 0, r.alloc.BuildCostRollup(ctx)
}

// BuildOutcomeCostRollup rebuilds the outcome_cost_rollup fact table. See
// BuildCostRollup's doc comment on the dummy int return.
func (r *Runner) BuildOutcomeCostRollup(ctx context.Context) (int, error) {
	return 0, r.alloc.BuildOutcomeCostRollup(ctx)
}

// Reconcile compares, per session, the ledger cost sum against the OTel
// authoritative total and persists the result to session_reconciliation for
// dashboards. Fetches OTel costs itself (r.otel must be non-nil — Run only
// calls this when it is) and delegates the ledger-vs-OTel upsert to
// AllocationStore.Reconcile.
func (r *Runner) Reconcile(ctx context.Context) (int, error) {
	otelCosts, err := r.otel.SessionCosts(ctx)
	if err != nil {
		return 0, err
	}
	n, err := r.alloc.Reconcile(ctx, otelCosts)
	return int(n), err
}

// AssembleIntervalCost projects the message-grain ledger onto the interval
// grain. See AllocationStore.AssembleIntervalCost for the conservation and
// idempotency guarantees.
func (r *Runner) AssembleIntervalCost(ctx context.Context) (int64, error) {
	return r.alloc.AssembleIntervalCost(ctx)
}

// ReassembleIntervals is the opt-in historical backfill for cost-by-phase.
// See AllocationStore.ReassembleIntervals.
func (r *Runner) ReassembleIntervals(ctx context.Context) (int64, error) {
	return r.alloc.ReassembleIntervals(ctx)
}
