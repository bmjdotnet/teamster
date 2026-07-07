package rollup

import (
	"context"
	"fmt"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/transcript"
)

// recoveryMethod is the attribution method label for cost re-attributed from the
// agent's intended-focus timeline (the wms_setFocus calls read from the .claude
// transcript). It is distinct from the live join methods so recovered cost is
// filterable and reversible, and never confused with temporal_join.
const recoveryMethod = "transcript_focus_recovery"

// FocusTimelineSource resolves a session's per-thread intended-focus timeline,
// reading from projectsDir (this pass's .claude/projects root; recovery only
// reaches sessions local to this host+user, so projectsDir is always opts.
// ProjectsDir). The production implementation reads the real transcripts via
// internal/transcript; tests inject a stub so the DB-side re-attribution logic is
// exercised without filesystem fixtures (the stub ignores projectsDir). The
// returned timeline's FocusAt yields the most-recent setFocus at-or-before a ts on
// a given agent thread (lead = "").
type FocusTimelineSource func(sessionID, projectsDir string) (*transcript.FocusTimeline, error)

// defaultTranscriptSource is the production FocusTimelineSource: it reuses
// internal/transcript so the recovery pass parses exactly the bytes — and builds
// exactly the dedup key (message.id|requestId) — that the token-scraper wrote into
// token_ledger.message_id, the load-bearing join key.
func defaultTranscriptSource(sessionID, projectsDir string) (*transcript.FocusTimeline, error) {
	return transcript.SetFocusTimeline(projectsDir, sessionID)
}

// localToUser reports whether a session stamped username belongs to THIS pass's
// user for transcript-reading purposes. A genuinely different non-empty user is
// NOT local (their ~/.claude home is a different home this pass does not read —
// it is deferred). The empty username ("") IS local: those rows predate the
// username stamping, and on a single-user host they belong to the operator's own
// home — so reading the default home is correct, and a stricter rule would defer
// the entire historical backlog the operator explicitly wants recovered (LENIENT).
func localToUser(username, currentUser string) bool {
	return username == "" || username == currentUser
}

// RecoverOptions configures a recovery pass.
type RecoverOptions struct {
	// ProjectsDir is the .claude projects root holding session transcripts; ""
	// uses $HOME/.claude/projects. Ignored when Source is set (tests).
	ProjectsDir string
	// Host identifies THIS host; the hard scope filter for transcript reads.
	Host string
	// User is THIS pass's OS user; see localToUser for the lenient match rule.
	User string
	// DryRun performs ZERO writes — it logs the plan and counts only.
	DryRun bool
	// Source overrides the transcript reader (tests inject a stub).
	Source FocusTimelineSource
}

// scoped reports whether host-scoping is active. Disabled only when Host is empty
// (the stub-Source test path); in production the driver always supplies a host.
func (o RecoverOptions) scoped() bool { return o.Host != "" }

// RecoverStats summarizes one recovery pass.
type RecoverStats struct {
	Sessions         int // local sessions whose transcript was examined
	Examined         int // unallocated messages considered (local sessions only)
	Recovered        int // re-attributed to a real entity (own-thread setFocus)
	RecoveredLead    int // re-attributed via teammate→lead chaining (subset of Recovered)
	Unrecoverable    int // left unallocated (predates the thread's first setFocus)
	Deferred         int // sessions not local to this host+user (transcript not here)
	DeferredMessages int // unallocated messages in deferred sessions
}

