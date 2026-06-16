package main

import (
	"context"
	"fmt"
	"os"
)

func runWMSAdopt(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster wms adopt <entity-id>")
		return 2
	}
	entityID := args[0]

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms adopt: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()
	entityType, err := resolveEntityType(ctx, s, entityID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms adopt: %v\n", err)
		return 1
	}

	var title, status, focus string
	switch entityType {
	case "outcome":
		o, err := s.GetOutcome(ctx, entityID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms adopt: %v\n", err)
			return 1
		}
		title = o.Title
		status = o.Status
		focus = o.Focus
	case "workunit":
		wu, err := s.GetWorkUnit(ctx, entityID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wms adopt: %v\n", err)
			return 1
		}
		title = wu.Title
		status = wu.Status
		focus = wu.Focus
	}

	fmt.Printf("Entity:  %s %s\n", entityType, entityID)
	fmt.Printf("Title:   %s\n", title)
	fmt.Printf("Status:  %s\n", status)
	if focus != "" {
		fmt.Printf("Focus:   %s\n", focus)
	}
	fmt.Println()
	fmt.Println("To adopt this entity in a new session, use the WMS MCP tools:")
	fmt.Printf("  wms_setFocus(entityType=%q, entityID=%q, focus=\"...\")\n", entityType, entityID)
	if entityType == "workunit" {
		fmt.Printf("  wms_claimWorkUnit(id=%q, agentID=\"@<your-agent>\")\n", entityID)
	}
	return 0
}
