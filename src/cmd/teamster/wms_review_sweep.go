package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// sweepAgentID is the fixed, non-live-session identity every close and
// journal row this command writes carries (WP3-DESIGN.md Operator Decision
// 1 + 9). Used both as the journal agent_id and as the StatusChange
// SessionID/AgentName — the SessionID assignment is what makes hookd's
// session-keyed side effects (focus-interval close, the W2 warning queue)
// run correctly instead of silently skipping (MAJOR-4) or impersonating a
// live interactive session (MAJOR-5). Sourced from wms.ReviewSweepAgentID —
// the single definition internal/store's two backends and hookd's
// warnMissingRequiredTags guard also read — rather than its own literal
// (wh2-sweep-warning-queue).
const sweepAgentID = wms.ReviewSweepAgentID

// sweepCounts accumulates the run summary §8/§9's log-line convention asks
// for: one line per population per stage, plus the gauge-notification and
// interval-drain counts.
type sweepCounts struct {
	wuParked, outcomeParked       int
	wuDone                        int
	wuAbandoned, outcomeAbandoned int
	// outcomeWalkSkipped counts an Outcome (either stage) whose
	// descendant-safety walk itself errored (could not verify safety —
	// errOpBudgetExceeded or a store error). There is no WorkUnit
	// equivalent (WorkUnits never go through the walk), so unlike the
	// WU/Outcome pairs above this is a single, entity-type-specific field,
	// not a pair.
	outcomeWalkSkipped int
	// outcomeBlocked counts an Outcome (either stage) the walk found a
	// genuine live or human-parked descendant under — correctly protected,
	// not an error, but also not a disposition (@bench/@auditor,
	// 2026-09-02: this and outcomeWalkSkipped were previously bare
	// `continue`s with no durable journal trace at all, only the latter
	// even had a counter).
	outcomeBlocked int
	// wuReviewSkipped/outcomeIdleSkipped/wuOnHoldSkipped/outcomeOnHoldSkipped
	// count the check-then-act re-verification's own skip disposition (see
	// stillReviewStage1Candidate/stillOutcomeStage1Candidate/
	// containsWorkUnitID/containsOutcomeID below): a candidate the initial
	// list query matched, but that no longer satisfies its candidacy
	// predicate by the time this run got to it (@auditor MAJOR,
	// 2026-09-02 — no status write in this command was guarded by a
	// conditional UPDATE, so a status change or updated_at bump between the
	// list and here would previously have been silently overwritten).
	wuReviewSkipped, outcomeIdleSkipped   int
	wuOnHoldSkipped, outcomeOnHoldSkipped int
	notified, notifyFailed                int
	intervalsDrained                      int64
}

