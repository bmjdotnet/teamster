// Package rollup implements the attribution aggregation pipeline that runs
// out-of-process on a timer (see cmd/rollup). It reads the immutable
// token_ledger (Record) plus wms_intervals (kind='focus'), writes usage_attribution
// (Associate, weights summing to 1 per message), then builds cost_rollup
// (Aggregate). It also computes a per-session OTel↔ledger reconciliation that
// is persisted for dashboards. Every step is idempotent: re-running over the
// same data reproduces identical rows.
package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
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

// focusCandidate is one (entity_type, entity_id) interval covering a timestamp.
type focusCandidate struct {
	entityType string
	entityID   string
}

// mostSpecific returns the most-specific candidate by entity type. ok is false
// when the slice is empty. Pure function so the selection rule is unit-tested
// without a database.
func mostSpecific(cands []focusCandidate) (etype, eid string, ok bool) {
	best := 0
	for _, c := range cands {
		if rank := entitySpecificity[c.entityType]; rank > best {
			best, etype, eid, ok = rank, c.entityType, c.entityID, true
		}
	}
	return etype, eid, ok
}

// Runner holds the database handle and an optional OTel cost source for
// reconciliation. OTelSource may be nil, in which case reconciliation is
// skipped (allocation and rollup still run).
type Runner struct {
	db   *sql.DB
	otel OTelSource
	log  *slog.Logger
}

// OTelSource returns the authoritative per-session total cost (USD) from OTel,
// keyed by session_id. Implementations query Prometheus; a nil OTelSource
// disables reconciliation.
type OTelSource interface {
	SessionCosts(ctx context.Context) (map[string]float64, error)
}

