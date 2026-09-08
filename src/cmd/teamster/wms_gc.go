package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
	"golang.org/x/term"
)

func runWMSGC(args []string) int {
	fs := flag.NewFlagSet("teamster wms gc", flag.ContinueOnError)
	olderThan := fs.String("older-than", "7d", "stale threshold for closing entities")
	confirm := fs.Bool("confirm", false, "actually execute the gc (default is a dry-run preview; --confirm is required but not sufficient by itself — an interactive 'yes' prompt is also required, see confirmGCInteractive)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	isDryRun := !*confirm

	d, err := parseDurationWithDays(*olderThan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: --older-than: %v\n", err)
		return 1
	}
	staleThreshold := time.Now().UTC().Add(-d)

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	// countStaleEntities' error is ignored here — the dry-run preview stays
	// lenient (best-effort partial counts, same as before this gate
	// existed) since nothing is at stake yet. The --confirm path below
	// checks staleErr itself: a miscounted blast radius on the path that
	// actually executes would defeat the point of the warning it feeds.
	staleOutcomes, staleWUs, candidates, staleErr := countStaleEntities(ctx, s, staleThreshold)

	// Phase 1: drain intervals on done entities
	if isDryRun {
		fmt.Println("[dry-run] gc would:")
		fmt.Println("  1. close intervals on terminal entities")
		fmt.Println("  2. close intervals on closed sessions")
		fmt.Printf("  3. close intervals on sessions idle since %s\n",
			staleThreshold.Local().Format("2006-01-02 15:04:05"))
		fmt.Println("  4. close stale non-terminal entities as abandoned")
		fmt.Println("  5. close intervals on newly-terminal entities")

		if staleOutcomes+staleWUs > 0 {
			fmt.Printf("  stale candidates: %d outcome(s), %d workunit(s)\n", staleOutcomes, staleWUs)
			for _, c := range candidates {
				fmt.Printf("    %-14s %-9s %-9s idle since %-25s %s\n",
					c.id, c.entityType, c.status, c.updatedAt.UTC().Format(time.RFC3339), c.title)
			}
		}
		fmt.Println("pass --confirm to execute")
		return 0
	}

	if staleErr != nil {
		fmt.Fprintf(os.Stderr, "wms gc: counting stale entities: %v\n", staleErr)
		return 1
	}

	// --confirm only gets an operator to the door — gc is destructive
	// (abandoned status + journal entries, no undo) and this second gate
	// is what makes it interactive-only: a script or unattended agent that
	// passes --confirm still hits a stdin prompt it cannot answer, unlike
	// review-sweep's config+flag gate (wms_review_sweep.go), which is safe
	// to run from a timer by design. See confirmGCInteractive.
	if err := confirmGCInteractive(staleOutcomes, staleWUs); err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: %v\n", err)
		return 1
	}

	var totalIntervals int64

	n, err := s.CloseIntervalsOnTerminalEntities(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: terminal entities: %v\n", err)
		return 1
	}
	totalIntervals += n
	if n > 0 {
		fmt.Printf("phase 1: closed %d interval(s) on terminal entities\n", n)
	}

	n, err = s.CloseIntervalsForClosedSessions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: closed sessions: %v\n", err)
		return 1
	}
	totalIntervals += n
	if n > 0 {
		fmt.Printf("phase 2: closed %d interval(s) on closed sessions\n", n)
	}

	// ExceptLiveLead: an in-process teammate whose own session went quiet
	// while idle must not have its interval closed while its lead session
	// is plainly still connected (wh2-idle-teammate-exemption).
	n, err = s.CloseIntervalsForStaleSessionsExceptLiveLead(ctx, staleThreshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: stale sessions: %v\n", err)
		return 1
	}
	totalIntervals += n
	if n > 0 {
		fmt.Printf("phase 3: closed %d interval(s) on stale sessions\n", n)
	}

	// Phase 4: close stale non-terminal entities as abandoned. Phase 5:
	// drain their own open intervals in the same run — LF-CLI-2 (VERIFY.md
	// Addendum §E): nothing previously closed the interval of an entity gc
	// itself just abandoned; it stayed open until some later run's phase 1
	// happened to catch it, silently inflating that entity's attributed
	// cost in between. Reuses phase 1's exact store method, not a new
	// query — CloseIntervalsOnTerminalEntities matches on current status,
	// so calling it again here catches every entity phase 4 just flipped
	// to abandoned that phase 1 (which ran first) could not have seen yet.
	closedEntities, drainedIntervals, err := closeStaleEntitiesAndDrain(ctx, s, staleThreshold, *olderThan, hostForJournal())
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: drain abandoned entities: %v\n", err)
		if closedEntities > 0 {
			// closeStaleEntities' status/journal writes already committed
			// before the drain call failed — durable, not rolled back — so
			// say so rather than letting a downstream error swallow the
			// report of upstream work that succeeded.
			fmt.Fprintf(os.Stderr, "wms gc: phase 4 closed %d entity(ies) as abandoned before this error; those writes are durable, only the interval drain failed\n", closedEntities)
		}
		return 1
	}
	totalIntervals += drainedIntervals
	if drainedIntervals > 0 {
		// CloseIntervalsOnTerminalEntities matches on current status across
		// the whole table, not on what phase 4 itself just closed — an
		// entity another agent closed over MCP mid-run lands in this count
		// too. "newly-terminal" is accurate in both cases; "abandoned this
		// run by gc" would overclaim on a busy hub (redteam review).
		fmt.Printf("phase 5: closed %d interval(s) on newly-terminal entities\n", drainedIntervals)
	}

	if totalIntervals == 0 && closedEntities == 0 {
		fmt.Println("nothing to collect")
	} else {
		fmt.Printf("gc complete: %d interval(s) drained, %d entity(ies) closed\n", totalIntervals, closedEntities)
	}
	return 0
}

