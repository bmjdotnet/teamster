package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

func runWMSDrain(args []string) int {
	fs := flag.NewFlagSet("teamster wms drain", flag.ContinueOnError)
	session := fs.String("session", "", "drain intervals for a specific session")
	olderThan := fs.String("older-than", "", "drain intervals on sessions idle longer than duration (e.g. 24h)")
	dryRun := fs.Bool("dry-run", false, "preview what would be drained (this is the default)")
	confirm := fs.Bool("confirm", false, "actually execute the drain")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// dry-run is the default; --confirm overrides it
	isDryRun := !*confirm
	_ = *dryRun // parsed for explicitness but dry-run is default

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms drain: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()
	var totalClosed int64

	if *session != "" {
		return drainSession(ctx, s, *session, isDryRun)
	}

	if *olderThan != "" {
		d, err := time.ParseDuration(*olderThan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms drain: --older-than: %v\n", err)
			return 1
		}
		threshold := time.Now().UTC().Add(-d)

		if isDryRun {
			fmt.Printf("[dry-run] would close intervals on sessions idle since %s\n",
				threshold.Local().Format("2006-01-02 15:04:05"))
			return 0
		}

		n, err := s.CloseIntervalsForStaleSessions(ctx, threshold)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms drain: stale sessions: %v\n", err)
			return 1
		}
		totalClosed += n
		fmt.Printf("closed %d interval(s) on sessions idle since %s\n",
			n, threshold.Local().Format("2006-01-02 15:04:05"))
		return 0
	}

	// Default: drain done entities, then closed sessions
	if isDryRun {
		fmt.Println("[dry-run] would drain intervals on:")
		fmt.Println("  - entities in terminal (done) status")
		fmt.Println("  - sessions marked closed")
		fmt.Println("pass --confirm to execute")
		return 0
	}

	n, err := s.CloseIntervalsOnTerminalEntities(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms drain: terminal entities: %v\n", err)
		return 1
	}
	totalClosed += n
	if n > 0 {
		fmt.Printf("closed %d interval(s) on terminal entities\n", n)
	}

	n, err = s.CloseIntervalsForClosedSessions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms drain: closed sessions: %v\n", err)
		return 1
	}
	totalClosed += n
	if n > 0 {
		fmt.Printf("closed %d interval(s) on closed sessions\n", n)
	}

	if totalClosed == 0 {
		fmt.Println("no open intervals to drain")
	} else {
		fmt.Printf("total: %d interval(s) closed\n", totalClosed)
	}
	return 0
}

func drainSession(ctx context.Context, s interface {
	CloseSessionIntervals(ctx context.Context, sessionID, agentName string, at time.Time) (int64, error)
}, sessionID string, dryRun bool) int {
	if dryRun {
		fmt.Printf("[dry-run] would close all open intervals for session %s\n", sessionID)
		return 0
	}

	n, err := s.CloseSessionIntervals(ctx, sessionID, "", time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms drain: session %s: %v\n", sessionID, err)
		return 1
	}
	fmt.Printf("closed %d interval(s) for session %s\n", n, sessionID)
	return 0
}