// RecoverFocus re-attributes method='unallocated' cost using the agent's intended
// -focus timeline read from the .claude transcripts. For each session still
// holding unallocated rows it builds the per-thread setFocus timeline and, for
// each unallocated message, attributes it to the most-recent setFocus at-or-
// before the message ts on the same thread (lead ""=lead thread). A teammate
// with no covering setFocus on its own thread falls back to the lead's intended
// focus at that instant. A message that predates the thread's first setFocus is
// left unallocated (the warmup floor).
//
// CONSERVATION: one usage_attribution row per message; ApplyRecovery's UPDATE is
// scoped to method='unallocated', never touching an already-attributed row.
//
// HOST-LOCAL: the .claude transcripts exist only on the host+user that ran the
// session, so recovery is scoped (see RecoverOptions.Host/User, localToUser).
// A deferred session is recovered by a pass run as that host+user.
func (r *Runner) RecoverFocus(ctx context.Context, opts RecoverOptions) (RecoverStats, error) {
	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	sessions, err := r.rec.UnallocatedSessions(ctx, store.UnallocatedFilter{})
	if err != nil {
		return RecoverStats{}, fmt.Errorf("list unallocated sessions: %w", err)
	}

	var stats RecoverStats
	// deferredByHost accumulates the non-local residual so it is reported, not
	// swallowed: which other host holds the transcripts this box can't reach.
	deferredByHost := map[string]int{}

	for _, s := range sessions {
		if opts.scoped() && (s.Host != opts.Host || !localToUser(s.Username, opts.User)) {
			stats.Deferred++
			stats.DeferredMessages += int(s.MessageCount)
			deferredByHost[s.Host+"\x00"+s.Username] += int(s.MessageCount)
			continue
		}

		stats.Sessions++
		tl, err := src(s.SessionID, opts.ProjectsDir)
		if err != nil {
			// A bad transcript for one session must not starve the rest: log and
			// continue. The session's rows simply stay unallocated this pass.
			r.log.Warn("recover: timeline build failed; leaving session unallocated",
				"session_id", s.SessionID, "error", err)
			continue
		}

		sid := s.SessionID
		msgs, err := r.rec.ReclaimableMessages(ctx, sid, "", false, []string{"unallocated"})
		if err != nil {
			return stats, fmt.Errorf("session %s: list unallocated messages: %w", sid, err)
		}

		for _, m := range msgs {
			stats.Examined++

			ev, ok := tl.FocusAt(m.AgentName, m.Timestamp)
			viaLead := false
			// Teammate→lead chaining: a teammate with no covering setFocus on its
			// own thread inherits the lead's intended focus at that instant. The
			// lead thread itself (agentName "") is not chained.
			if !ok && m.AgentName != "" {
				ev, ok = tl.FocusAt("", m.Timestamp)
				viaLead = ok
			}
			if !ok {
				// Predates the thread's first setFocus → warmup floor; leave it.
				stats.Unrecoverable++
				continue
			}
			// A half-specified setFocus (type or id missing) is not a usable
			// attribution target.
			if ev.EntityType == "" || ev.EntityID == "" {
				stats.Unrecoverable++
				continue
			}

			// Cross-check wms_intervals for a more specific concurrent focus
			// interval — mirrors the live Allocate path's precedence (workunit over
			// its parent outcome when both have concurrent open focus).
			crossCheckAgent := m.AgentName
			if viaLead {
				crossCheckAgent = ""
			}
			if wmsRef, wmsOK, wmsErr := r.alloc.FocusEntityAt(ctx, sid, crossCheckAgent, m.Timestamp); wmsErr != nil {
				r.log.Warn("recover: wms_intervals cross-check failed; using transcript entity",
					"message_id", m.MessageID, "session_id", sid, "error", wmsErr)
			} else if wmsOK && entitySpecificity[wmsRef.EntityType] > entitySpecificity[ev.EntityType] {
				r.log.Info("recover: wms_intervals override — more specific entity",
					"message_id", m.MessageID, "session_id", sid,
					"transcript_entity", ev.EntityType+"/"+ev.EntityID,
					"wms_entity", wmsRef.EntityType+"/"+wmsRef.EntityID)
				ev.EntityType = wmsRef.EntityType
				ev.EntityID = wmsRef.EntityID
			}

			stats.Recovered++
			if viaLead {
				stats.RecoveredLead++
			}

			if opts.DryRun {
				r.log.Info("recover (dry-run): would re-attribute",
					"message_id", m.MessageID, "session_id", sid, "agent_name", m.AgentName,
					"to_entity_type", ev.EntityType, "to_entity_id", ev.EntityID,
					"matched_setfocus_at", ev.Timestamp, "via_lead", viaLead)
				continue
			}

			if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
				Strategy:   "focus",
				Method:     recoveryMethod,
				MessageIDs: []string{m.MessageID},
				Entity:     store.EntityRef{EntityType: ev.EntityType, EntityID: ev.EntityID},
				Evidence:   map[string]any{"setfocus_at": ev.Timestamp},
			}); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply recovery: %w", sid, m.MessageID, err)
			}
		}
	}

	if stats.Deferred > 0 {
		for hu, n := range deferredByHost {
			host, user, _ := strings.Cut(hu, "\x00")
			r.log.Warn("recover: session not local to this host+user — deferred to a host-local or fetch-based pass",
				"transcript_host", host, "transcript_user", user, "messages", n,
				"this_host", opts.Host, "this_user", opts.User)
		}
	}

	if !opts.DryRun && stats.Recovered > 0 {
		if err := r.alloc.BuildCostRollup(ctx); err != nil {
			return stats, fmt.Errorf("recover: rebuild cost_rollup: %w", err)
		}
		if _, err := r.alloc.AssembleIntervalCost(ctx); err != nil {
			return stats, fmt.Errorf("recover: reassemble interval cost: %w", err)
		}
	}

	r.log.Info("recover-focus pass complete",
		"sessions", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "via_lead", stats.RecoveredLead,
		"unrecoverable", stats.Unrecoverable,
		"deferred_sessions", stats.Deferred, "deferred_messages", stats.DeferredMessages,
		"dry_run", opts.DryRun)
	return stats, nil
}