func runWMSReviewSweep(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: load config: %v\n", err)
		return 1
	}

	fs := flag.NewFlagSet("teamster wms review-sweep", flag.ContinueOnError)
	olderThanStr := fs.String("older-than", cfg.ReviewSweep.OlderThan.String(), "Sweep Stage 1 idle threshold (review WorkUnits and stale Outcomes)")
	abandonAfterStr := fs.String("abandon-after", cfg.ReviewSweep.AbandonAfter.String(), "Sweep Stage 2 rescue-window threshold (on_hold -> abandoned)")
	confirm := fs.Bool("confirm", false, "actually execute (default is a dry-run preview; --confirm must be passed explicitly on every invocation, including the nightly timer's unattended run — teamster.yaml's ReviewSweep.Confirm no longer gates this, see incident note below)")
	noHookd := fs.Bool("no-hookd", !cfg.ReviewSweep.NotifyHookd, "disable hookd gauge/journal-mirror notification entirely for this run (default: ReviewSweep.NotifyHookd, else on)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	notifyHookd := cfg.ReviewSweep.NotifyHookd && !*noHookd
	// ReviewSweep.Confirm (teamster.yaml / TEAMSTER_REVIEW_SWEEP_CONFIRM) used
	// to seed this flag's default, so a bare `teamster wms review-sweep` (the
	// timer's unattended invocation) executed for real once an operator
	// flipped the config on — a real sweep ran unintentionally on chunk this
	// way. --confirm is now the ONLY gate for real execution; the config key
	// is read for nothing here and is kept only as a documentation-facing
	// record of intent, so warn loudly when it's set but silently doing
	// nothing, rather than leaving an operator to wonder why their "burn-in
	// flip" stopped working.
	if cfg.ReviewSweep.Confirm && !*confirm {
		fmt.Println("note: teamster.yaml's ReviewSweep.Confirm=true has no effect — pass --confirm on the command line to execute for real")
	}

	// Checked first and unconditionally — not a flag. Defense-in-depth
	// against a systemd timer left enabled after the operator toggled
	// Enabled back off, and against a fresh install where the timer wiring
	// and the config value could otherwise drift independently (§8, AC4).
	if !cfg.ReviewSweep.Enabled {
		fmt.Println("review-sweep: disabled (ReviewSweep.Enabled=false in teamster.yaml)")
		return 0
	}

	olderThan, err := parseDurationWithDays(*olderThanStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: --older-than: %v\n", err)
		return 1
	}
	abandonAfter, err := parseDurationWithDays(*abandonAfterStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: --abandon-after: %v\n", err)
		return 1
	}

	// Go-computed time.Time bound as a query parameter, never a SQL clock
	// function inside the query (MINOR-2: removes the session-time-zone
	// hazard by construction) — matches wms_gc.go's own staleThreshold line.
	now := time.Now().UTC()
	stage1Threshold := now.Add(-olderThan)
	stage2Threshold := now.Add(-abandonAfter)

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()
	host := hostForJournal()
	// Operator Decision 2: a standalone HookObserver, not a full Engine —
	// the cascade-relevant parts of OnStatusChange no longer exist, and the
	// remaining parts (dependency-unblock, advisory outcome-status log) are
	// a pre-existing engine-bypass gap wms_gc.go/wms_close.go already share,
	// not something this command is chartered to fix (§5, §7).
	hookObs := wms.NewHookObserver(cfg.HookServerURL, cfg.Host)

	// Printed unconditionally, dry-run or not, so nobody runs this command
	// blind to where it will post (the hookd leak incident this line exists
	// to prevent: cfg.HookServerURL is NEVER empty — config.Load()'s
	// Default() always constructs http://<hostname>:9125/event even with
	// TEAMSTER_HOOK_SERVER_URL unset — so unlike wms-mcp's raw-env check, an
	// empty URL can never mean "off" here; NotifyHookd/--no-hookd is the
	// only lever, and this line is how an operator or harness confirms
	// which state is actually in effect before anything runs).
	if notifyHookd {
		fmt.Printf("hookd notification target: %s\n", cfg.HookServerURL)
	} else {
		fmt.Println("hookd notification target: none (disabled via --no-hookd or ReviewSweep.NotifyHookd=false)")
	}

	isDryRun := !*confirm
	var counts sweepCounts
	var listing []string

	// --- Sweep Stage 1: review-status WorkUnits (§2a, §3 wu-review-*) ---
	reviewCandidates, err := s.ListReviewSweepCandidates(ctx, stage1Threshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: list review candidates: %v\n", err)
		return 1
	}
	for _, wu := range reviewCandidates {
		// Re-fetch immediately before disposition and re-apply §2a's
		// candidacy predicate — closes the check-then-act window between
		// the list above and this entity's turn (@auditor MAJOR,
		// 2026-09-02): UpdateWorkUnitStatus carries no status guard (a
		// plain `WHERE id = ?`), so a human's status change or updated_at
		// bump in between would previously have been silently overwritten.
		// Delivered-ness (hasDeliverable, below) was already read live at
		// disposal time, not from the stale list snapshot — only the
		// status/idle fields needed this fix.
		fresh, ferr := s.GetWorkUnit(ctx, wu.ID)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: re-check workunit %s: %v\n", wu.ID, ferr)
			continue
		}
		if !stillReviewStage1Candidate(fresh, stage1Threshold) {
			days := daysSince(fresh.UpdatedAt, now)
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s workunit  %-9s idle %-13s wu-review-skipped      -> (left open, no longer a candidate)", wu.ID, fresh.Status, fmt.Sprintf("%dd", days)))
				counts.wuReviewSkipped++
				continue
			}
			notes := fmt.Sprintf("wu-review-skipped (threshold %s): idle %dd, no longer a candidate at disposition time (status=%s)", *olderThanStr, days, fresh.Status)
			recordSweepSkip(ctx, s, host, wms.EntityWorkUnit, wu.ID, notes)
			counts.wuReviewSkipped++
			continue
		}
		wu = fresh

		delivered, derr := hasDeliverable(ctx, s, wms.EntityWorkUnit, wu.ID)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: check deliverable for %s: %v\n", wu.ID, derr)
			continue
		}
		days := daysSince(wu.UpdatedAt, now)
		if delivered {
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s workunit  %-9s idle %-13s wu-review-delivered    -> done (resolution: swept-unreviewed)", wu.ID, wu.Status, fmt.Sprintf("%dd", days)))
				counts.wuDone++
				continue
			}
			notes := fmt.Sprintf("wu-review-delivered (threshold %s): idle %dd, deliverable on file", *olderThanStr, days)
			if uerr := s.UpdateWorkUnitStatus(ctx, wu.ID, wms.StatusDone); uerr != nil {
				fmt.Fprintf(os.Stderr, "wms review-sweep: close workunit %s: %v\n", wu.ID, uerr)
				continue
			}
			if terr := s.TagEntity(ctx, wms.EntityWorkUnit, wu.ID, "resolution", "swept-unreviewed", "manual", ""); terr != nil {
				fmt.Fprintf(os.Stderr, "wms review-sweep: tag resolution for %s: %v\n", wu.ID, terr)
			}
			recordSweepClose(ctx, s, hookObs, host, notifyHookd, wms.EntityWorkUnit, wu.ID, wu.Status, wms.StatusDone, notes, &counts)
			fmt.Printf("stage 1: closed workunit %s as done, resolution=swept-unreviewed (%s)\n", wu.ID, wu.Title)
			counts.wuDone++
		} else {
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s workunit  %-9s idle %-13s wu-review-undelivered  -> on_hold", wu.ID, wu.Status, fmt.Sprintf("%dd", days)))
				counts.wuParked++
				continue
			}
			notes := fmt.Sprintf("wu-review-undelivered (threshold %s): idle %dd, no deliverable on file, parked to on_hold", *olderThanStr, days)
			if uerr := s.UpdateWorkUnitStatus(ctx, wu.ID, wms.StatusOnHold); uerr != nil {
				fmt.Fprintf(os.Stderr, "wms review-sweep: park workunit %s: %v\n", wu.ID, uerr)
				continue
			}
			recordSweepClose(ctx, s, hookObs, host, notifyHookd, wms.EntityWorkUnit, wu.ID, wu.Status, wms.StatusOnHold, notes, &counts)
			fmt.Printf("stage 1: parked workunit %s to on_hold (%s)\n", wu.ID, wu.Title)
			counts.wuParked++
		}
	}

	// --- Sweep Stage 1: stale Outcomes (§2b, §3 outcome-idle) ---
	// ListStaleOutcomeCandidates' candidacy filter alone is NOT sufficient
	// (MAJOR-1: terminality is not transitive) — every candidate must also
	// pass the descendant-safety walk before being parked.
	staleOutcomes, err := s.ListStaleOutcomeCandidates(ctx, stage1Threshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: list stale outcome candidates: %v\n", err)
		return 1
	}
	for _, o := range staleOutcomes {
		// Re-fetch and re-apply §2b's candidacy-filter status/idle clause —
		// same fix as the WorkUnit loop above (@auditor MAJOR). The
		// interlock and descendant-safety walk below are ALREADY a fresh,
		// live read (outcomeHasLiveDescendant composes entirely from
		// wms.Reader calls made right now, never from the stale list
		// snapshot) — only the candidate's own status/updated_at needed a
		// re-fetch.
		fresh, ferr := s.GetOutcome(ctx, o.ID)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: re-check outcome %s: %v\n", o.ID, ferr)
			continue
		}
		if !stillOutcomeStage1Candidate(fresh, stage1Threshold) {
			days := daysSince(fresh.UpdatedAt, now)
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s idle %-13s outcome-idle-skipped   -> (left open, no longer a candidate)", o.ID, fresh.Status, fmt.Sprintf("%dd", days)))
				counts.outcomeIdleSkipped++
				continue
			}
			notes := fmt.Sprintf("outcome-idle-skipped (threshold %s): idle %dd, no longer a candidate at disposition time (status=%s)", *olderThanStr, days, fresh.Status)
			recordSweepSkip(ctx, s, host, wms.EntityOutcome, o.ID, notes)
			counts.outcomeIdleSkipped++
			continue
		}
		o = fresh

		days := daysSince(o.UpdatedAt, now)
		live, walkErr := outcomeHasLiveDescendant(ctx, s, o.ID)
		if walkErr != nil {
			reportWalkError(o.ID, walkErr)
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s idle %-13s outcome-idle-skipped   -> (left open, %s)", o.ID, o.Status, fmt.Sprintf("%dd", days), walkErrorReason(walkErr)))
			} else {
				notes := fmt.Sprintf("outcome-idle-skipped (threshold %s): idle %dd, %s", *olderThanStr, days, walkErrorReason(walkErr))
				recordSweepSkip(ctx, s, host, wms.EntityOutcome, o.ID, notes)
			}
			counts.outcomeWalkSkipped++
			continue
		}
		if live {
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s idle %-13s outcome-idle-skipped   -> (left open, live or human-parked descendant)", o.ID, o.Status, fmt.Sprintf("%dd", days)))
			} else {
				notes := fmt.Sprintf("outcome-idle-skipped (threshold %s): idle %dd, live or human-parked descendant found, not safe to park", *olderThanStr, days)
				recordSweepSkip(ctx, s, host, wms.EntityOutcome, o.ID, notes)
			}
			counts.outcomeBlocked++
			continue
		}
		if isDryRun {
			listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s idle %-13s outcome-idle           -> on_hold", o.ID, o.Status, fmt.Sprintf("%dd", days)))
			counts.outcomeParked++
			continue
		}
		notes := fmt.Sprintf("outcome-idle (threshold %s): idle %dd, 0 direct live children, subtree walk clean, parked to on_hold", *olderThanStr, days)
		if uerr := s.UpdateOutcomeStatus(ctx, o.ID, wms.StatusOnHold); uerr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: park outcome %s: %v\n", o.ID, uerr)
			continue
		}
		recordSweepClose(ctx, s, hookObs, host, notifyHookd, wms.EntityOutcome, o.ID, o.Status, wms.StatusOnHold, notes, &counts)
		fmt.Printf("stage 1: parked outcome %s to on_hold (%s)\n", o.ID, o.Title)
		counts.outcomeParked++
	}

	// --- Sweep Stage 2: sweep-parked WorkUnits past the rescue window (§2c, §3 wu-onhold-abandon) ---
	abandonWU, err := s.ListOnHoldAbandonCandidateWorkUnits(ctx, stage2Threshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: list stage-2 workunit candidates: %v\n", err)
		return 1
	}
	for _, wu := range abandonWU {
		// Re-apply §2c's full four-condition candidacy predicate
		// immediately before disposition (@auditor MAJOR). Unlike Stage 1's
		// simple two-field check, Stage 2's predicate (status, the
		// sweep-parked discriminator, and "no non-sweep journal/interval/
		// deliverable activity since the parking timestamp T") is exactly
		// what ListOnHoldAbandonCandidateWorkUnits' SQL already encodes —
		// re-deriving it a second time in Go would be a second,
		// independently-drift-prone implementation of the same rule (the
		// design's own stated reason isSweepParked/isLive are composed from
		// primitives instead of duplicating the SQL, §2c). Re-running the
		// SAME store query and checking membership reuses that one already-
		// reviewed predicate instead of duplicating it.
		recheck, rerr := s.ListOnHoldAbandonCandidateWorkUnits(ctx, stage2Threshold)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: re-check stage-2 workunit candidates: %v\n", rerr)
			continue
		}
		if !containsWorkUnitID(recheck, wu.ID) {
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s workunit  %-9s parked        onhold-evaluated-skipped -> (left on_hold, no longer a stage-2 candidate)", wu.ID, wu.Status))
				counts.wuOnHoldSkipped++
				continue
			}
			notes := fmt.Sprintf("onhold-evaluated-skipped (threshold %s): no longer a stage-2 candidate at disposition time", *abandonAfterStr)
			recordSweepSkip(ctx, s, host, wms.EntityWorkUnit, wu.ID, notes)
			counts.wuOnHoldSkipped++
			continue
		}
		if isDryRun {
			listing = append(listing, fmt.Sprintf("  %-14s workunit  %-9s parked        wu-onhold-abandon      -> abandoned", wu.ID, wu.Status))
			counts.wuAbandoned++
			continue
		}
		notes := fmt.Sprintf("wu-onhold-abandon (threshold %s): parked by %s, untouched past the rescue window, no journal/interval/deliverable activity, abandoned", *abandonAfterStr, sweepAgentID)
		if uerr := s.UpdateWorkUnitStatus(ctx, wu.ID, wms.StatusAbandoned); uerr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: abandon workunit %s: %v\n", wu.ID, uerr)
			continue
		}
		recordSweepClose(ctx, s, hookObs, host, notifyHookd, wms.EntityWorkUnit, wu.ID, wu.Status, wms.StatusAbandoned, notes, &counts)
		fmt.Printf("stage 2: abandoned workunit %s (%s)\n", wu.ID, wu.Title)
		counts.wuAbandoned++
	}

	// --- Sweep Stage 2: sweep-parked Outcomes past the rescue window (§2c, §3 outcome-onhold-abandon) ---
	// Candidacy alone is necessary but not sufficient — state can change
	// during the rescue window, so every candidate is re-verified against
	// the SAME descendant-safety walk used at parking time (§2c's fifth
	// step: "the same code, called again, not a separate mechanism" — the
	// walk already subsumes the direct-child interlock, since it inspects
	// every descendant at every level, not just direct children).
	abandonOC, err := s.ListOnHoldAbandonCandidateOutcomes(ctx, stage2Threshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: list stage-2 outcome candidates: %v\n", err)
		return 1
	}
	for _, o := range abandonOC {
		// Same fix as the Stage-2 WorkUnit loop above: re-run the store's
		// own candidacy query and check membership, closing the
		// check-then-act window on status/sweep-parked/no-later-activity.
		// The descendant-safety walk below is separately re-verified live
		// (already required by §2c's "fifth step" — this was not new).
		recheck, rerr := s.ListOnHoldAbandonCandidateOutcomes(ctx, stage2Threshold)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: re-check stage-2 outcome candidates: %v\n", rerr)
			continue
		}
		if !containsOutcomeID(recheck, o.ID) {
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s parked        onhold-evaluated-skipped -> (left on_hold, no longer a stage-2 candidate)", o.ID, o.Status))
				counts.outcomeOnHoldSkipped++
				continue
			}
			notes := fmt.Sprintf("onhold-evaluated-skipped (threshold %s): no longer a stage-2 candidate at disposition time", *abandonAfterStr)
			recordSweepSkip(ctx, s, host, wms.EntityOutcome, o.ID, notes)
			counts.outcomeOnHoldSkipped++
			continue
		}

		live, walkErr := outcomeHasLiveDescendant(ctx, s, o.ID)
		if walkErr != nil {
			reportWalkError(o.ID, walkErr)
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s parked        onhold-evaluated-skipped -> (left on_hold, %s)", o.ID, o.Status, walkErrorReason(walkErr)))
			} else {
				notes := fmt.Sprintf("onhold-evaluated-skipped (threshold %s): during re-verification, %s", *abandonAfterStr, walkErrorReason(walkErr))
				recordSweepSkip(ctx, s, host, wms.EntityOutcome, o.ID, notes)
			}
			counts.outcomeWalkSkipped++
			continue
		}
		if live {
			if isDryRun {
				listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s parked        onhold-evaluated-skipped -> (left on_hold, live or human-parked descendant)", o.ID, o.Status))
			} else {
				notes := fmt.Sprintf("onhold-evaluated-skipped (threshold %s): live or human-parked descendant found during re-verification, not safe to abandon", *abandonAfterStr)
				recordSweepSkip(ctx, s, host, wms.EntityOutcome, o.ID, notes)
			}
			counts.outcomeBlocked++
			continue
		}
		if isDryRun {
			listing = append(listing, fmt.Sprintf("  %-14s outcome   %-9s parked        outcome-onhold-abandon -> abandoned", o.ID, o.Status))
			counts.outcomeAbandoned++
			continue
		}
		notes := fmt.Sprintf("outcome-onhold-abandon (threshold %s): parked by %s, untouched past the rescue window, interlock+walk re-verified clean, abandoned", *abandonAfterStr, sweepAgentID)
		if uerr := s.UpdateOutcomeStatus(ctx, o.ID, wms.StatusAbandoned); uerr != nil {
			fmt.Fprintf(os.Stderr, "wms review-sweep: abandon outcome %s: %v\n", o.ID, uerr)
			continue
		}
		recordSweepClose(ctx, s, hookObs, host, notifyHookd, wms.EntityOutcome, o.ID, o.Status, wms.StatusAbandoned, notes, &counts)
		fmt.Printf("stage 2: abandoned outcome %s (%s)\n", o.ID, o.Title)
		counts.outcomeAbandoned++
	}

	if isDryRun {
		fmt.Println("[dry-run] review-sweep would:")
		for _, line := range listing {
			fmt.Println(line)
		}
		fmt.Printf("would park %d workunit(s), %d outcome(s); close done %d workunit(s); abandon %d workunit(s), %d outcome(s); skip %d workunit(s), %d outcome(s) (no longer a candidate at disposition time); leave %d outcome(s) blocked (live/human-parked descendant), %d outcome(s) unverifiable\n",
			counts.wuParked, counts.outcomeParked, counts.wuDone, counts.wuAbandoned, counts.outcomeAbandoned,
			counts.wuReviewSkipped+counts.wuOnHoldSkipped, counts.outcomeIdleSkipped+counts.outcomeOnHoldSkipped,
			counts.outcomeBlocked, counts.outcomeWalkSkipped)
		fmt.Println("pass --confirm to execute")
		return 0
	}

	// Phase: drain intervals on whatever this run just made terminal — same
	// primitive and placement as wms_gc.go's own phase 5
	// (closeStaleEntitiesAndDrain), called once after every population's
	// writes complete. Sweep Stage 1 parking (-> on_hold) drains nothing,
	// since on_hold is not wms.IsTerminal — a named, not-fixed gap (§7).
	n, derr := s.CloseIntervalsOnTerminalEntities(ctx)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: drain intervals: %v\n", derr)
	} else {
		counts.intervalsDrained = n
	}

	if notifyHookd {
		fmt.Printf("notified hookd: %d/%d closes\n", counts.notified, counts.notified+counts.notifyFailed)
	} else {
		fmt.Println("hookd notification: disabled for this run, nothing posted")
	}
	if counts.intervalsDrained > 0 {
		fmt.Printf("drained %d interval(s) on newly-terminal entities\n", counts.intervalsDrained)
	}
	if counts.outcomeWalkSkipped > 0 {
		fmt.Printf("could not verify safety for %d outcome(s) this run; journaled and re-evaluated next run\n", counts.outcomeWalkSkipped)
	}
	if counts.outcomeBlocked > 0 {
		fmt.Printf("left %d outcome(s) untouched: live or human-parked descendant found; journaled and re-evaluated next run\n", counts.outcomeBlocked)
	}
	if counts.wuReviewSkipped+counts.outcomeIdleSkipped+counts.wuOnHoldSkipped+counts.outcomeOnHoldSkipped > 0 {
		fmt.Printf("no longer a candidate at disposition time: %d workunit(s), %d outcome(s); journaled and left open, re-evaluated next run\n",
			counts.wuReviewSkipped+counts.wuOnHoldSkipped, counts.outcomeIdleSkipped+counts.outcomeOnHoldSkipped)
	}
	fmt.Printf("review-sweep complete: parked %d workunit(s), %d outcome(s); closed done %d workunit(s); abandoned %d workunit(s), %d outcome(s)\n",
		counts.wuParked, counts.outcomeParked, counts.wuDone, counts.wuAbandoned, counts.outcomeAbandoned)
	return 0
}