// staleCandidate is one line of countStaleEntities' listing output — enough
// for an operator to identify exactly what a dry-run gc run would abandon
// (previously only aggregate counts were shown, giving no way to review the
// actual blast radius before typing "yes" at confirmGCInteractive).
type staleCandidate struct {
	entityType string
	id         string
	title      string
	status     string
	updatedAt  time.Time
}

// countStaleEntities returns a best-effort partial count/listing alongside
// the first error it hit (if any) — it keeps scanning the remaining outcomes
// after a ListWorkUnits failure rather than aborting, so a caller that only
// wants a lenient dry-run preview can ignore the error and use the partial
// numbers. A caller feeding these counts into an irreversible action (the
// --confirm path's blast-radius warning) must check the error instead: a
// partial count there is a misleading undercount, not a usable approximation.
//
// An Outcome candidate is gated by the same outcomeHasLiveDescendant walk
// closeStaleEntities' abandon path uses (wms_descendant_walk.go, shared with
// wms review-sweep) — not just its own direct WorkUnits — so this preview
// never overclaims what the real run would do: an outcome the walk finds a
// live or unverifiable descendant under is silently excluded here exactly as
// it would be skipped there, rather than counted as "would abandon" and then
// quietly not abandoned.
func countStaleEntities(ctx context.Context, s interface {
	ListOutcomes(ctx context.Context, parentID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error)
	descendantWalkReader
}, threshold time.Time) (int, int, []staleCandidate, error) {
	outcomes, err := s.ListOutcomes(ctx, "", nil, "", "")
	if err != nil {
		return 0, 0, nil, fmt.Errorf("list outcomes: %w", err)
	}
	staleOutcomes := 0
	staleWUs := 0
	var candidates []staleCandidate
	var firstErr error
	for _, o := range outcomes {
		wus, err := s.ListWorkUnits(ctx, o.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list workunits for outcome %s: %w", o.ID, err)
			}
			continue
		}
		hasActiveChild := false
		for _, wu := range wus {
			if !wms.IsTerminal(wms.EntityWorkUnit, wu.Status) && wu.UpdatedAt.Before(threshold) {
				staleWUs++
				candidates = append(candidates, staleCandidate{wms.EntityWorkUnit, wu.ID, wu.Title, wu.Status, wu.UpdatedAt})
			}
			if !wms.IsTerminal(wms.EntityWorkUnit, wu.Status) && !wu.UpdatedAt.Before(threshold) {
				hasActiveChild = true
			}
		}
		if !wms.IsTerminal(wms.EntityOutcome, o.Status) && o.UpdatedAt.Before(threshold) && !hasActiveChild {
			live, walkErr := outcomeHasLiveDescendant(ctx, s, o.ID)
			if walkErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("check descendants of outcome %s: %w", o.ID, walkErr)
				}
				continue
			}
			if live {
				continue
			}
			staleOutcomes++
			candidates = append(candidates, staleCandidate{wms.EntityOutcome, o.ID, o.Title, o.Status, o.UpdatedAt})
		}
	}
	return staleOutcomes, staleWUs, candidates, firstErr
}