// Unrecover reverses a recovery pass: deletes every
// method='transcript_focus_recovery' attribution and its evidence, returning
// those messages to the unallocated bucket.
func (r *Runner) Unrecover(ctx context.Context) (int, error) {
	n, err := r.rec.UncoverRecovery(ctx, "focus")
	return int(n), err
}

// warmupMethod is the attribution method label for warmup cost re-attributed from
// the pre-first-setFocus interval to the session's resolved outcome.
const warmupMethod = "admin_warmup"

// WarmupStats summarizes one warmup recovery pass.
type WarmupStats struct {
	Sessions         int
	Examined         int
	Recovered        int
	NoOutcome        int
	Deferred         int
	DeferredMessages int
}

// RecoverWarmup re-attributes method='unallocated' warmup cost — messages that
// predate the session thread's first wms_setFocus — to the session's resolved
// outcome under a new phase=admin.
//
//  1. Build the intended-focus timeline (local: transcript; remote: DB focus
//     intervals the token-scraper already shipped, via SessionFocusIntervals).
//  2. Identify warmup messages: those whose timestamp precedes the thread's
//     first setFocus. A thread with NO setFocus at all is not warmup (that's
//     Objective 2 — no-focus sessions).
//  3. Resolve the session's outcome: the parent Outcome of the first-focused
//     entity across ALL threads.
//  4. ApplyRecovery: entity→resolved outcome, method='admin_warmup', plus a
//     synthetic kind='state' interval [session start, first focus) phase='admin'
//     via EnsureAdminInterval so intervalAt naturally covers it.
//
// REVERSIBLE: UncoverRecovery("warmup") deletes by method + removes synthetic
// admin intervals + evidence.
func (r *Runner) RecoverWarmup(ctx context.Context, opts RecoverOptions) (WarmupStats, error) {
	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	sessions, err := r.rec.UnallocatedSessions(ctx, store.UnallocatedFilter{})
	if err != nil {
		return WarmupStats{}, fmt.Errorf("list unallocated sessions: %w", err)
	}

	var stats WarmupStats
	deferredByHost := map[string]int{}

	for _, s := range sessions {
		isRemote := opts.scoped() && (s.Host != opts.Host || !localToUser(s.Username, opts.User))

		var tl *transcript.FocusTimeline
		if isRemote {
			// Remote session: the transcript lives on another host. Try the DB
			// focus intervals the remote token-scraper already shipped to the hub
			// (identity_source='remote_scraper'); if none exist, defer.
			events, err := r.rec.SessionFocusIntervals(ctx, s.SessionID)
			if err != nil {
				r.log.Warn("recover-warmup: DB timeline build failed for remote session; deferring",
					"session_id", s.SessionID, "host", s.Host, "error", err)
				stats.Deferred++
				stats.DeferredMessages += int(s.MessageCount)
				deferredByHost[s.Host+"\x00"+s.Username] += int(s.MessageCount)
				continue
			}
			if len(events) == 0 {
				stats.Deferred++
				stats.DeferredMessages += int(s.MessageCount)
				deferredByHost[s.Host+"\x00"+s.Username] += int(s.MessageCount)
				continue
			}
			tl = timelineFromFocusEvents(s.SessionID, events)
		} else {
			var err error
			tl, err = src(s.SessionID, opts.ProjectsDir)
			if err != nil {
				r.log.Warn("recover-warmup: timeline build failed; skipping session",
					"session_id", s.SessionID, "error", err)
				continue
			}
		}

		stats.Sessions++

		if len(tl.Events) == 0 {
			continue
		}

		// Find the session-global first setFocus across ALL threads.
		var sessionFirstFocus *transcript.FocusEvent
		for _, evs := range tl.Events {
			if len(evs) > 0 {
				if sessionFirstFocus == nil || evs[0].Timestamp.Before(sessionFirstFocus.Timestamp) {
					cp := evs[0]
					sessionFirstFocus = &cp
				}
			}
		}
		if sessionFirstFocus == nil {
			continue
		}

		outcomeType, outcomeID, err := r.resolveOutcome(ctx, sessionFirstFocus.EntityType, sessionFirstFocus.EntityID)
		if err != nil {
			r.log.Warn("recover-warmup: outcome resolution failed; skipping session",
				"session_id", s.SessionID, "error", err)
			continue
		}
		if outcomeType == "" || outcomeID == "" {
			continue
		}

		msgs, err := r.rec.ReclaimableMessages(ctx, s.SessionID, "", false, []string{"unallocated"})
		if err != nil {
			return stats, fmt.Errorf("session %s: list unallocated messages: %w", s.SessionID, err)
		}

		// Partition: warmup = messages that predate their thread's first setFocus.
		var warmupMsgs []store.LedgerMessage
		for _, m := range msgs {
			threadEvs := tl.Events[m.AgentName]
			if len(threadEvs) == 0 {
				if m.AgentName != "" {
					leadEvs := tl.Events[""]
					if len(leadEvs) > 0 && m.Timestamp.Before(leadEvs[0].Timestamp) {
						warmupMsgs = append(warmupMsgs, m)
					}
				}
				continue
			}
			if m.Timestamp.Before(threadEvs[0].Timestamp) {
				warmupMsgs = append(warmupMsgs, m)
			}
		}
		if len(warmupMsgs) == 0 {
			continue
		}

		warmupStart := warmupMsgs[0].Timestamp
		for _, m := range warmupMsgs[1:] {
			if m.Timestamp.Before(warmupStart) {
				warmupStart = m.Timestamp
			}
		}

		if opts.DryRun {
			stats.Examined += len(warmupMsgs)
			stats.Recovered += len(warmupMsgs)
			r.log.Info("recover-warmup (dry-run): would re-attribute",
				"session_id", s.SessionID, "count", len(warmupMsgs),
				"to_entity_type", outcomeType, "to_entity_id", outcomeID,
				"warmup_start", warmupStart, "first_focus_at", sessionFirstFocus.Timestamp)
			continue
		}

		adminIntervalID, err := r.rec.EnsureAdminInterval(ctx, s.SessionID,
			store.EntityRef{EntityType: outcomeType, EntityID: outcomeID},
			warmupStart, sessionFirstFocus.Timestamp)
		if err != nil {
			return stats, fmt.Errorf("session %s: ensure admin interval: %w", s.SessionID, err)
		}

		msgIDs := make([]string, len(warmupMsgs))
		for i, m := range warmupMsgs {
			msgIDs[i] = m.MessageID
		}
		stats.Examined += len(msgIDs)
		if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
			Strategy:   "warmup",
			Method:     warmupMethod,
			MessageIDs: msgIDs,
			Entity:     store.EntityRef{EntityType: outcomeType, EntityID: outcomeID},
			IntervalID: &adminIntervalID,
			Evidence: map[string]any{
				"warmup_start":   warmupStart,
				"first_focus_at": sessionFirstFocus.Timestamp,
			},
		}); err != nil {
			return stats, fmt.Errorf("session %s: apply warmup: %w", s.SessionID, err)
		}
		stats.Recovered += len(msgIDs)
	}

	if stats.Deferred > 0 {
		for hu, n := range deferredByHost {
			host, user, _ := strings.Cut(hu, "\x00")
			r.log.Warn("recover-warmup: session not local — deferred",
				"transcript_host", host, "transcript_user", user, "messages", n,
				"this_host", opts.Host, "this_user", opts.User)
		}
	}

	if !opts.DryRun && stats.Recovered > 0 {
		if err := r.alloc.BuildCostRollup(ctx); err != nil {
			return stats, fmt.Errorf("recover-warmup: rebuild cost_rollup: %w", err)
		}
		if _, err := r.alloc.AssembleIntervalCost(ctx); err != nil {
			return stats, fmt.Errorf("recover-warmup: reassemble interval cost: %w", err)
		}
	}

	r.log.Info("recover-warmup pass complete",
		"sessions", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "no_outcome", stats.NoOutcome,
		"deferred_sessions", stats.Deferred, "deferred_messages", stats.DeferredMessages,
		"dry_run", opts.DryRun)
	return stats, nil
}

