package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/transcript"
)

// recoveryMethod is the attribution method label for cost re-attributed from the
// agent's intended-focus timeline (the wms_setFocus calls read from the .claude
// transcript). It is distinct from the live join methods so recovered cost is
// filterable and reversible, and never confused with temporal_join. See spec §5.4
// and semantic-conventions.md §7.2. The method column was widened to VARCHAR(48)
// in migration v33; this label is 25 chars and fits comfortably.
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
// token_ledger.message_id, the load-bearing join key (spec §2.4).
func defaultTranscriptSource(sessionID, projectsDir string) (*transcript.FocusTimeline, error) {
	return transcript.SetFocusTimeline(projectsDir, sessionID)
}

// localToUser reports whether a session stamped username belongs to THIS pass's
// user for transcript-reading purposes. A genuinely different non-empty user is
// NOT local (their ~/.claude home is a different home this pass does not read —
// it is deferred). The empty username ("") IS local: those rows predate the
// username stamping (the live backlog is 100% username='' on schema_version 30),
// and on a single-user host they belong to the operator's own home — so reading
// the default home is correct, and a stricter rule would defer the entire
// historical backlog the operator explicitly wants recovered (LENIENT filter).
func localToUser(username, currentUser string) bool {
	return username == "" || username == currentUser
}

// RecoverOptions configures a recovery pass.
type RecoverOptions struct {
	// ProjectsDir is the .claude projects root holding session transcripts; ""
	// uses $HOME/.claude/projects (the scraper's default). Ignored when Source is
	// set (tests).
	ProjectsDir string
	// Host identifies THIS host. It is the hard scope filter: only sessions whose
	// token_ledger.host == Host have their transcript on this box. A session on a
	// different host is DEFERRED (its transcript lives elsewhere) — never read off
	// a wrong local file, never mis-counted as warmup. The driver passes cfg.Host
	// (TEAMSTER_HOST else os.Hostname).
	Host string
	// User is THIS pass's OS user. On this host a session is local — and its
	// transcript read from ProjectsDir ($HOME/.claude/projects) — when its
	// token_ledger.username equals User OR is "" (unstamped/pre-v34 rows, which on
	// a single-user host are the operator's own; the live backlog is entirely
	// username=''). A session by a genuinely DIFFERENT non-empty user on this host
	// is DEFERRED (its transcript is in a home this pass does not read). The driver
	// passes cfg.User (os/user.Current, override TEAMSTER_USER). When Host is empty
	// (the stub-Source test path) scoping is disabled entirely.
	User string
	// DryRun performs ZERO writes — it logs the plan and counts only. This is what
	// the live-DB validation runs before any real write (spec §6.6, §7.3).
	DryRun bool
	// Source overrides the transcript reader (tests inject a stub). nil uses the
	// real transcript reader; the stub receives the resolved projectsDir but
	// ignores it.
	Source FocusTimelineSource
}

// scoped reports whether host-scoping is active. Disabled only when Host is empty
// (the stub-Source test path); in production the driver always supplies a host.
func (o RecoverOptions) scoped() bool { return o.Host != "" }

// RecoverStats summarizes one recovery pass.
type RecoverStats struct {
	Sessions           int // local sessions whose transcript was examined
	Examined           int // unallocated messages considered (local sessions only)
	Recovered          int // re-attributed to a real entity (own-thread setFocus)
	RecoveredLead      int // re-attributed via teammate→lead chaining (subset of Recovered)
	Unrecoverable    int // left unallocated (predates the thread's first setFocus)
	Deferred         int // sessions not local to this host+user (transcript not here)
	DeferredMessages int // unallocated messages in deferred sessions
}

