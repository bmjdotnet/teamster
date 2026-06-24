package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

func runWMSList(args []string) int {
	fs := flag.NewFlagSet("teamster wms list", flag.ContinueOnError)
	status := fs.String("status", "", "filter by status (e.g. active, pending, done)")
	stale := fs.String("stale", "", "show entities not updated within duration (e.g. 24h)")
	entityType := fs.String("type", "", "filter by type: outcome or workunit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *entityType != "" && *entityType != "outcome" && *entityType != "workunit" {
		fmt.Fprintf(os.Stderr, "wms list: --type must be 'outcome' or 'workunit', got %q\n", *entityType)
		return 1
	}

	var staleThreshold time.Time
	if *stale != "" {
		d, err := time.ParseDuration(*stale)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms list: --stale: %v\n", err)
			return 1
		}
		staleThreshold = time.Now().UTC().Add(-d)
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms list: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	showOutcomes := *entityType == "" || *entityType == "outcome"
	showWorkunits := *entityType == "" || *entityType == "workunit"

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	printed := 0

	if showOutcomes {
		outcomes, err := s.ListOutcomes(ctx, "", nil, "", "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms list: outcomes: %v\n", err)
			return 1
		}

		filtered := filterOutcomes(outcomes, *status, staleThreshold)
		if len(filtered) > 0 {
			fmt.Fprintln(w, "TYPE\tID\tSTATUS\tTITLE\tUPDATED")
			for _, o := range filtered {
				fmt.Fprintf(w, "outcome\t%s\t%s\t%s\t%s\n",
					o.ID, o.Status, truncate(o.Title, 50), o.UpdatedAt.Local().Format("2006-01-02 15:04"))
			}
			printed += len(filtered)
		}
	}

	if showWorkunits {
		outcomes, err := s.ListOutcomes(ctx, "", nil, "", "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms list: outcomes: %v\n", err)
			return 1
		}

		var allWUs []*wms.WorkUnit
		for _, o := range outcomes {
			wus, err := s.ListWorkUnits(ctx, o.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "wms list: workunits for %s: %v\n", o.ID, err)
				return 1
			}
			allWUs = append(allWUs, wus...)
		}

		filtered := filterWorkUnits(allWUs, *status, staleThreshold)
		if len(filtered) > 0 {
			if printed == 0 {
				fmt.Fprintln(w, "TYPE\tID\tSTATUS\tOUTCOME\tTITLE\tUPDATED")
			}
			for _, wu := range filtered {
				fmt.Fprintf(w, "workunit\t%s\t%s\t%s\t%s\t%s\n",
					wu.ID, wu.Status, wu.OutcomeID, truncate(wu.Title, 40), wu.UpdatedAt.Local().Format("2006-01-02 15:04"))
			}
			printed += len(filtered)
		}
	}

	w.Flush() //nolint:errcheck

	if printed == 0 {
		fmt.Fprintln(os.Stderr, "no matching entities found")
	}
	return 0
}

func filterOutcomes(outcomes []*wms.Outcome, status string, staleThreshold time.Time) []*wms.Outcome {
	var out []*wms.Outcome
	for _, o := range outcomes {
		if status != "" && o.Status != status {
			continue
		}
		if status == "" && o.Status == wms.StatusDone {
			continue
		}
		if !staleThreshold.IsZero() && o.UpdatedAt.After(staleThreshold) {
			continue
		}
		out = append(out, o)
	}
	return out
}

func filterWorkUnits(wus []*wms.WorkUnit, status string, staleThreshold time.Time) []*wms.WorkUnit {
	var out []*wms.WorkUnit
	for _, wu := range wus {
		if status != "" && wu.Status != status {
			continue
		}
		if status == "" && wu.Status == wms.StatusDone {
			continue
		}
		if !staleThreshold.IsZero() && wu.UpdatedAt.After(staleThreshold) {
			continue
		}
		out = append(out, wu)
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
