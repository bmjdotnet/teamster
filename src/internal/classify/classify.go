// Package classify is the async, timer-driven phase + work-type classifier.
//
// It is the B4 stage: a run-once-and-exit pass (peer of internal/rollup) that
// the systemd timer fires every five minutes. Each pass is idempotent — it
// derives output only for intervals/workunits that need it, so re-running is
// cheap and reproduces identical rows.
//
// Two outputs, two grains (Stream-B design §4.2):
//
//   - phase ON the closed INTERVAL (wms_intervals.phase column). NEW in B4.
//     For each closed, non-declared, unassembled/stale interval the engine
//     reads the same JSONL activity signals the inline classifier uses, maps
//     them to exactly one phase from {design,build,test,review,rework}, and
//     writes it via UpdateEventRecordPhase(id, phase, "classifier"). The
//     declared-wins guard in that UPDATE means a wms_setPhase declaration is
//     never overwritten.
//
//   - work-type ON the WORKUNIT (entity_tags). Unchanged rules — the engine
//     reuses wms.RuleClassifier verbatim; the only change from the inline path
//     is the timer cadence and the re-runnable forward pass.
//
// --reclassify (the Reallocate model) clears classifier-derived phases
// (phase_source='classifier' only — declared phases are never touched) and then
// runs a normal forward pass to re-derive them with the current rules.
package classify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// Store is the persistence surface the classifier needs. *mysql.Store satisfies
// it; tests use a fake. It is a subset of the full wms.Store plus the two
// additive B4 queries (ListIntervalsNeedingPhase, ClearClassifierPhases) and
// the work-type enumeration (ListWorkUnitsNeedingWorkType).
type Store interface {
	wms.Store
	ListIntervalsNeedingPhase(ctx context.Context, limit int) ([]wms.EventRecord, error)
	ClearClassifierPhases(ctx context.Context) (int64, error)
	ListWorkUnitsNeedingWorkType(ctx context.Context, jobName string) ([]string, error)
	// ListOutcomesNeedingPhase returns [outcomeID, workType] pairs for outcomes
	// with no phase tag and no child workunits.
	ListOutcomesNeedingPhase(ctx context.Context) ([][2]string, error)
	MarkIntervalAssembled(ctx context.Context, id int64) error
	// EarliestClosureByEntity returns each entity's first review/done start time,
	// keyed by [entity_type, entity_id], so cross-batch rework is detectable.
	EarliestClosureByEntity(ctx context.Context, keys [][2]string) (map[[2]string]time.Time, error)
	// ListWorkUnitsNeedingLifecycleTags returns [workunitID, missingKey,
	// existingWorkType] triples for work units missing any required lifecycle
	// tag (category='lifecycle' AND required=1 in the tags table).
	// existingWorkType carries the work unit's current work-type value (empty
	// when unset) so the caller can derive the right phase default without a
	// second query.
	ListWorkUnitsNeedingLifecycleTags(ctx context.Context) ([][3]string, error)
	// RecordJobHeartbeat stamps classify's last-completed-run timestamp,
	// independent of whether this pass produced any other write. Backs the
	// "Classify Freshness" dashboard panel.
	RecordJobHeartbeat(ctx context.Context, jobName string, at time.Time) error
}

// jobNameClassify is this job's key in job_heartbeats.
const jobNameClassify = "classify"

// intervalBatch caps how many intervals one phase pass derives, so a single run
// stays bounded; the next timer tick picks up the rest.
const intervalBatch = 500

// Runner executes one classification pass.
type Runner struct {
	store      Store
	signals    wms.SignalReader
	classifier wms.Classifier
	logFile    string
	log        *slog.Logger
}

// New builds a Runner. logFile is the JSONL event log (config.LogFile); the
// classifier reads it for activity signals, so the classifier must run on the
// same host as hookd (the hub).
func New(store Store, sr wms.SignalReader, logFile string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		store:      store,
		signals:    sr,
		classifier: wms.NewRuleClassifier(store, sr, logFile),
		logFile:    logFile,
		log:        log,
	}
}