func closeStaleEntities(ctx context.Context, s interface {
	ListOutcomes(ctx context.Context, parentID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error)
	UpdateOutcomeStatus(ctx context.Context, id, status string) error
	UpdateWorkUnitStatus(ctx context.Context, id, status string) error
	WriteJournalEntry(ctx context.Context, entry wms.JournalEntry) error
	descendantWalkReader
}, threshold time.Time, thresholdStr string, host string) int {
	outcomes, err := s.ListOutcomes(ctx, "", nil, "", "")
	if err != nil {
		return 0
	}
	closed := 0
	for _, o := range outcomes {
		wus, err := s.ListWorkUnits(ctx, o.ID)
		if err != nil {
			continue
		}
		hasActiveChild := false
		for _, wu := range wus {
			if !wms.IsTerminal(wms.EntityWorkUnit, wu.Status) && wu.UpdatedAt.Before(threshold) {
				priorStatus := wu.Status
				if err := s.UpdateWorkUnitStatus(ctx, wu.ID, wms.StatusAbandoned); err != nil {
					fmt.Fprintf(os.Stderr, "wms gc: close workunit %s: %v\n", wu.ID, err)
					continue
				}
				notes := fmt.Sprintf("closed by `wms gc`: idle since %s (threshold %s)",
					wu.UpdatedAt.UTC().Format(time.RFC3339), thresholdStr)
				entry := wms.JournalEntry{
					EntityType: wms.EntityWorkUnit, EntityID: wu.ID,
					Field: "status", OldValue: priorStatus, NewValue: wms.StatusAbandoned,
					Notes: notes, Host: host, AgentID: "wms-gc",
				}
				if err := wms.RecordMutation(ctx, s, entry); err != nil {
					fmt.Fprintf(os.Stderr, "wms gc: record mutation for %s: %v\n", wu.ID, err)
				}
				fmt.Printf("phase 4: closed workunit %s as abandoned (%s)\n", wu.ID, wu.Title)
				closed++
			} else if !wms.IsTerminal(wms.EntityWorkUnit, wu.Status) {
				hasActiveChild = true
			}
		}
		if !wms.IsTerminal(wms.EntityOutcome, o.Status) && o.UpdatedAt.Before(threshold) && !hasActiveChild {
			// hasActiveChild above only guards o's DIRECT WorkUnits — a
			// nested child Outcome (terminal itself or not) can still hold
			// live work further down the subtree, invisible to that check.
			// outcomeHasLiveDescendant (wms_descendant_walk.go, shared with
			// wms review-sweep) walks the FULL subtree — including this
			// outcome's own WorkUnits again, now reflecting the abandons
			// just written above in this same pass — before this outcome is
			// allowed to close. Fails closed: an unverifiable subtree is
			// treated the same as a live one, skipped rather than abandoned.
			live, walkErr := outcomeHasLiveDescendant(ctx, s, o.ID)
			if walkErr != nil {
				fmt.Fprintf(os.Stderr, "wms gc: outcome %s: %s\n", o.ID, walkErrorReason(walkErr))
				continue
			}
			if live {
				continue
			}
			priorStatus := o.Status
			if err := s.UpdateOutcomeStatus(ctx, o.ID, wms.StatusAbandoned); err != nil {
				fmt.Fprintf(os.Stderr, "wms gc: close outcome %s: %v\n", o.ID, err)
				continue
			}
			notes := fmt.Sprintf("closed by `wms gc`: idle since %s (threshold %s)",
				o.UpdatedAt.UTC().Format(time.RFC3339), thresholdStr)
			entry := wms.JournalEntry{
				EntityType: wms.EntityOutcome, EntityID: o.ID,
				Field: "status", OldValue: priorStatus, NewValue: wms.StatusAbandoned,
				Notes: notes, Host: host, AgentID: "wms-gc",
			}
			if err := wms.RecordMutation(ctx, s, entry); err != nil {
				fmt.Fprintf(os.Stderr, "wms gc: record mutation for %s: %v\n", o.ID, err)
			}
			fmt.Printf("phase 4: closed outcome %s as abandoned (%s)\n", o.ID, o.Title)
			closed++
		}
	}
	return closed
}

// closeStaleEntitiesAndDrain runs phase 4 (closeStaleEntities) then, in the
// same call, drains the open intervals of whatever it just abandoned —
// the same store method phase 1 already uses, called a second time so it
// also catches entities that only became terminal during this run (LF-CLI-2,
// VERIFY.md Addendum §E). One seam so both counts are testable against a
// fake store — same testability pattern as closeStaleEntities and
// closeEntity in wms_close.go.
func closeStaleEntitiesAndDrain(ctx context.Context, s interface {
	ListOutcomes(ctx context.Context, parentID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error)
	UpdateOutcomeStatus(ctx context.Context, id, status string) error
	UpdateWorkUnitStatus(ctx context.Context, id, status string) error
	WriteJournalEntry(ctx context.Context, entry wms.JournalEntry) error
	CloseIntervalsOnTerminalEntities(ctx context.Context) (int64, error)
	descendantWalkReader
}, threshold time.Time, thresholdStr string, host string) (closedEntities int, drainedIntervals int64, err error) {
	closedEntities = closeStaleEntities(ctx, s, threshold, thresholdStr, host)
	drainedIntervals, err = s.CloseIntervalsOnTerminalEntities(ctx)
	return closedEntities, drainedIntervals, err
}

// confirmGCInteractive is gc's safety gate on top of --confirm: it refuses
// to proceed unless stdin is a real terminal AND the operator types exactly
// "yes" in response to a printed blast-radius warning. This makes gc
// impossible to run unattended or scripted — a piped/redirected stdin (cron,
// CI, another agent) is refused outright before the prompt is even printed,
// so there is no way to "yes | teamster wms gc --confirm" past it.
func confirmGCInteractive(staleOutcomes, staleWUs int) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("gc requires interactive confirmation at a terminal; stdin is not a TTY (refusing to run unattended or scripted)")
	}

	fmt.Printf("This will abandon %d outcome(s) and %d workunit(s). This is irreversible.\n", staleOutcomes, staleWUs)
	fmt.Print("Type 'yes' to proceed: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return fmt.Errorf("aborted: confirmation not received")
	}
	return nil
}

func parseDurationWithDays(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}
