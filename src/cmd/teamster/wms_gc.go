package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

func runWMSGC(args []string) int {
	fs := flag.NewFlagSet("teamster wms gc", flag.ContinueOnError)
	olderThan := fs.String("older-than", "7d", "stale threshold for closing entities")
	dryRun := fs.Bool("dry-run", false, "preview what would be collected (this is the default)")
	confirm := fs.Bool("confirm", false, "actually execute the gc")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	isDryRun := !*confirm
	_ = *dryRun

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

	// Phase 1: drain intervals on done entities
	if isDryRun {
		fmt.Println("[dry-run] gc would:")
		fmt.Println("  1. close intervals on terminal entities")
		fmt.Println("  2. close intervals on closed sessions")
		fmt.Printf("  3. close intervals on sessions idle since %s\n",
			staleThreshold.Local().Format("2006-01-02 15:04:05"))
		fmt.Println("  4. close stale non-terminal entities as abandoned")

		staleOutcomes, staleWUs := countStaleEntities(ctx, s, staleThreshold)
		if staleOutcomes+staleWUs > 0 {
			fmt.Printf("  stale candidates: %d outcome(s), %d workunit(s)\n", staleOutcomes, staleWUs)
		}
		fmt.Println("pass --confirm to execute")
		return 0
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

	n, err = s.CloseIntervalsForStaleSessions(ctx, staleThreshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms gc: stale sessions: %v\n", err)
		return 1
	}
	totalIntervals += n
	if n > 0 {
		fmt.Printf("phase 3: closed %d interval(s) on stale sessions\n", n)
	}

	// Phase 4: close stale non-terminal entities as abandoned
	closedEntities := closeStaleEntities(ctx, s, staleThreshold)

	if totalIntervals == 0 && closedEntities == 0 {
		fmt.Println("nothing to collect")
	} else {
		fmt.Printf("gc complete: %d interval(s) drained, %d entity(ies) closed\n", totalIntervals, closedEntities)
	}
	return 0
}

func countStaleEntities(ctx context.Context, s interface {
	ListOutcomes(ctx context.Context, parentID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error)
}, threshold time.Time) (int, int) {
	outcomes, err := s.ListOutcomes(ctx, "", nil, "", "")
	if err != nil {
		return 0, 0
	}
	staleOutcomes := 0
	staleWUs := 0
	for _, o := range outcomes {
		wus, err := s.ListWorkUnits(ctx, o.ID)
		if err != nil {
			continue
		}
		hasActiveChild := false
		for _, wu := range wus {
			if wu.Status != wms.StatusDone && wu.UpdatedAt.Before(threshold) {
				staleWUs++
			}
			if wu.Status != wms.StatusDone && !wu.UpdatedAt.Before(threshold) {
				hasActiveChild = true
			}
		}
		if o.Status != wms.StatusDone && o.UpdatedAt.Before(threshold) && !hasActiveChild {
			staleOutcomes++
		}
	}
	return staleOutcomes, staleWUs
}

func closeStaleEntities(ctx context.Context, s interface {
	ListOutcomes(ctx context.Context, parentID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error)
	UpdateOutcomeStatus(ctx context.Context, id, status string) error
	UpdateWorkUnitStatus(ctx context.Context, id, status string) error
	TagEntity(ctx context.Context, entityType, entityID, tagKey, tagValue, source, description string) error
}, threshold time.Time) int {
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
			if wu.Status != wms.StatusDone && wu.UpdatedAt.Before(threshold) {
				if err := s.UpdateWorkUnitStatus(ctx, wu.ID, "done"); err != nil {
					fmt.Fprintf(os.Stderr, "wms gc: close workunit %s: %v\n", wu.ID, err)
					continue
				}
				s.TagEntity(ctx, "workunit", wu.ID, "resolution", "abandoned", "manual", "") //nolint:errcheck
				fmt.Printf("phase 4: closed workunit %s as abandoned (%s)\n", wu.ID, wu.Title)
				closed++
			} else if wu.Status != wms.StatusDone {
				hasActiveChild = true
			}
		}
		if o.Status != wms.StatusDone && o.UpdatedAt.Before(threshold) && !hasActiveChild {
			if err := s.UpdateOutcomeStatus(ctx, o.ID, "done"); err != nil {
				fmt.Fprintf(os.Stderr, "wms gc: close outcome %s: %v\n", o.ID, err)
				continue
			}
			s.TagEntity(ctx, "outcome", o.ID, "resolution", "abandoned", "manual", "") //nolint:errcheck
			fmt.Printf("phase 4: closed outcome %s as abandoned (%s)\n", o.ID, o.Title)
			closed++
		}
	}
	return closed
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