// RecoverFocus re-attributes method='unallocated' cost using the agent's intended
// -focus timeline read from the .claude transcripts (spec §5). For each session
// still holding unallocated rows it builds the per-thread setFocus timeline and,
// for each unallocated message, attributes it to the most-recent setFocus
// at-or-before the message ts on the same thread (lead ""=lead thread). A teammate
// with no covering setFocus on its own thread falls back to the lead's intended
// focus at that instant (mirrors the allocator's lead-fallback). A message that
// predates the thread's first setFocus is left unallocated (the warmup floor).
//
// CONSERVATION (spec §5.3): there is exactly one usage_attribution row per message
// (PK message_id,entity_type,entity_id; weight 1.0). Recovery REPLACES the
// unallocated row in place via an UPDATE scoped to method='unallocated' — it never
// inserts a second row, so per-message weight stays 1.0 and SUM(cost_facts.cost_usd)
// is unchanged. (An INSERT...ON DUPLICATE KEY UPDATE would NOT work here: the PK
// includes entity_type/entity_id, which recovery changes, so an upsert would add a
// second row rather than replace. The in-place UPDATE of the PK columns is correct
// because a message has exactly one row and the new entity never collides.)
//
// REVERSIBLE (spec §5.4): every write is scoped to rows currently
// method='unallocated'; a non-unallocated row is never touched. Unrecover reverses
// the whole pass.
//
// AUDITABLE (spec §5.4): for each recovered row a recovery_evidence row records the
// matched setFocus (ts + entity), so "why is this $X on entity Y" is answerable.
//
// HOST-LOCAL (operator ruling): the .claude transcripts exist only on the host+
// user that ran the session, so recovery is scoped. A session is LOCAL — its
// transcript read from opts.ProjectsDir ($HOME default) — only when its host is
// opts.Host AND its user is opts.User or "" (the unstamped/pre-v34 case, which on
// a single-user host is the operator's own; the live backlog is 100% username='',
// so a STRICT username match would defer the entire historical recovery — hence
// LENIENT on ""). Any other session (different host, or a genuinely different
// non-empty user whose home this pass does not read) is DEFERRED: counted +
// logged, never read off a wrong file, never mis-classed as warmup. A deferred
// session is recovered by a pass run as that host+user (or a future fetch-based
// pass). All residuals are surfaced (no-silent-failures), not swallowed.
//
// DryRun performs ZERO writes (logs the plan + counts only). It is idempotent: a
// re-run only touches sessions that still have unallocated rows, and re-resolves
// each to the same entity, so steady-state runs are cheap residual mop-up.
func (r *Runner) RecoverFocus(ctx context.Context, opts RecoverOptions) (RecoverStats, error) {
	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	sessions, err := r.unallocatedSessions(ctx)
	if err != nil {
		return RecoverStats{}, fmt.Errorf("list unallocated sessions: %w", err)
	}

	now := time.Now().UTC()
	var stats RecoverStats
	// deferredByHost accumulates the non-local residual so it is reported, not
	// swallowed: which other host holds the transcripts this box can't reach (the
	// no-silent-failures rule). The fix is to run recovery on that host, or to
	// fetch its transcripts here later (future remote-scraper work).
	deferredByHost := map[string]int{}

	for _, s := range sessions {
		// SCOPE: a session is local — its transcript readable from ProjectsDir on
		// this box — only when its host is THIS host AND its user is THIS user (or
		// "", the unstamped/pre-v34 case, which is local on a single-user host;
		// the live backlog is 100% username='' so STRICT username matching would
		// defer the entire historical recovery the operator wants — LENIENT). Any
		// other session (different host, or a genuinely different non-empty user
		// whose ~/.claude home this pass does not read) is DEFERRED: counted +
		// logged, never read off a wrong file, never mis-classed as warmup. The
		// fix for a deferred session is a recovery pass run as that host+user (or
		// a future fetch-based pass). Disabled when Host=="" (the stub-Source tests).
		if opts.scoped() && (s.host != opts.Host || !localToUser(s.username, opts.User)) {
			stats.Deferred++
			stats.DeferredMessages += s.msgCount
			deferredByHost[s.host+"\x00"+s.username] += s.msgCount
			continue
		}

		stats.Sessions++
		tl, err := src(s.sessionID, opts.ProjectsDir)
		if err != nil {
			// A bad transcript for one session must not starve the rest: log and
			// continue. The session's rows simply stay unallocated this pass.
			r.log.Warn("recover: timeline build failed; leaving session unallocated",
				"session_id", s.sessionID, "error", err)
			continue
		}

		sid := s.sessionID
		msgs, err := r.unallocatedMessages(ctx, s)
		if err != nil {
			return stats, fmt.Errorf("session %s: list unallocated messages: %w", sid, err)
		}

		for _, m := range msgs {
			stats.Examined++

			ev, ok := tl.FocusAt(m.agentName, m.ts)
			viaLead := false
			// Teammate→lead chaining: a teammate with no covering setFocus on its
			// own thread inherits the lead's intended focus at that instant (spec
			// §5.2 step 5). The lead thread itself has agentName "" and is not
			// chained (FocusAt("",ts) is its own resolution).
			if !ok && m.agentName != "" {
				ev, ok = tl.FocusAt("", m.ts)
				viaLead = ok
			}
			if !ok {
				// Predates the thread's first setFocus → warmup floor; leave it.
				stats.Unrecoverable++
				continue
			}
			// A half-specified setFocus (type or id missing) is not a usable
			// attribution target — writing (type,'') would create a malformed
			// entity. The transcript reader already drops fully-empty events, but
			// guard the partial case here so recovery never invents an entity.
			if ev.EntityType == "" || ev.EntityID == "" {
				stats.Unrecoverable++
				continue
			}

			stats.Recovered++
			if viaLead {
				stats.RecoveredLead++
			}

			if opts.DryRun {
				r.log.Info("recover (dry-run): would re-attribute",
					"message_id", m.messageID, "session_id", sid, "agent_name", m.agentName,
					"to_entity_type", ev.EntityType, "to_entity_id", ev.EntityID,
					"matched_setfocus_at", ev.Timestamp, "via_lead", viaLead)
				continue
			}

			if err := r.applyRecovery(ctx, m, ev, now); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply recovery: %w", sid, m.messageID, err)
			}
		}
	}

	// Report the deferred residual explicitly (no-silent-failures): these sessions
	// are not local to this host+user, so this box can't read their transcripts.
	// They are NOT irreducible — a recovery pass run as that host+user (or a future
	// fetch-based pass) recovers them. One line per (host,user) so the operator
	// sees exactly where the deferred cost lives.
	if stats.Deferred > 0 {
		for hu, n := range deferredByHost {
			host, user, _ := strings.Cut(hu, "\x00")
			r.log.Warn("recover: session not local to this host+user — deferred to a host-local or fetch-based pass",
				"transcript_host", host, "transcript_user", user, "messages", n,
				"this_host", opts.Host, "this_user", opts.User)
		}
	}

	// Recovery rewrote usage_attribution, so the derived aggregates (cost_rollup
	// table and per-interval cost) are now stale — rebuild them exactly as Run
	// does after Allocate, so the recovered cost is reflected in the entity/day
	// and cost-by-phase views, not just the live cost_facts VIEW. Skipped on a
	// dry-run (nothing changed) and when nothing was recovered (no-op pass).
	if !opts.DryRun && stats.Recovered > 0 {
		rows, err := r.BuildCostRollup(ctx)
		if err != nil {
			return stats, fmt.Errorf("recover: rebuild cost_rollup: %w", err)
		}
		intervals, err := r.AssembleIntervalCost(ctx)
		if err != nil {
			return stats, fmt.Errorf("recover: reassemble interval cost: %w", err)
		}
		r.log.Info("recover-focus rebuilt aggregates", "rollup_rows", rows, "intervals_costed", intervals)
	}

	r.log.Info("recover-focus pass complete",
		"sessions", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "via_lead", stats.RecoveredLead,
		"unrecoverable", stats.Unrecoverable,
		"deferred_sessions", stats.Deferred, "deferred_messages", stats.DeferredMessages,
		"dry_run", opts.DryRun)
	return stats, nil
}