// stillReviewStage1Candidate reports whether wu still satisfies §2a's
// candidacy predicate (status='review' AND updated_at < threshold) — the
// literal two-field re-check the Stage-1 WorkUnit loop applies to a fresh
// fetch immediately before writing, closing the window between the initial
// candidate list and this entity's disposition (@auditor MAJOR,
// 2026-09-02): pass it the entity as it stood AT LIST TIME and it proves
// nothing; pass it the entity as re-fetched just now and a status change or
// updated_at bump that happened in between is exactly what flips this false.
func stillReviewStage1Candidate(wu *wms.WorkUnit, threshold time.Time) bool {
	return wu.Status == wms.StatusReview && wu.UpdatedAt.Before(threshold)
}

// stillOutcomeStage1Candidate reports whether o still satisfies §2b's
// candidacy filter's own status/idle clause: `NOT IN ('done','abandoned',
// 'on_hold')` and idle past threshold. This is deliberately narrower than
// the full candidacy filter — the interlock and descendant-safety walk are
// re-verified separately, live, by outcomeHasLiveDescendant (composed
// entirely from fresh wms.Reader calls, never from the stale list snapshot),
// so only the candidate's own two fields needed a re-fetch-based re-check.
func stillOutcomeStage1Candidate(o *wms.Outcome, threshold time.Time) bool {
	return o.Status != wms.StatusDone && o.Status != wms.StatusAbandoned &&
		o.Status != wms.StatusOnHold && o.UpdatedAt.Before(threshold)
}