// DefaultReclassifyLimit is the total-intervals circuit breaker for the
// reclassify draining loop when no override is provided.
const DefaultReclassifyLimit = 5000

// Run executes one pass: derive work-type on workunits, recover missing
// required lifecycle tags on work units, then phase on closed intervals. When
// reclassify is true it first clears classifier-derived phases and then loops
// classifyPhases until the backlog is drained or the circuit breaker
// (reclassifyLimit total intervals) trips. reclassifyLimit is ignored on the
// normal (non-reclassify) path, which always processes one batch.
//
// dryRun suppresses all writes in the recoverRequiredTags pass and logs what
// would be applied instead; other passes are unaffected.
func (r *Runner) Run(ctx context.Context, reclassify bool, reclassifyLimit int, dryRun bool) error {
	if reclassify {
		n, err := r.store.ClearClassifierPhases(ctx)
		if err != nil {
			return fmt.Errorf("reclassify: clear classifier phases: %w", err)
		}
		r.log.Info("reclassify: cleared classifier-derived phases for re-derivation", "rows", n)
	}

	if err := r.classifyWorkTypes(ctx); err != nil {
		// A work-type failure must not abort the phase pass — they are
		// independent outputs. Log and continue.
		r.log.Error("work-type pass failed (continuing to phase pass)", "error", err)
	}

	if err := r.classifyOutcomePhases(ctx); err != nil {
		r.log.Error("outcome-phase safety-net failed (continuing to lifecycle recovery pass)", "error", err)
	}

	if err := r.recoverRequiredTags(ctx, dryRun); err != nil {
		r.log.Error("lifecycle tag recovery failed (continuing to interval phase pass)", "error", err)
	}

	if reclassify {
		total := 0
		for {
			derived, err := r.classifyPhases(ctx)
			if err != nil {
				return fmt.Errorf("phase pass: %w", err)
			}
			total += derived
			if derived == 0 {
				break
			}
			if total >= reclassifyLimit {
				r.log.Warn("reclassify circuit breaker: processed N intervals, more may remain — increase TEAMSTER_RECLASSIFY_LIMIT or run again",
					"processed", total, "limit", reclassifyLimit)
				break
			}
		}
		r.log.Info("reclassify pass complete", "intervals_phased", total)
		r.recordHeartbeat(ctx)
		return nil
	}

	derived, err := r.classifyPhases(ctx)
	if err != nil {
		return fmt.Errorf("phase pass: %w", err)
	}
	r.log.Info("classify pass complete", "intervals_phased", derived)
	r.recordHeartbeat(ctx)
	return nil
}

// recordHeartbeat stamps this pass's completion time. Called only on Run's
// success paths, so a genuinely failing classify (e.g. DB errors on every
// pass) still shows as stale on the freshness dashboard rather than masking
// the failure. A heartbeat write failure itself is logged, not propagated —
// the pass's real work already committed and must not be undone by a
// secondary write's error.
func (r *Runner) recordHeartbeat(ctx context.Context) {
	if err := r.store.RecordJobHeartbeat(ctx, jobNameClassify, time.Now()); err != nil {
		r.log.Warn("heartbeat stamp failed", "job", jobNameClassify, "error", err)
	}
}