// unallocatedMsg is one method='unallocated' message considered for recovery.
type unallocatedMsg struct {
	messageID string
	agentName string
	ts        time.Time
}

// unallocatedSession is one (session, host, username) group still holding
// method='unallocated' rows, with its message count. The host/username come from
// token_ledger (the scraper stamps them); they decide whether this host can read
// the session's transcript. Grouping by all three means a session_id that somehow
// spans hosts is split into per-host groups, each scoped correctly.
type unallocatedSession struct {
	sessionID string
	host      string
	username  string
	msgCount  int
}

// unallocatedSessions returns the (session, host, username) groups that still have
// method='unallocated' rows — the only sessions a recovery pass need touch (spec
// §7.3 cadence). host/username are carried so RecoverFocus can host-scope: only a
// group whose host+username match this host has a local transcript to read.
func (r *Runner) unallocatedSessions(ctx context.Context) ([]unallocatedSession, error) {
	const q = `
		SELECT t.session_id, t.host, t.username, COUNT(*)
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated' AND t.session_id <> ''
		GROUP BY t.session_id, t.host, t.username`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []unallocatedSession
	for rows.Next() {
		var s unallocatedSession
		if err := rows.Scan(&s.sessionID, &s.host, &s.username, &s.msgCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// unallocatedMessages returns the method='unallocated' messages of ONE
// (session, host, username) group with the (agent, ts) the timeline lookup needs.
// It is scoped to host+username (not just session_id) so a session that spanned
// hosts only yields the rows whose transcript IS on this host — matching the
// group RecoverFocus already verified is local. message_id is the composite
// ledger key (message.id|requestId) the scraper wrote — the same key
// transcript.DedupKey builds, which is why the timeline ts and the ledger ts
// align exactly (spec §2.4, §3.7).
func (r *Runner) unallocatedMessages(ctx context.Context, s unallocatedSession) ([]unallocatedMsg, error) {
	const q = `
		SELECT ua.message_id, t.agent_name, t.timestamp
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated'
		  AND t.session_id = ? AND t.host = ? AND t.username = ?`
	rows, err := r.db.QueryContext(ctx, q, s.sessionID, s.host, s.username)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []unallocatedMsg
	for rows.Next() {
		var m unallocatedMsg
		if err := rows.Scan(&m.messageID, &m.agentName, &m.ts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// applyRecovery re-attributes one unallocated message to the entity its matched
// setFocus names, and records the provenance — both in one transaction so a
// recovered row always has its evidence (and an interrupted pass never leaves an
// orphan evidence row or an un-evidenced recovery).
//
// The usage_attribution write is an UPDATE scoped to method='unallocated' (so a
// concurrent change that already moved this row off unallocated makes it a 0-row
// no-op, never clobbering good attribution). It rewrites the PK entity columns in
// place — correct because the message has exactly one row and the recovered entity
// (never '') cannot collide with the '' row being replaced.
func (r *Runner) applyRecovery(ctx context.Context, m unallocatedMsg, ev transcript.FocusEvent, now time.Time) error {
	// Resolve the recovered entity's covering kind='state' interval for cost-by-
	// phase, exactly as Allocate does (a miss leaves interval_id=0, harmless).
	var intervalID uint64
	if id, ok, err := r.intervalAt(ctx, ev.EntityType, ev.EntityID, m.ts); err != nil {
		return fmt.Errorf("intervalAt: %w", err)
	} else if ok {
		intervalID = id
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_attribution
		SET entity_type = ?, entity_id = ?, method = ?, interval_id = ?, computed_at = ?
		WHERE message_id = ? AND method = 'unallocated'`,
		ev.EntityType, ev.EntityID, recoveryMethod, intervalID, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// The row was no longer unallocated (raced or already recovered). Skip the
		// evidence write so we never record evidence for a row we didn't move.
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO recovery_evidence
			(message_id, entity_type, entity_id, setfocus_at, recovered_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type  = VALUES(entity_type),
			entity_id    = VALUES(entity_id),
			setfocus_at  = VALUES(setfocus_at),
			recovered_at = VALUES(recovered_at)`,
		m.messageID, ev.EntityType, ev.EntityID, ev.Timestamp, now); err != nil {
		return fmt.Errorf("insert evidence: %w", err)
	}

	return tx.Commit()
}

// Unrecover reverses a recovery pass (spec §5.4): it deletes every
// method='transcript_focus_recovery' attribution and its evidence, returning those
// messages to the unallocated bucket. After Unrecover, a normal Allocate restores
// the prior unallocated rows (the anti-join re-picks them since their attribution
// row is gone). Returns the number of attribution rows reverted.
//
// Scoped strictly to the recovery method, so it can never disturb temporal_join,
// the lead fallbacks, or unallocated rows — only what recovery itself wrote.
func (r *Runner) Unrecover(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Evidence first (it references the recovered rows by message_id); both are
	// scoped to the recovery method so nothing else is touched.
	if _, err := tx.ExecContext(ctx, `
		DELETE re FROM recovery_evidence re
		JOIN usage_attribution ua ON ua.message_id = re.message_id
		WHERE ua.method = ?`, recoveryMethod); err != nil {
		return 0, fmt.Errorf("delete evidence: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = ?`, recoveryMethod)
	if err != nil {
		return 0, fmt.Errorf("delete recovered attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// warmupMethod is the attribution method label for warmup cost re-attributed from
// the pre-first-setFocus interval to the session's resolved outcome. Distinct from
// transcript_focus_recovery (which handles post-first-setFocus unallocated cost)
// and from all live join methods. See KIT §3.2 and semantic-conventions.md §7.2.
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
// outcome under a new phase=admin. For each local session with unallocated rows:
//
//  1. Build the intended-focus timeline (reusing the same FocusTimelineSource as
//     RecoverFocus).
//  2. Identify warmup messages: those whose timestamp precedes the thread's first
//     setFocus. If the thread has NO setFocus at all, the message is not warmup
//     (it belongs to Objective 2 — no-focus sessions); skip it.
//  3. Resolve the session's outcome: the parent Outcome of the first-focused entity.
//     Use the first setFocus on any thread in the timeline and look up the entity
//     hierarchy to find a covering Outcome (entity_type='outcome'); if the first
//     focus IS an outcome, use it directly.
//  4. UPDATE the unallocated row in place: entity→resolved outcome,
//     method='admin_warmup'. Create a synthetic kind='state' interval
//     [session start, first focus) with phase='admin' so intervalAt naturally
//     returns it for cost-by-phase.
//  5. Record warmup_evidence provenance.
//
// CONSERVATION: in-place UPDATE scoped to method='unallocated'; one row/message,
// weight 1.0; SUM(cost_facts) unchanged.
//
// REVERSIBLE: UncoverWarmup deletes by method + removes synthetic admin intervals
// and evidence.
func (r *Runner) RecoverWarmup(ctx context.Context, opts RecoverOptions) (WarmupStats, error) {
	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	sessions, err := r.unallocatedSessions(ctx)
	if err != nil {
		return WarmupStats{}, fmt.Errorf("list unallocated sessions: %w", err)
	}

	now := time.Now().UTC()
	var stats WarmupStats
	deferredByHost := map[string]int{}

	for _, s := range sessions {
		if opts.scoped() && (s.host != opts.Host || !localToUser(s.username, opts.User)) {
			stats.Deferred++
			stats.DeferredMessages += s.msgCount
			deferredByHost[s.host+"\x00"+s.username] += s.msgCount
			continue
		}

		stats.Sessions++
		tl, err := src(s.sessionID, opts.ProjectsDir)
		if err != nil {
			r.log.Warn("recover-warmup: timeline build failed; skipping session",
				"session_id", s.sessionID, "error", err)
			continue
		}

		// The timeline must have at least one setFocus on some thread — otherwise
		// this is a no-focus session (Objective 2 territory), not a warmup session.
		if len(tl.Events) == 0 {
			continue
		}

		// Find the session-global first setFocus across ALL threads.
		var sessionFirstFocus *FocusEvent
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

		// Resolve the session's outcome: the first-focused entity's parent Outcome.
		outcomeType, outcomeID, err := r.resolveOutcome(ctx, sessionFirstFocus.EntityType, sessionFirstFocus.EntityID)
		if err != nil {
			r.log.Warn("recover-warmup: outcome resolution failed; skipping session",
				"session_id", s.sessionID, "error", err)
			continue
		}
		if outcomeType == "" || outcomeID == "" {
			// No outcome found — can't attribute warmup cost.
			continue
		}

		msgs, err := r.unallocatedMessages(ctx, s)
		if err != nil {
			return stats, fmt.Errorf("session %s: list unallocated messages: %w", s.sessionID, err)
		}

		// Partition: warmup = messages that predate their thread's first setFocus.
		var warmupMsgs []unallocatedMsg
		for _, m := range msgs {
			threadEvs := tl.Events[m.agentName]
			if len(threadEvs) == 0 {
				// This thread has no setFocus at all. If the lead thread does, fall
				// back: check if the message predates the lead's first setFocus.
				if m.agentName != "" {
					leadEvs := tl.Events[""]
					if len(leadEvs) > 0 && m.ts.Before(leadEvs[0].Timestamp) {
						warmupMsgs = append(warmupMsgs, m)
					}
				}
				continue
			}
			if m.ts.Before(threadEvs[0].Timestamp) {
				warmupMsgs = append(warmupMsgs, m)
			}
		}

		if len(warmupMsgs) == 0 {
			continue
		}

		// Determine warmup interval bounds for this session: earliest message ts →
		// session's first setFocus. These are the provenance bounds.
		warmupStart := warmupMsgs[0].ts
		for _, m := range warmupMsgs[1:] {
			if m.ts.Before(warmupStart) {
				warmupStart = m.ts
			}
		}

		// Create (or reuse) a synthetic admin state-interval so intervalAt covers
		// the warmup window with phase=admin.
		var adminIntervalID uint64
		if !opts.DryRun {
			id, err := r.ensureAdminInterval(ctx, s.sessionID, outcomeType, outcomeID, warmupStart, sessionFirstFocus.Timestamp)
			if err != nil {
				return stats, fmt.Errorf("session %s: ensure admin interval: %w", s.sessionID, err)
			}
			adminIntervalID = id
		}

		for _, m := range warmupMsgs {
			stats.Examined++

			if opts.DryRun {
				stats.Recovered++
				r.log.Info("recover-warmup (dry-run): would re-attribute",
					"message_id", m.messageID, "session_id", s.sessionID,
					"agent_name", m.agentName,
					"to_entity_type", outcomeType, "to_entity_id", outcomeID,
					"warmup_start", warmupStart, "first_focus_at", sessionFirstFocus.Timestamp)
				continue
			}

			if err := r.applyWarmupRecovery(ctx, m, outcomeType, outcomeID, adminIntervalID, warmupStart, sessionFirstFocus.Timestamp, now); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply warmup: %w", s.sessionID, m.messageID, err)
			}
			stats.Recovered++
		}
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
		rows, err := r.BuildCostRollup(ctx)
		if err != nil {
			return stats, fmt.Errorf("recover-warmup: rebuild cost_rollup: %w", err)
		}
		intervals, err := r.AssembleIntervalCost(ctx)
		if err != nil {
			return stats, fmt.Errorf("recover-warmup: reassemble interval cost: %w", err)
		}
		r.log.Info("recover-warmup rebuilt aggregates", "rollup_rows", rows, "intervals_costed", intervals)
	}

	r.log.Info("recover-warmup pass complete",
		"sessions", stats.Sessions, "examined", stats.Examined,
		"recovered", stats.Recovered, "no_outcome", stats.NoOutcome,
		"deferred_sessions", stats.Deferred, "deferred_messages", stats.DeferredMessages,
		"dry_run", opts.DryRun)
	return stats, nil
}

// FocusEvent re-export for warmup recovery's sessionFirstFocus type usage.
type FocusEvent = transcript.FocusEvent

// resolveOutcome finds the parent Outcome for a given entity. If the entity is
// already an outcome, it is returned directly. For a workunit, we look up its
// parent outcome in the WMS hierarchy.
func (r *Runner) resolveOutcome(ctx context.Context, entityType, entityID string) (string, string, error) {
	if entityType == "outcome" {
		if !r.outcomeExists(ctx, entityID) {
			r.log.Warn("resolveOutcome: outcome row not found",
				"entity_id", entityID)
			return "", "", nil
		}
		return entityType, entityID, nil
	}
	if entityType == "workunit" {
		var parentID string
		err := r.db.QueryRowContext(ctx,
			`SELECT outcome_id FROM workunits WHERE id = ?`, entityID).Scan(&parentID)
		if err != nil {
			if err == sql.ErrNoRows {
				return "", "", nil
			}
			return "", "", fmt.Errorf("lookup workunit parent: %w", err)
		}
		if parentID != "" {
			if !r.outcomeExists(ctx, parentID) {
				r.log.Warn("resolveOutcome: parent outcome row not found",
					"workunit_id", entityID, "outcome_id", parentID)
				return "", "", nil
			}
			return "outcome", parentID, nil
		}
		return "", "", nil
	}
	return "", "", nil
}

func (r *Runner) outcomeExists(ctx context.Context, id string) bool {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM outcomes WHERE id = ?`, id).Scan(&n); err != nil {
		return false
	}
	return true
}

// ensureAdminInterval creates (or returns the existing) synthetic kind='state'
// interval covering [warmupStart, firstFocusAt) with phase='admin' for the
// resolved outcome entity. This interval is what intervalAt uses to assign
// phase=admin to warmup cost in cost-by-phase views.
//
// The synthetic interval is scoped by (entity_type, entity_id, session_id) plus
// the 'admin' phase so it never collides with real phase transitions.
// UncoverWarmup deletes it by the same scope.
func (r *Runner) ensureAdminInterval(ctx context.Context, sessionID, entityType, entityID string, warmupStart, firstFocusAt time.Time) (uint64, error) {
	// INSERT IGNORE so a concurrent caller harmlessly no-ops instead of failing.
	if _, err := r.db.ExecContext(ctx, `
		INSERT IGNORE INTO wms_intervals
			(kind, session_id, agent_name, entity_type, entity_id, state, started_at, ended_at, phase, phase_source)
		VALUES ('state', ?, '', ?, ?, 'admin', ?, ?, 'admin', 'warmup_recovery')`,
		sessionID, entityType, entityID, warmupStart, firstFocusAt); err != nil {
		return 0, err
	}

	// SELECT the id whether we inserted or it already existed.
	var id uint64
	if err := r.db.QueryRowContext(ctx, `
		SELECT id FROM wms_intervals
		WHERE kind = 'state' AND session_id = ? AND entity_type = ? AND entity_id = ?
		  AND phase = 'admin' AND phase_source = 'warmup_recovery'
		LIMIT 1`, sessionID, entityType, entityID).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// applyWarmupRecovery re-attributes one unallocated warmup message and records
// provenance, mirroring applyRecovery's transactional pattern.
func (r *Runner) applyWarmupRecovery(ctx context.Context, m unallocatedMsg, entityType, entityID string, intervalID uint64, warmupStart, firstFocusAt, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_attribution
		SET entity_type = ?, entity_id = ?, method = ?, interval_id = ?, computed_at = ?
		WHERE message_id = ? AND method = 'unallocated'`,
		entityType, entityID, warmupMethod, intervalID, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO warmup_evidence
			(message_id, entity_type, entity_id, warmup_start, first_focus_at, recovered_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type    = VALUES(entity_type),
			entity_id      = VALUES(entity_id),
			warmup_start   = VALUES(warmup_start),
			first_focus_at = VALUES(first_focus_at),
			recovered_at   = VALUES(recovered_at)`,
		m.messageID, entityType, entityID, warmupStart, firstFocusAt, now); err != nil {
		return fmt.Errorf("insert warmup evidence: %w", err)
	}

	return tx.Commit()
}

// UncoverWarmup reverses a warmup recovery pass: deletes every method='admin_warmup'
// attribution and its evidence, removes synthetic admin state-intervals, and returns
// those messages to the unallocated bucket.
func (r *Runner) UncoverWarmup(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		DELETE we FROM warmup_evidence we
		JOIN usage_attribution ua ON ua.message_id = we.message_id
		WHERE ua.method = ?`, warmupMethod); err != nil {
		return 0, fmt.Errorf("delete warmup evidence: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = ?`, warmupMethod)
	if err != nil {
		return 0, fmt.Errorf("delete warmup attribution: %w", err)
	}
	n, _ := res.RowsAffected()

	// Remove synthetic admin state-intervals created by the warmup pass.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM wms_intervals WHERE kind = 'state' AND phase = 'admin' AND phase_source = 'warmup_recovery'`); err != nil {
		return 0, fmt.Errorf("delete admin intervals: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