// containsWorkUnitID and containsOutcomeID report whether id appears in a
// freshly re-run Stage-2 candidate list — see the Stage-2 loops' own comment
// for why Stage 2's four-condition predicate (status, the sweep-parked
// discriminator, no non-sweep activity since the parking timestamp) is
// re-verified by re-running the store's own already-reviewed query rather
// than re-deriving the same rule a second time in Go.
func containsWorkUnitID(list []*wms.WorkUnit, id string) bool {
	for _, wu := range list {
		if wu.ID == id {
			return true
		}
	}
	return false
}

func containsOutcomeID(list []*wms.Outcome, id string) bool {
	for _, o := range list {
		if o.ID == id {
			return true
		}
	}
	return false
}

// hasDeliverable reports whether entityID has at least one submitted
// deliverable — "delivered" means a real wms_deliverables row (§3), checked
// per-entity via the existing ListDeliverables(limit=1) rather than a new
// store method (§10's note: either is functionally correct; this is the
// simpler of the two, left as an implementation judgment call by design).
func hasDeliverable(ctx context.Context, r wms.Reader, entityType, entityID string) (bool, error) {
	d, err := r.ListDeliverables(ctx, entityType, entityID, 1)
	if err != nil {
		return false, err
	}
	return len(d) > 0, nil
}