// classifyWorkTypes re-derives work-type on every workunit that needs it,
// reusing the inline RuleClassifier rules verbatim. Watermarked on
// job_heartbeats (jobNameClassify), not on the work-type tag's own
// applied_at (GH #13 follow-up, two live-diagnosed rounds): the prior
// unwatermarked version re-scanned every active workunit (2253 on this
// install) each tick, each scan a full JSONL read via RuleClassifier.Classify,
// driving a 23-minute pass that overran its own 10-minute timer. An
// applied_at-based watermark fails in both directions — a workunit with no
// derivable signal never gets a tag written at all (nothing to satisfy the
// watermark), and a workunit that keeps re-deriving the SAME work-type value
// never advances applied_at (TagEntity only bumps it on a value change) — so
// both cohorts (1976 no-tag, then all 1988 already-tagged) were reselected
// forever. job_heartbeats.last_run_at sidesteps both: a workunit is selected
// only when there's no heartbeat yet (first run) or its latest closed
// interval ended after the last completed run. The classifier's manualKeys
// deference protects operator-set work-type values. A per-workunit error is
// logged and skipped so one bad entity does not stop the rest.
func (r *Runner) classifyWorkTypes(ctx context.Context) error {
	ids, err := r.store.ListWorkUnitsNeedingWorkType(ctx, jobNameClassify)
	if err != nil {
		return fmt.Errorf("list workunits: %w", err)
	}
	applied := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := r.classifier.Classify(ctx, wms.EntityWorkUnit, id)
		if err != nil {
			r.log.Warn("work-type classify failed for workunit", "workunit", id, "error", err)
			continue
		}
		applied += len(res.Applied)
	}
	r.log.Info("work-type pass complete", "workunits", len(ids), "tags_applied", applied)
	return nil
}

// workTypeToPhase maps a work-type tag value to its default lifecycle phase.
// Used by the outcome-phase safety net for outcomes that have no workunits and
// therefore cannot acquire phase via the view's promotion leg.
var workTypeToPhase = map[string]string{
	"feature":  "build",
	"bug":      "build",
	"infra":    "build",
	"refactor": "build",
	"research": "design",
	"docs":     "design",
	"test":     "test",
}