// UncoverWarmup reverses a warmup recovery pass: deletes every
// method='admin_warmup' attribution and its evidence, and removes the
// synthetic admin state-intervals EnsureAdminInterval created (F3 — handled by
// RecoveryStore.UncoverRecovery("warmup") itself, not a separate step here).
func (r *Runner) UncoverWarmup(ctx context.Context) (int, error) {
	n, err := r.rec.UncoverRecovery(ctx, "warmup")
	return int(n), err
}

// timelineFromFocusEvents builds a *transcript.FocusTimeline from raw
// RecoveryStore.SessionFocusIntervals rows — the fallback timeline source for
// remote sessions whose transcript does not exist on the local host. Events
// are grouped by agent_name (lead = "") and sorted ascending by timestamp,
// matching transcript.SetFocusTimeline's guarantee.
func timelineFromFocusEvents(sessionID string, events []store.FocusEvent) *transcript.FocusTimeline {
	tl := &transcript.FocusTimeline{
		SessionID: sessionID,
		Events:    make(map[string][]transcript.FocusEvent),
	}
	for _, e := range events {
		tl.Events[e.AgentName] = append(tl.Events[e.AgentName], transcript.FocusEvent{
			Timestamp:  e.StartedAt,
			EntityType: e.Entity.EntityType,
			EntityID:   e.Entity.EntityID,
			AgentName:  e.AgentName,
		})
	}
	for agent, evs := range tl.Events {
		for i := 1; i < len(evs); i++ {
			for j := i; j > 0 && evs[j].Timestamp.Before(evs[j-1].Timestamp); j-- {
				evs[j], evs[j-1] = evs[j-1], evs[j]
			}
		}
		tl.Events[agent] = evs
	}
	return tl
}