// New builds a Runner. otel may be nil to skip reconciliation.
func New(db *sql.DB, otel OTelSource, log *slog.Logger) *Runner {
	return &Runner{db: db, otel: otel, log: log}
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
	rows, err := r.BuildCostRollup(ctx)
	if err != nil {
		return fmt.Errorf("cost_rollup: %w", err)
	}
	// OD-1: interval-cost assembly rides inside this pass, AFTER BuildCostRollup,
	// reusing the usage_attribution it just refreshed. cost_rollup (entity/day
	// grain) is untouched; interval cost is an ADDITIONAL projection.
	intervals, err := r.AssembleIntervalCost(ctx)
	if err != nil {
		return fmt.Errorf("interval_cost: %w", err)
	}
	outcomeRows, err := r.BuildOutcomeCostRollup(ctx)
	if err != nil {
		return fmt.Errorf("outcome_cost_rollup: %w", err)
	}
	r.log.Info("rollup pass complete", "messages_allocated", allocated, "rollup_rows", rows, "intervals_costed", intervals, "outcome_cost_rows", outcomeRows)

	if r.otel != nil {
		n, err := r.Reconcile(ctx)
		if err != nil {
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
	const q = `
		SELECT t.message_id, t.session_id, t.agent_name, t.timestamp
		FROM token_ledger t
		LEFT JOIN usage_attribution ua ON ua.message_id = t.message_id
		WHERE ua.message_id IS NULL`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer rows.Close() //nolint:errcheck

	type pending struct {
		messageID string
		sessionID string
		agentName string
		ts        time.Time
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.messageID, &p.sessionID, &p.agentName, &p.ts); err != nil {
			return 0, err
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	n := 0
	var failed int
	var firstErr error
	for _, p := range batch {
		etype, eid, method := "", "", "unallocated"
		// intervalID is the covering wms_intervals (kind='state') row for the
		// resolved entity; 0 = "no interval covers ts" (harmless — the cost is still
		// entity-attributed in cost_rollup, just not interval-/phase-costed).
		var intervalID uint64
		// The "unknown" sentinel can never have opened a focus interval, so skip
		// the per-message focusAt query and route straight to unallocated (elides
		// ~13K pointless queries on a large backfill, 84% of rows). The empty
		// agent IS the lead and DOES open ''-keyed focus intervals, so it runs
		// focusAt — see isAttributable.
		if isAttributable(p.agentName) && p.sessionID != "" {
			et, ei, ok, err := r.focusAt(ctx, p.sessionID, p.agentName, p.ts)
			if err != nil {
				// Log-and-continue: one poison message must not starve the rest of
				// the batch. The anti-join re-picks any skipped message next run.
				r.log.Warn("focusAt failed; leaving message for next pass",
					"message_id", p.messageID, "error", err)
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if ok {
				etype, eid, method = et, ei, "temporal_join"
			} else if p.agentName != "" {
				// P2 lead-focus fallback. The message's own agent had NO covering
				// focus interval at ts, but the agent is not the lead. An ephemeral
				// subagent (e.g. "@general-purpose", spawned in solo mode) never
				// opens a focus interval of its own and exits too fast to — yet it
				// runs under, and bills against, the lead's focused entity. Inherit
				// the lead's (agent_name='') focus in the SAME session so that cost
				// attributes to what the lead was working on rather than dropping to
				// unallocated.
				//
				// Scoping (no team-mode regression): this fires ONLY when the agent
				// has no own covering interval. A named teammate that DID set its
				// own focus took the ok branch above and never reaches here, so its
				// cost still lands on its own entity. A team-mode teammate that
				// never set focus DOES fall back to the lead's entity — which is
				// strictly better than the unallocated bucket it lands in today, and
				// is the correct "doing the lead's bidding" attribution. The fallback
				// is gated on agentName != "" so the lead's own miss is not re-queried
				// against itself (it would return the same miss).
				let, lei, lok, lerr := r.focusAt(ctx, p.sessionID, "", p.ts)
				if lerr != nil {
					r.log.Warn("lead-fallback focusAt failed; leaving message for next pass",
						"message_id", p.messageID, "error", lerr)
					failed++
					if firstErr == nil {
						firstErr = lerr
					}
					continue
				}
				if lok {
					etype, eid, method = let, lei, "temporal_join_lead_fallback"
				}
			} else {
				// P1a lead-session fallback. The LEAD itself (agent_name="") had no
				// covering focus interval of its own — the dominant unallocated bucket
				// (~52% of unallocated cost): the lead coordinates between dispatches
				// and often never opens a strategic interval, yet it bills against
				// whatever the SESSION was working on. The teammate→lead fallback above
				// is gated on agentName != "" (re-querying focusAt(session,"",ts) for
				// the lead would just return the same miss), so the lead's own miss
				// needs a DIFFERENT source: the entity the SESSION had focus on at ts
				// under ANY agent.
				//
				// Rule (§7.3): "session focus regardless of agent, prefer Outcome."
				// Among all kind='focus' intervals covering ts in this session (any
				// agent), prefer the most-specific *Outcome* — the lead's role is
				// strategic/cross-cutting coordination, so attributing its spend to a
				// teammate's narrow WorkUnit would mis-place cross-cutting work. Only
				// when no Outcome covers ts do we fall to the most-specific other
				// covering entity. This fires ONLY on an otherwise-unallocated lead
				// message, moves a dollar from "" to a real entity (conservation holds),
				// and needs no transcript machinery — it runs every rollup pass.
				set, sei, sok, serr := r.focusInSession(ctx, p.sessionID, p.ts)
				if serr != nil {
					r.log.Warn("lead-session-fallback focusInSession failed; leaving message for next pass",
						"message_id", p.messageID, "error", serr)
					failed++
					if firstErr == nil {
						firstErr = serr
					}
					continue
				}
				if sok {
					etype, eid, method = set, sei, "temporal_join_lead_session_fallback"
				}
			}
			if method != "unallocated" {
				// Resolve the interval on the RESOLVED ENTITY (not the agent) — the
				// agent who opened the interval is not necessarily the one incurring
				// this cost (SB-2). A miss leaves intervalID = 0, which is correct.
				id, iok, ierr := r.intervalAt(ctx, etype, eid, p.ts)
				if ierr != nil {
					r.log.Warn("intervalAt failed; leaving message for next pass",
						"message_id", p.messageID, "error", ierr)
					failed++
					if firstErr == nil {
						firstErr = ierr
					}
					continue
				}
				if iok {
					intervalID = id
				}
			}
		}
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO usage_attribution
				(message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
			 VALUES (?, ?, ?, 1.00000, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
				entity_type = VALUES(entity_type),
				entity_id   = VALUES(entity_id),
				weight      = VALUES(weight),
				method      = VALUES(method),
				computed_at = VALUES(computed_at),
				interval_id = VALUES(interval_id)`,
			p.messageID, etype, eid, method, now, intervalID,
		); err != nil {
			r.log.Warn("attribution insert failed; leaving message for next pass",
				"message_id", p.messageID, "error", err)
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
// so a focusAt query for it is guaranteed to miss — it falls straight to
// unallocated, eliding ~13K pointless queries on a large backfill (84% of rows).
//
// The EMPTY agent, by contrast, IS the lead — in BOTH solo and team mode — and
// the lead DOES open focus intervals (keyed agent_name=''). Excluding "" here
// short-circuited every lead message to unallocated before focusAt ever ran,
// the exact twin of the B0 classifier bug (classifier.go: the old
// `AgentName != ""` clause starved a lead-only session of all work-type signal).
// So "" must be attributable: let it query the ''-keyed focus intervals it opened.
func isAttributable(agentName string) bool {
	return agentName != "unknown"
}

// Reallocate deletes the usage_attribution rows that landed in the unallocated
// bucket so the next Allocate re-derives them. It is scoped to
// method='unallocated' ONLY — a correctly attributed (temporal_join) row is
// never touched, so this cannot disturb good attribution or introduce double-
// counting. The deletion drops each row's anti-join shadow, so the immediately
// following Allocate re-processes exactly those messages with the current agent
// identity and focus intervals (e.g. after a later phase rewrites
// token_ledger.agent_name from "unknown" to a real teammate name). A message
// whose timestamp still has no covering interval simply returns to the
// unallocated bucket — idempotent and safe to run repeatedly.
func (r *Runner) Reallocate(ctx context.Context) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = 'unallocated'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// focusAt returns the most-specific entity the agent was focused on at time ts.
// Multiple intervals can cover ts (e.g. a goal still open while a task is also
// open); we pick the most specific by entity type. ok is false when no interval
// covers ts.
//
// The agent_name match is @-prefix-insensitive on BOTH sides (defense-in-depth):
// the two writers should agree (scraper and focus-writer both emit "@name"), but
// normalizing here means a future namespace drift can't silently re-route all
// attribution to the unallocated bucket. We compare on the bare name.
func (r *Runner) focusAt(ctx context.Context, sessionID, agentName string, ts time.Time) (etype, eid string, ok bool, err error) {
	// Exclude brief_directive intervals: those are a focus-less teammate's
	// INTENDED focus, recovered separately (and reversibly) by RecoverDirective
	// with method='brief_directive_recovery'. Letting Allocate's focusAt consume
	// them would write indistinguishable, non-reversible temporal_join rows.
	const q = `
		SELECT entity_type, entity_id
		FROM wms_intervals
		WHERE kind = 'focus'
		  AND identity_source <> 'brief_directive'
		  AND session_id = ?
		  AND TRIM(LEADING '@' FROM agent_name) = ?
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)`
	rows, qerr := r.db.QueryContext(ctx, q, sessionID, strings.TrimPrefix(agentName, "@"), ts, ts)
	if qerr != nil {
		return "", "", false, qerr
	}
	defer rows.Close() //nolint:errcheck

	var cands []focusCandidate
	for rows.Next() {
		var c focusCandidate
		if err := rows.Scan(&c.entityType, &c.entityID); err != nil {
			return "", "", false, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	etype, eid, ok = mostSpecific(cands)
	return etype, eid, ok, nil
}

// focusInSession returns the entity the SESSION had focus on at time ts, across
// ALL agents (not a single agent_name like focusAt). It is the P1a lead-session
// fallback source: when the lead (agent_name="") has no covering interval of its
// own, its coordination cost is attributed to whatever the session was working
// on at that instant rather than dropping to the unallocated bucket.
//
// Selection rule (§7.3 "session focus regardless of agent, prefer Outcome"):
// the lead's role is strategic, cross-cutting coordination, so we PREFER the
// most-specific covering Outcome over an arbitrary child WorkUnit — attributing
// the lead's spend to a teammate's narrow WorkUnit would mis-place cross-cutting
// work. Only when NO Outcome (or goal/project — the strategic tier) covers ts do
// we fall back to the most-specific covering entity of any type. ok is false when
// no interval covers ts in the session.
//
// agent_name is deliberately NOT filtered: we want the session's focus under any
// agent. We still pick deterministically (most-specific entity type, then the
// most recently started interval) so a re-run reproduces the same attribution.
func (r *Runner) focusInSession(ctx context.Context, sessionID string, ts time.Time) (etype, eid string, ok bool, err error) {
	const q = `
		SELECT entity_type, entity_id
		FROM wms_intervals
		WHERE kind = 'focus'
		  AND identity_source <> 'brief_directive'
		  AND session_id = ?
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)
		ORDER BY started_at DESC`
	rows, qerr := r.db.QueryContext(ctx, q, sessionID, ts, ts)
	if qerr != nil {
		return "", "", false, qerr
	}
	defer rows.Close() //nolint:errcheck

	var cands []focusCandidate
	for rows.Next() {
		var c focusCandidate
		if err := rows.Scan(&c.entityType, &c.entityID); err != nil {
			return "", "", false, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}

	// Prefer the strategic tier (outcome/goal/project) the lead actually
	// coordinates, picking the most specific within it; fall back to the overall
	// most-specific entity only when no strategic interval covers ts. The DESC
	// ordering above makes mostSpecific deterministic on ties (first-seen wins,
	// i.e. most recently started).
	if et, ei, sok := mostSpecific(strategicCandidates(cands)); sok {
		return et, ei, true, nil
	}
	etype, eid, ok = mostSpecific(cands)
	return etype, eid, ok, nil
}

// strategicCandidates keeps only the strategic-tier entities (the coordination
// surface the lead owns: v3 outcome, v1 goal/project) so the lead-session
// fallback prefers them over a teammate's narrow leaf workunit/task.
func strategicCandidates(cands []focusCandidate) []focusCandidate {
	var out []focusCandidate
	for _, c := range cands {
		switch c.entityType {
		case "outcome", "goal", "project":
			out = append(out, c)
		}
	}
	return out
}

// intervalAt returns the wms_intervals (kind='state') interval that covers ts for
// the given entity. The entity was already resolved by focusAt (agent → focused
// entity); intervalAt finds THAT entity's covering interval purely on
// (entity_type, entity_id) and the time window — deliberately NOT on agent_name.
//
// wms_intervals.agent_name (kind='state') records who OPENED the interval (the
// transitioning agent), which is not necessarily the agent incurring the cost
// message at ts: a teammate can run under a workunit whose current state interval
// a different agent opened. Filtering by agent_name would drop that cost to
// interval_id=0 and silently shrink cost-by-phase, so the lookup is entity-scoped
// only (SB-2).
//
// When several state intervals cover ts (shouldn't happen — uq_open keeps ≤1
// open per entity, and closed intervals don't overlap — but be defensive) we pick
// the most recently started. ok is false when no interval covers ts.
func (r *Runner) intervalAt(ctx context.Context, entityType, entityID string, ts time.Time) (id uint64, ok bool, err error) {
	const q = `
		SELECT id
		FROM wms_intervals
		WHERE kind = 'state'
		  AND entity_type = ?
		  AND entity_id = ?
		  AND started_at <= ?
		  AND (ended_at IS NULL OR ended_at > ?)
		ORDER BY started_at DESC
		LIMIT 1`
	row := r.db.QueryRowContext(ctx, q, entityType, entityID, ts, ts)
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	return id, true, nil
}

// BuildCostRollup rebuilds the cost_rollup fact table from token_ledger joined
// to usage_attribution. cost = SUM(cost_usd * weight) grouped by
// hour/entity/agent/model; the unallocated bucket is entity_type=”” / entity_id=””.
// bucket_day (DATE) is kept for backward compatibility with existing dashboards;
// bucket_hour (DATETIME, truncated to the hour) is the finer grain added in v43.
// The table is fully derived, so a TRUNCATE + rebuild is always safe and keeps
// the result exactly conserved against the ledger.
func (r *Runner) BuildCostRollup(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `TRUNCATE TABLE cost_rollup`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO cost_rollup
			(bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd)
		SELECT
			DATE(t.timestamp)                              AS bucket_day,
			DATE_FORMAT(t.timestamp, '%Y-%m-%d %H:00:00')  AS bucket_hour,
			ua.entity_type                                 AS entity_type,
			ua.entity_id                                   AS entity_id,
			t.agent_name                                   AS agent_name,
			t.model                                        AS model,
			ROUND(SUM(t.total_input * ua.weight))          AS tokens,
			SUM(t.cost_usd * ua.weight)                    AS cost_usd
		FROM token_ledger t
		JOIN usage_attribution ua ON ua.message_id = t.message_id
		GROUP BY bucket_day, bucket_hour, ua.entity_type, ua.entity_id, t.agent_name, t.model`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// BuildOutcomeCostRollup rebuilds the outcome_cost_rollup fact table from
// cost_rollup joined to workunits. Each outcome gets rows decomposed by source:
// source_type='direct' for cost attributed directly to the outcome entity, and
// source_type='workunit' (with source_id=workunit_id) for cost from child
// workunits. The table is fully derived — TRUNCATE + rebuild is always safe and
// stays conserved against cost_rollup (no new cost is created, only re-grouped).
//
// Chained after BuildCostRollup in Run() so the source data is always fresh.
// Post-compute drift from sweep/re-tag is handled by the same rebuild-on-next-
// run guarantee as cost_rollup: any change to usage_attribution triggers a
// cost_rollup rebuild, which triggers this rebuild.
func (r *Runner) BuildOutcomeCostRollup(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `TRUNCATE TABLE outcome_cost_rollup`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO outcome_cost_rollup
			(bucket_day, bucket_hour, outcome_id, source_type, source_id, model, agent_name, tokens, cost_usd)
		SELECT
			cr.bucket_day,
			cr.bucket_hour,
			cr.entity_id                AS outcome_id,
			'direct'                    AS source_type,
			''                          AS source_id,
			cr.model,
			cr.agent_name,
			SUM(cr.tokens)              AS tokens,
			SUM(cr.cost_usd)            AS cost_usd
		FROM cost_rollup cr
		WHERE cr.entity_type = 'outcome'
		GROUP BY cr.bucket_day, cr.bucket_hour, cr.entity_id, cr.model, cr.agent_name

		UNION ALL

		SELECT
			cr.bucket_day,
			cr.bucket_hour,
			w.outcome_id                AS outcome_id,
			'workunit'                  AS source_type,
			cr.entity_id                AS source_id,
			cr.model,
			cr.agent_name,
			SUM(cr.tokens)              AS tokens,
			SUM(cr.cost_usd)            AS cost_usd
		FROM cost_rollup cr
		JOIN workunits w ON w.id = cr.entity_id
		WHERE cr.entity_type = 'workunit'
		GROUP BY cr.bucket_day, cr.bucket_hour, w.outcome_id, cr.entity_id, cr.model, cr.agent_name`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// Reconcile compares, per session, the ledger cost sum against the OTel
// authoritative total and persists the result to session_reconciliation for
// dashboards. Divergence (OTel materially above ledger) is the data-quality
// signal that catches pricing gaps, scraper failures, and un-ingested
// subagents. Read-only against OTel; the only write is the reconciliation table.
func (r *Runner) Reconcile(ctx context.Context) (int, error) {
	otelCosts, err := r.otel.SessionCosts(ctx)
	if err != nil {
		return 0, err
	}

	ledger := map[string]float64{}
	rows, err := r.db.QueryContext(ctx,
		`SELECT session_id, SUM(cost_usd) FROM token_ledger GROUP BY session_id`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var sid string
		var cost float64
		if err := rows.Scan(&sid, &cost); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		ledger[sid] = cost
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Union of sessions seen by either source.
	sessions := map[string]struct{}{}
	for sid := range otelCosts {
		sessions[sid] = struct{}{}
	}
	for sid := range ledger {
		sessions[sid] = struct{}{}
	}

	now := time.Now().UTC()
	n := 0
	for sid := range sessions {
		o := otelCosts[sid]
		l := ledger[sid]
		if _, err := r.db.ExecContext(ctx,
			// otel_cost_usd uses GREATEST so that a session absent from the
			// current Prometheus result (series aged out of retention, exporter
			// already exited) never overwrites a previously recorded non-zero
			// value with 0. The last good reading sticks. Sessions present in
			// this result always win because o > 0 beats any stale 0.
			// divergence_usd is derived from the same GREATEST-resolved otel value
			// so the row stays internally consistent: divergence always equals the
			// stored otel_cost_usd minus the current ledger sum.
			`INSERT INTO session_reconciliation
				(session_id, otel_cost_usd, ledger_cost_usd, divergence_usd, computed_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
				otel_cost_usd   = GREATEST(otel_cost_usd, VALUES(otel_cost_usd)),
				ledger_cost_usd = VALUES(ledger_cost_usd),
				divergence_usd  = GREATEST(otel_cost_usd, VALUES(otel_cost_usd)) - VALUES(ledger_cost_usd),
				computed_at     = VALUES(computed_at)`,
			sid, o, l, o-l, now,
		); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// AssembleIntervalCost projects the message-grain ledger onto the interval grain:
// for every wms_intervals (kind='state') row referenced by a
// usage_attribution.interval_id, it sets cost_usd / cost_tokens to
// SUM(token_ledger.cost_usd * weight) of the messages attributed to that interval,
// plus an assembled_at watermark.
//
// It is TRULY idempotent (SB-3): rather than a plain UPDATE...JOIN (which would
// leave a stale cost on any interval that dropped out of the source), it first
// clears every previously-assembled cost back to NULL, then re-derives from
// source. So a re-run with a since-removed attribution clears the orphan, exactly
// mirroring BuildCostRollup's TRUNCATE+rebuild semantics — running twice over the
// same data reproduces identical rows, and running after attribution changes
// reflects only the live source.
//
// Conservation: token_ledger.uq_message gives one ledger row per message (no
// ledger-side fan-out), each usage_attribution row carries one interval_id and
// one weight, and each interval has exactly one phase (B1 column). Therefore
//
//	Σ wms_intervals.cost_usd (kind='state', interval-attributed)
//	  == Σ usage_attribution(weight·cost) for interval_id ≠ 0
//
// with no double-count across phases. interval_id = 0 rows are excluded; that
// cost is not lost — it remains entity-attributed in cost_rollup.
//
// Returns the number of intervals that received a cost in this pass.
func (r *Runner) AssembleIntervalCost(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Clear-then-reassemble: NULL out any prior assembly so an interval that no
	// longer matches the source loses its stale cost (true idempotency).
	if _, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET cost_usd = NULL, cost_tokens = NULL, assembled_at = NULL
		WHERE kind = 'state'
		  AND (cost_usd IS NOT NULL OR cost_tokens IS NOT NULL OR assembled_at IS NOT NULL)`); err != nil {
		return 0, err
	}

	// v23 already remapped usage_attribution.interval_id from the old
	// wms_event_records.id to the new wms_intervals.id (B3 §3.1 R2), so the join
	// resolves directly against wms_intervals — no re-remap here.
	res, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals wi
		JOIN (
			SELECT ua.interval_id AS interval_id,
			       SUM(t.cost_usd    * ua.weight) AS cost_usd,
			       SUM(t.total_input * ua.weight) AS cost_tokens
			FROM usage_attribution ua
			JOIN token_ledger t ON t.message_id = ua.message_id
			WHERE ua.interval_id <> 0
			GROUP BY ua.interval_id
		) x ON wi.id = x.interval_id AND wi.kind = 'state'
		SET wi.cost_usd    = x.cost_usd,
		    wi.cost_tokens = ROUND(x.cost_tokens),
		    wi.assembled_at = UTC_TIMESTAMP(6)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// ReassembleIntervals is the OPT-IN historical backfill for cost-by-phase. The
// normal pass is FORWARD-ONLY: Allocate is incremental (it only processes
// messages absent from usage_attribution), so attribution rows written before
// the interval_id column existed keep interval_id = 0 and never appear in
// cost-by-phase. On a live database — where essentially all cost is already
// allocated — that means cost-by-phase starts empty and only fills going forward.
//
// This method re-resolves interval_id for every already-attributed row that has a
// real entity but interval_id = 0, then rebuilds interval cost. It is the
// interval-grain analogue of Reallocate: opt-in, run once by the operator to
// populate historical cost-by-phase. It is NOT run automatically every pass (it
// re-queries intervalAt for potentially every historical message). Re-running is
// idempotent — a row that still has no covering interval simply stays at 0.
//
// Returns the number of attribution rows whose interval_id was populated.
func (r *Runner) ReassembleIntervals(ctx context.Context) (int, error) {
	// Only rows with a real entity can have a covering interval; the unallocated
	// bucket (entity_type='') never does, so skip it. Re-resolving an already-set
	// interval_id is unnecessary, so scope to interval_id = 0.
	const q = `
		SELECT ua.message_id, ua.entity_type, ua.entity_id, t.timestamp
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.interval_id = 0
		  AND ua.entity_type <> ''`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return 0, err
	}
	type pending struct {
		messageID  string
		entityType string
		entityID   string
		ts         time.Time
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.messageID, &p.entityType, &p.entityID, &p.ts); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		batch = append(batch, p)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}

	updated := 0
	for _, p := range batch {
		id, ok, err := r.intervalAt(ctx, p.entityType, p.entityID, p.ts)
		if err != nil {
			return updated, err
		}
		if !ok {
			continue // still no covering interval — leave at 0
		}
		if _, err := r.db.ExecContext(ctx,
			`UPDATE usage_attribution SET interval_id = ?
			 WHERE message_id = ? AND entity_type = ? AND entity_id = ?`,
			id, p.messageID, p.entityType, p.entityID); err != nil {
			return updated, err
		}
		updated++
	}

	// Rebuild interval cost so the freshly-populated interval_ids are reflected.
	if _, err := r.AssembleIntervalCost(ctx); err != nil {
		return updated, err
	}
	return updated, nil
}