// classifyOutcomePhases is the safety net: outcomes with no workunits and no
// phase tag get a rule-based default derived from their work-type. This catches
// synthesized outcomes the sweep created without phase, and MCP-created
// outcomes that never had child workunits. It respects manual tags — outcomes
// already tagged with phase (any source) are excluded by the query.
func (r *Runner) classifyOutcomePhases(ctx context.Context) error {
	candidates, err := r.store.ListOutcomesNeedingPhase(ctx)
	if err != nil {
		return fmt.Errorf("list outcomes needing phase: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}
	applied := 0
	for _, pair := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		outcomeID, workType := pair[0], pair[1]
		phase := workTypeToPhase[workType]
		if phase == "" {
			phase = "build"
		}
		if err := r.store.TagEntity(ctx, "outcome", outcomeID, "phase", phase, "classifier", ""); err != nil {
			r.log.Warn("outcome-phase tag failed", "outcome", outcomeID, "phase", phase, "error", err)
			continue
		}
		applied++
	}
	r.log.Info("outcome-phase safety-net complete", "candidates", len(candidates), "applied", applied)
	return nil
}

// recoverRequiredTags is the safety net for work units created without required
// lifecycle tags. It finds each (workunit, missing-key) gap and applies a
// rule-based default. Only lifecycle-category required keys are targeted —
// context keys (product, component, etc.) require operator knowledge and are
// left to the dispatch lead.
//
// Current defaults:
//   - phase: derived from the work unit's existing work-type via workTypeToPhase;
//     falls back to "build" when work-type is absent or unrecognised.
//   - work-type: skipped — classifyWorkTypes already handles work units with
//     activity, and work units with no activity cannot be rule-classified.
//
// dryRun suppresses all writes and logs what WOULD be applied instead.
func (r *Runner) recoverRequiredTags(ctx context.Context, dryRun bool) error {
	triples, err := r.store.ListWorkUnitsNeedingLifecycleTags(ctx)
	if err != nil {
		return fmt.Errorf("list workunits needing lifecycle tags: %w", err)
	}
	if len(triples) == 0 {
		return nil
	}
	applied, skipped := 0, 0
	for _, triple := range triples {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		workunitID, missingKey, workType := triple[0], triple[1], triple[2]
		switch missingKey {
		case "phase":
			phase := workTypeToPhase[workType]
			if phase == "" {
				phase = "build"
			}
			if dryRun {
				r.log.Info("recover-required-tags (dry-run): would apply",
					"workunit", workunitID, "key", missingKey, "value", phase)
				applied++
				continue
			}
			if err := r.store.TagEntity(ctx, "workunit", workunitID, missingKey, phase, "classifier", ""); err != nil {
				r.log.Warn("recover-required-tags: tag failed",
					"workunit", workunitID, "key", missingKey, "error", err)
				continue
			}
			applied++
		default:
			// All other required lifecycle keys (work-type, etc.) are handled
			// elsewhere or cannot be inferred without activity signals.
			skipped++
		}
	}
	r.log.Info("recover-required-tags complete",
		"candidates", len(triples), "applied", applied, "skipped", skipped, "dry_run", dryRun)
	return nil
}

// classifyPhases derives one phase per closed interval that needs it and writes
// it to the wms_intervals.phase column. Returns the number of intervals for
// which a phase was written.
//
// Performance (GH #13): a batch is up to intervalBatch (500) intervals, and
// naively deriving each one's phase re-reads and re-scans the ENTIRE shared
// JSONL event log from scratch per interval (JSONLSignalReader.ReadSignals is
// a documented full linear scan) — O(intervals × log-file-size) per pass, the
// root cause of classify passes taking 20-28 CPU-minutes instead of seconds.
// batchReadSignals collapses this to a SINGLE scan per pass via
// wms.BatchSignalReader, bucketing events by (session, agent) so intervals
// that share a session benefit from each other's read instead of each paying
// for its own full scan. It also bounds that one scan to the current batch's
// own time range (computed fresh from the batch, not a persisted cursor) so a
// pass never re-indexes JSONL regions covering intervals a PRIOR pass already
// fully assembled — the log's full historical size no longer sets the
// per-pass cost. Falls back to the original per-interval derivePhase path
// when the configured SignalReader doesn't implement BatchSignalReader (e.g.
// a test fake).
func (r *Runner) classifyPhases(ctx context.Context) (int, error) {
	intervals, err := r.store.ListIntervalsNeedingPhase(ctx, intervalBatch)
	if err != nil {
		return 0, fmt.Errorf("list intervals needing phase: %w", err)
	}
	if len(intervals) == 0 {
		return 0, nil
	}

	// reEntry marks the intervals that re-enter active work after the entity had
	// already reached review/done — the rework signal. It is computed against the
	// entity's FULL closure history (queried per batch entity), NOT just the
	// intervals in this batch, so a re-entry whose predecessor review/done
	// interval was assembled in an earlier batch is still detected. A query
	// failure degrades to no-rework (logged) rather than aborting the pass.
	reEntry, err := r.detectReEntry(ctx, intervals)
	if err != nil {
		r.log.Warn("re-entry detection degraded (no cross-batch rework this pass)", "error", err)
		reEntry = map[int64]bool{}
	}

	sigsByInterval, err := r.batchReadSignals(ctx, intervals, reEntry)
	if err != nil {
		r.log.Warn("batched signal read failed, falling back to per-interval reads", "error", err)
		sigsByInterval = nil
	}

	written := 0
	for _, rec := range intervals {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		var (
			phase string
			err   error
		)
		if sigsByInterval != nil {
			phase, err = r.derivePhaseWithSignals(rec, reEntry[rec.ID], sigsByInterval[rec.ID])
		} else {
			phase, err = r.derivePhase(ctx, rec, reEntry[rec.ID])
		}
		if errors.Is(err, errNoSignal) {
			// No activity in this interval's window — leave phase NULL so it is
			// reported as "unclassified", not mislabeled. Stamp phase_assembled_at
			// so the anti-join does not re-select it every pass (idempotency).
			if err := r.store.MarkIntervalAssembled(ctx, rec.ID); err != nil {
				r.log.Warn("mark interval assembled failed", "interval", rec.ID, "error", err)
			}
			continue
		}
		if err != nil {
			r.log.Warn("phase derivation failed for interval", "interval", rec.ID, "error", err)
			continue
		}
		if err := r.store.UpdateEventRecordPhase(ctx, rec.ID, phase, "classifier"); err != nil {
			r.log.Warn("write phase failed for interval", "interval", rec.ID, "phase", phase, "error", err)
			continue
		}
		written++
	}
	return written, nil
}

// batchReadSignals builds one SessionWindow per batch interval that actually
// needs signals (rework and review-state intervals are decided by reEntry/
// rec.State alone, without touching the log — see derivePhaseWithSignals) and
// issues a SINGLE JSONL scan for the whole set via wms.BatchSignalReader.
// lowerBound/upperBound are the batch's own window extents, so the scan is
// bounded to what THIS batch needs — a runtime-determined bound recomputed
// fresh every call from the SQL-watermarked interval list
// (ListIntervalsNeedingPhase's phase_assembled_at anti-join), rather than a
// persisted byte-offset/line cursor. Returns (nil, nil) when r.signals
// doesn't implement wms.BatchSignalReader — callers must fall back to the
// per-interval path in that case.
func (r *Runner) batchReadSignals(ctx context.Context, intervals []wms.EventRecord, reEntry map[int64]bool) (map[int64]*wms.ActivitySignals, error) {
	batchReader, ok := r.signals.(wms.BatchSignalReader)
	if !ok {
		return nil, nil
	}

	var ids []int64
	var windows []wms.SessionWindow
	var lowerBound, upperBound time.Time
	for _, rec := range intervals {
		if reEntry[rec.ID] || rec.State == wms.StatusReview {
			continue // phase is already determined without signals
		}
		w := intervalWindows(rec)
		if len(w) == 0 {
			continue // no session — never queries the log (matches derivePhase)
		}
		ids = append(ids, rec.ID)
		windows = append(windows, w[0])
		if lowerBound.IsZero() || w[0].Start.Before(lowerBound) {
			lowerBound = w[0].Start
		}
		if w[0].End.After(upperBound) {
			upperBound = w[0].End
		}
	}
	if len(windows) == 0 {
		return map[int64]*wms.ActivitySignals{}, nil
	}

	results, err := batchReader.ReadSignalsBatch(ctx, windows, r.logFile, lowerBound, upperBound)
	if err != nil {
		return nil, err
	}
	if len(results) != len(ids) {
		return nil, fmt.Errorf("batch signal read: got %d results for %d windows", len(results), len(ids))
	}

	out := make(map[int64]*wms.ActivitySignals, len(ids))
	for i, id := range ids {
		out[id] = results[i]
	}
	return out, nil
}

// detectReEntry returns the set of batch interval ids that begin active work
// AFTER the same entity first finished a review/done interval — i.e. correction
// work (rework). It queries each batch entity's EARLIEST review/done END across
// its FULL history (EarliestClosureByEntity), so the predecessor closure need
// NOT be in the current batch: a re-entry active interval is detected even when
// the review/done interval that closed the first pass was assembled in an earlier
// batch and is therefore excluded by the anti-join. An active interval is rework
// iff its started_at is strictly after the entity's earliest closure end (a
// review/done that ENDED before this active STARTED).
func (r *Runner) detectReEntry(ctx context.Context, intervals []wms.EventRecord) (map[int64]bool, error) {
	// Distinct entities in the batch.
	seen := map[[2]string]bool{}
	var keys [][2]string
	for _, rec := range intervals {
		k := [2]string{rec.EntityType, rec.EntityID}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	firstClosure, err := r.store.EarliestClosureByEntity(ctx, keys)
	if err != nil {
		return nil, err
	}
	return markReEntry(intervals, firstClosure), nil
}

// markReEntry flags each active interval that started strictly after its
// entity's earliest review/done END. It is the pure comparison split out of
// detectReEntry so it is unit-testable without a store. firstClosure maps
// [entity_type, entity_id] → earliest review/done ended_at; an entity absent
// from the map has never closed, so none of its active intervals are rework.
func markReEntry(intervals []wms.EventRecord, firstClosure map[[2]string]time.Time) map[int64]bool {
	out := map[int64]bool{}
	for _, rec := range intervals {
		if rec.State != wms.StatusActive {
			continue
		}
		first, ok := firstClosure[[2]string{rec.EntityType, rec.EntityID}]
		if ok && rec.StartedAt.After(first) {
			out[rec.ID] = true
		}
	}
	return out
}

// derivePhase maps one closed interval to exactly one phase from the seeded
// vocabulary {design,build,test,review,rework}. Priority (first match wins):
//
//  1. rework  — the interval re-enters active work after review/done (the
//     entity was sent back; correction cost, the most important to isolate).
//  2. review  — the interval itself occupies the review state (review work is
//     defined by lifecycle position, not tool mix).
//  3. test    — test-matching bash commands dominate (>60% of bash commands),
//     mirroring the work-type=test rule threshold.
//  4. build   — EDIT/WRITE tool tags dominate over READ/GREP (producing change).
//  5. design  — READ/GREP dominate (>50% of tool tags), mirroring the
//     work-type=research threshold (investigation/shaping, no code yet).
//  6. build   — there is activity but no rule fired (default: most work is
//     producing change). An interval with NO activity signals at all stays
//     NULL (returns the no-signal sentinel), so it is reported as
//     "unclassified" rather than mislabeled — UNLESS the interval has no
//     readable signal window at all (no session/agent) yet demonstrably had
//     activity (positive duration); see below.
//
// The interval's own state column drives review (and feeds rework); tool/bash
// ratios come from the same JSONL signals the work-type rules read, scoped to a
// SessionWindow built from THIS interval.
//
// Identity gap: status-transition intervals (written by TransitionEventRecord
// from the wms-mcp path) carry NO session_id/agent_name — Claude Code does not
// put them in the MCP _meta, so p.Meta is empty there (the MCP layer is a no-op
// for identity; only hooks see agent_type). intervalWindows therefore returns an
// empty window for such an interval, ReadSignals returns TotalEvents==0, and
// before this guard a costed, hour-long closed interval was wrongly left NULL.
// NULL is reserved for "no activity in the window"; a closed interval with a
// positive duration demonstrably HAD activity, the signals just can't be scoped
// to a session window. Such an interval takes the rule-6 build default.
func (r *Runner) derivePhase(ctx context.Context, rec wms.EventRecord, reEntry bool) (string, error) {
	if reEntry {
		return "rework", nil
	}
	if rec.State == wms.StatusReview {
		return "review", nil
	}

	windows := intervalWindows(rec)
	sigs, err := r.signals.ReadSignals(ctx, windows, r.logFile)
	if err != nil {
		return "", err
	}
	return classifyIntervalPhase(rec, len(windows) == 0, sigs)
}

// derivePhaseWithSignals is derivePhase's counterpart for the batched signal
// path (batchReadSignals): sigs was already read for this interval in the
// pass's single JSONL scan, so no further I/O happens here. sigs is nil for
// an interval batchReadSignals never queried the log for at all (matching
// derivePhase's own "no window → no ReadSignals call" behavior).
func (r *Runner) derivePhaseWithSignals(rec wms.EventRecord, reEntry bool, sigs *wms.ActivitySignals) (string, error) {
	if reEntry {
		return "rework", nil
	}
	if rec.State == wms.StatusReview {
		return "review", nil
	}
	windows := intervalWindows(rec)
	return classifyIntervalPhase(rec, len(windows) == 0, sigs)
}

// classifyIntervalPhase is the pure rule engine shared by derivePhase and
// derivePhaseWithSignals: given an interval, whether it had no session window
// at all, and its (possibly nil) signals, pick exactly one phase. See
// derivePhase's original doc comment for the full priority-order rationale
// (rework > review > test > build > design > build-default).
func classifyIntervalPhase(rec wms.EventRecord, noWindow bool, sigs *wms.ActivitySignals) (string, error) {
	if sigs == nil || sigs.TotalEvents == 0 {
		// An interval with no readable signal window (no session/agent — the
		// status-transition case) but a positive duration had real lifecycle
		// activity we simply cannot scope to signals. Take the rule-6 build
		// default rather than mislabel it unclassified. Only a genuinely empty
		// interval (zero/unknown duration) stays NULL.
		if noWindow && intervalHasActivity(rec) {
			return "build", nil
		}
		// No activity in this interval's window: leave phase NULL (unclassified)
		// rather than invent one. The store write is skipped by returning the
		// sentinel error so the collector reports it under "unclassified".
		return "", errNoSignal
	}

	// test: test-command-dominant.
	if len(sigs.BashCommands) > 0 {
		testCount := 0
		for _, cmd := range sigs.BashCommands {
			if strings.Contains(cmd, "test") {
				testCount++
			}
		}
		if float64(testCount)/float64(len(sigs.BashCommands)) > 0.60 {
			return "test", nil
		}
	}

	totalTags := 0
	for _, n := range sigs.ToolTagCounts {
		totalTags += n
	}
	if totalTags > 0 {
		editWrite := sigs.ToolTagCounts["EDIT"] + sigs.ToolTagCounts["WRITE"]
		readGrep := sigs.ToolTagCounts["READ"] + sigs.ToolTagCounts["GREP"]
		// build: change-producing tools outweigh reading.
		if editWrite > 0 && editWrite >= readGrep {
			return "build", nil
		}
		// design: reading/searching dominates — investigation/shaping.
		if float64(readGrep)/float64(totalTags) > 0.50 {
			return "design", nil
		}
	}

	// Activity present but no rule fired: default to build (most agent work is
	// producing change).
	return "build", nil
}

// errNoSignal is the sentinel derivePhase returns for an interval that has no
// activity at all; the caller leaves its phase NULL.
var errNoSignal = fmt.Errorf("no activity signal for interval")

// intervalHasActivity reports whether a closed interval represents real
// lifecycle activity even when no signal window can be built for it (no
// session/agent). The proxy is a positive duration: a closed interval that
// spanned real time was an entity actively in that state, whereas an
// instantaneous transition (zero/unknown duration) carries no work. On live
// data this matches cost exactly on the phase-eligible set: every CLOSED costed
// interval is durated (the only costed-but-undurated rows are still OPEN, so
// they are never in phase scope). Prefers the stored duration_ms; falls back to
// the started_at→ended_at span.
func intervalHasActivity(rec wms.EventRecord) bool {
	if rec.DurationMs != nil {
		return *rec.DurationMs > 0
	}
	if rec.EndedAt != nil {
		return rec.EndedAt.After(rec.StartedAt)
	}
	return false
}

// intervalWindows builds the single SessionWindow that scopes signal reading to
// one interval: its session prefix, agent, and time span. Returns an empty
// slice when the interval lacks a session (no signals can be attributed),
// which ReadSignals treats as "no events". AgentName may be "" for the lead —
// that's the canonical value, and signals.go matches it exactly against the
// lead's "" JSONL rows (same fix as the inline classifier, classifier.go:119).
func intervalWindows(rec wms.EventRecord) []wms.SessionWindow {
	if rec.SessionID == "" || rec.EndedAt == nil {
		return nil
	}
	prefix := rec.SessionID
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	agentName := rec.AgentName
	if agentName != "" && agentName[0] != '@' {
		agentName = "@" + agentName
	}
	return []wms.SessionWindow{{
		SessionPrefix: prefix,
		AgentName:     agentName,
		Start:         rec.StartedAt,
		End:           *rec.EndedAt,
	}}
}