// recordSweepClose writes the audit journal entry and, when notifyHookd is
// true, notifies hookd for one close — the two side effects every
// disposition rule needs (§4, §5). SessionID is set to sweepAgentID on the
// StatusChange specifically (Operator Decision 9): change.SessionID always
// overrides HookObserver's own session fallback chain once non-empty, for
// both the top-level session_id field and wms_session_id — without it,
// hookd's session-keyed side effects (focus-interval close, the W2 warning
// queue) silently never run, and the top-level session_id field would
// impersonate whatever session happens to be live on the host (MAJOR-4,
// MAJOR-5). notifyHookd=false skips the POST entirely — no attempt, no
// counted failure — the explicit opt-out lever (config NotifyHookd /
// --no-hookd) this design's own hookd-leak incident showed was missing.
func recordSweepClose(ctx context.Context, w wms.JournalWriter, hookObs *wms.HookObserver, host string, notifyHookd bool,
	entityType, entityID, priorStatus, newStatus, notes string, counts *sweepCounts) {
	entry := wms.JournalEntry{
		EntityType: entityType, EntityID: entityID,
		Field: "status", OldValue: priorStatus, NewValue: newStatus,
		Notes: notes, Host: host, AgentID: sweepAgentID,
	}
	if rerr := wms.RecordMutation(ctx, w, entry); rerr != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: record mutation for %s: %v\n", entityID, rerr)
	}
	if !notifyHookd {
		return
	}
	if hookObs.PostStatusChange(wms.StatusChange{
		EntityType: entityType, EntityID: entityID,
		OldStatus: priorStatus, NewStatus: newStatus,
		AgentName: sweepAgentID, Host: host, SessionID: sweepAgentID,
	}) {
		counts.notified++
	} else {
		counts.notifyFailed++
	}
}