// resolveOutcome finds the parent Outcome for a given entity. If the entity is
// already an outcome, it is returned directly (after existence-checking via
// GetOutcome). For a workunit, we look up its parent outcome in the WMS
// hierarchy via GetWorkUnit/GetOutcome.
func (r *Runner) resolveOutcome(ctx context.Context, entityType, entityID string) (string, string, error) {
	if entityType == "outcome" {
		if _, err := r.reader.GetOutcome(ctx, entityID); err != nil {
			if store.IsNotFound(err) {
				r.log.Warn("resolveOutcome: outcome row not found", "entity_id", entityID)
				return "", "", nil
			}
			return "", "", fmt.Errorf("get outcome: %w", err)
		}
		return entityType, entityID, nil
	}
	if entityType == "workunit" {
		wu, err := r.reader.GetWorkUnit(ctx, entityID)
		if err != nil {
			if store.IsNotFound(err) {
				return "", "", nil
			}
			return "", "", fmt.Errorf("get workunit: %w", err)
		}
		if wu.OutcomeID == "" {
			return "", "", nil
		}
		if _, err := r.reader.GetOutcome(ctx, wu.OutcomeID); err != nil {
			if store.IsNotFound(err) {
				r.log.Warn("resolveOutcome: parent outcome row not found",
					"workunit_id", entityID, "outcome_id", wu.OutcomeID)
				return "", "", nil
			}
			return "", "", fmt.Errorf("get parent outcome: %w", err)
		}
		return "outcome", wu.OutcomeID, nil
	}
	return "", "", nil
}
