package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bmjdotnet/teamster/internal/wms"
)

func runWMSClose(args []string) int {
	fs := flag.NewFlagSet("teamster wms close", flag.ContinueOnError)
	resolution := fs.String("resolution", "abandoned", "resolution tag: abandoned or achieved")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster wms close <entity-id> [--resolution abandoned|achieved]")
		return 2
	}
	entityID := fs.Arg(0)

	if *resolution != "abandoned" && *resolution != "achieved" {
		fmt.Fprintf(os.Stderr, "wms close: --resolution must be 'abandoned' or 'achieved', got %q\n", *resolution)
		return 1
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms close: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	entityType, err := resolveEntityType(ctx, s, entityID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms close: %v\n", err)
		return 1
	}

	switch entityType {
	case "outcome":
		if err := s.UpdateOutcomeStatus(ctx, entityID, "done"); err != nil {
			fmt.Fprintf(os.Stderr, "wms close: update outcome status: %v\n", err)
			return 1
		}
	case "workunit":
		if err := s.UpdateWorkUnitStatus(ctx, entityID, "done"); err != nil {
			fmt.Fprintf(os.Stderr, "wms close: update workunit status: %v\n", err)
			return 1
		}
	}

	if err := s.TagEntity(ctx, entityType, entityID, "resolution", *resolution, "manual", ""); err != nil {
		fmt.Fprintf(os.Stderr, "wms close: tag resolution: %v\n", err)
		return 1
	}

	n, err := s.CloseIntervalsOnTerminalEntities(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms close: drain intervals: %v\n", err)
		return 1
	}

	fmt.Printf("closed %s %s as done (resolution=%s)", entityType, entityID, *resolution)
	if n > 0 {
		fmt.Printf(", drained %d interval(s)", n)
	}
	fmt.Println()
	return 0
}

func resolveEntityType(ctx context.Context, s wms.Reader, entityID string) (string, error) {
	if strings.HasPrefix(entityID, "wu-") {
		return "workunit", nil
	}
	if strings.HasPrefix(entityID, "oc-") {
		return "outcome", nil
	}
	if _, err := s.GetOutcome(ctx, entityID); err == nil {
		return "outcome", nil
	}
	if _, err := s.GetWorkUnit(ctx, entityID); err == nil {
		return "workunit", nil
	}
	return "", fmt.Errorf("entity %q not found as outcome or workunit", entityID)
}