// recordSweepSkip writes the audit journal entry for a candidate the sweep
// evaluated but left alone — no status change, so no hookd notification
// (§5's PostStatusChange is for closes only). Sibling to recordSweepClose,
// added because AC3 ("skipped at either stage -> field='sweep_evaluated'")
// applies to every candidate a run's list queries returned, not only the
// ones actually disposed (@bench, @auditor, 2026-09-02: the
// descendant-safety walk's two non-disposal outcomes — a genuine live
// descendant found, and the walk itself erroring — were previously bare
// `continue`s with no durable trace at all, the same gap the check-then-act
// re-verification's own skip cases needed this for).
func recordSweepSkip(ctx context.Context, w wms.JournalWriter, host, entityType, entityID, notes string) {
	entry := wms.JournalEntry{
		EntityType: entityType, EntityID: entityID,
		Field: "sweep_evaluated", NewValue: "skipped",
		Notes: notes, Host: host, AgentID: sweepAgentID,
	}
	if rerr := wms.RecordMutation(ctx, w, entry); rerr != nil {
		fmt.Fprintf(os.Stderr, "wms review-sweep: record skip for %s: %v\n", entityID, rerr)
	}
}

// walkErrorReason (shared with wms gc — see wms_descendant_walk.go) renders
// walkErr as the one-clause reason text used both in this command's
// operator-facing stderr log (reportWalkError, below) and its durable
// journal skip row (recordSweepSkip).
func reportWalkError(outcomeID string, err error) {
	fmt.Fprintf(os.Stderr, "wms review-sweep: outcome %s: %s\n", outcomeID, walkErrorReason(err))
}

func daysSince(t, now time.Time) int {
	return int(now.Sub(t).Hours() / 24)
}

// outcomeHasLiveDescendant, isLive, isSweepParked, walkErrorReason, and the
// errOpBudgetExceeded/errJournalWindowTruncated sentinels live in
// wms_descendant_walk.go — extracted there so wms gc's phase 4 (wms_gc.go)
// can reuse the exact same descendant-safety walk instead of its own
// direct-children-only check.
