package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func runWMSBackfill(args []string) int {
	fs := flag.NewFlagSet("teamster wms backfill", flag.ContinueOnError)
	eventsPath := fs.String("events", "", "path to events.jsonl (default: $TEAMSTER_BASEDIR/var/events.jsonl)")
	dryRun := fs.Bool("dry-run", false, "preview what would be changed (this is the default)")
	confirm := fs.Bool("confirm", false, "actually execute the backfill")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	isDryRun := !*confirm
	_ = *dryRun

	jsonlPath := *eventsPath
	if jsonlPath == "" {
		jsonlPath = resolveEventsPath()
	}
	if jsonlPath == "" {
		fmt.Fprintln(os.Stderr, "wms backfill: cannot find events.jsonl; use --events=PATH")
		return 1
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms backfill: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	orphans, err := s.OrphanIntervals(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms backfill: load orphans: %v\n", err)
		return 1
	}
	if len(orphans) == 0 {
		fmt.Println("no orphaned intervals found")
		return 0
	}
	fmt.Printf("found %d orphaned interval(s)\n", len(orphans))

	parsed, err := parseJSONL(jsonlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms backfill: parse JSONL: %v\n", err)
		return 1
	}
	fmt.Printf("parsed %d WMSStatusChange, %d creation, %d PreToolUse WMS, %d sessions\n",
		len(parsed.statusChanges), len(parsed.creates), len(parsed.preToolUses), len(parsed.lastEventBySession))

	plan := buildBackfillPlan(orphans, parsed)

	matched := 0
	skipped := 0
	for _, p := range plan {
		if p.skip {
			skipped++
			continue
		}
		matched++
	}

	if isDryRun {
		fmt.Printf("\n[dry-run] would backfill %d interval(s):\n", matched)
		for _, p := range plan {
			if p.skip {
				continue
			}
			parts := []string{fmt.Sprintf("  %s %s (id=%d):", p.orphan.EntityType, p.orphan.EntityID, p.orphan.ID)}
			if p.sessionID != "" {
				parts = append(parts, fmt.Sprintf("session_id=%s", p.sessionID))
			}
			if p.agentName != "" {
				parts = append(parts, fmt.Sprintf("agent=%s", p.agentName))
			}
			if !p.endedAt.IsZero() {
				parts = append(parts, fmt.Sprintf("ended_at=%s (%s)", p.endedAt.Format(time.RFC3339), p.closeSource))
			}
			fmt.Println(strings.Join(parts, " "))
		}
		if skipped > 0 {
			fmt.Printf("\n%d interval(s): no matching event found (skipped)\n", skipped)
			for _, p := range plan {
				if !p.skip {
					continue
				}
				fmt.Printf("  %s %s (id=%d, state=%s, started=%s): %s\n",
					p.orphan.EntityType, p.orphan.EntityID, p.orphan.ID,
					p.orphan.State, p.orphan.StartedAt.Format(time.RFC3339), p.skipReason)
			}
		}
		return 0
	}

	applied := 0
	errCount := 0
	for _, p := range plan {
		if p.skip {
			continue
		}
		if err := applyBackfill(ctx, s, p); err != nil {
			fmt.Fprintf(os.Stderr, "  error: %s %s (id=%d): %v\n", p.orphan.EntityType, p.orphan.EntityID, p.orphan.ID, err)
			errCount++
			continue
		}
		applied++
	}

	fmt.Printf("\nbackfill complete: %d applied, %d skipped, %d errors\n", applied, skipped, errCount)
	if errCount > 0 {
		return 1
	}
	return 0
}

func resolveEventsPath() string {
	basedir := os.Getenv("TEAMSTER_BASEDIR")
	if basedir != "" {
		p := filepath.Join(basedir, "var", "events.jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, "teamster", "var", "events.jsonl")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

type statusChangeEvent struct {
	entityType string
	entityID   string
	oldStatus  string
	newStatus  string
	sessionID  string
	agentName  string
	ts         time.Time
}

type preToolEvent struct {
	entityID  string
	sessionID string
	agentName string
	ts        time.Time
}

type createEvent struct {
	entityType string
	entityID   string
	sessionID  string
	agentName  string
	ts         time.Time
}

type backfillAction struct {
	orphan      store.Interval
	sessionID   string
	agentName   string
	endedAt     time.Time
	closeSource string
	skip        bool
	skipReason  string
}

// statusChangeRe parses: "entity_type entity_id: old_status → new_status"
var statusChangeRe = regexp.MustCompile(`^(\w+)\s+(\S+):\s+(\S+)\s+→\s+(\S+)$`)

// autoCompleteRe parses: "entity_type entity_id auto-completed (rollup)"
var autoCompleteRe = regexp.MustCompile(`^(\w+)\s+(\S+)\s+auto-completed`)

// createToolToEntityType maps MCP creation tool names to entity types.
var createToolToEntityType = map[string]string{
	"mcp__wms__wms_createOutcome":  "outcome",
	"mcp__wms__wms_createWorkUnit": "workunit",
	"mcp__wms__wms_createProject":  "project",
	"mcp__wms__wms_createGoal":     "goal",
	"mcp__wms__wms_createTask":     "task",
	"mcp__wms__wms_createWorkItem": "workitem",
}

type parsedEvents struct {
	statusChanges      []statusChangeEvent
	preToolUses        []preToolEvent
	creates            []createEvent
	lastEventBySession map[string]time.Time
}

func parseJSONL(path string) (*parsedEvents, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	p := &parsedEvents{
		lastEventBySession: make(map[string]time.Time),
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}

		event, _ := raw["event"].(string)
		tsStr, _ := raw["ts"].(string)
		if tsStr == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			ts, err = time.Parse("2006-01-02T15:04:05Z", tsStr)
			if err != nil {
				continue
			}
		}

		session, _ := raw["session"].(string)
		if session != "" {
			if prev, ok := p.lastEventBySession[session]; !ok || ts.After(prev) {
				p.lastEventBySession[session] = ts
			}
		}

		switch event {
		case "WMSStatusChange":
			display, _ := raw["display"].(string)
			agentName, _ := raw["agent_name"].(string)
			if m := statusChangeRe.FindStringSubmatch(display); m != nil {
				p.statusChanges = append(p.statusChanges, statusChangeEvent{
					entityType: m[1],
					entityID:   m[2],
					oldStatus:  m[3],
					newStatus:  m[4],
					sessionID:  session,
					agentName:  agentName,
					ts:         ts,
				})
			} else if m := autoCompleteRe.FindStringSubmatch(display); m != nil {
				p.statusChanges = append(p.statusChanges, statusChangeEvent{
					entityType: m[1],
					entityID:   m[2],
					oldStatus:  "",
					newStatus:  "done",
					sessionID:  session,
					agentName:  agentName,
					ts:         ts,
				})
			}

		case "PreToolUse":
			tool, _ := raw["tool"].(string)
			if !strings.HasPrefix(tool, "mcp__wms__wms_") {
				continue
			}
			display, _ := raw["display"].(string)
			agentName, _ := raw["agent_name"].(string)
			entityID := extractEntityIDFromDisplay(display)

			if entityType, ok := createToolToEntityType[tool]; ok && entityID != "" {
				p.creates = append(p.creates, createEvent{
					entityType: entityType,
					entityID:   entityID,
					sessionID:  session,
					agentName:  agentName,
					ts:         ts,
				})
			}

			if entityID != "" {
				p.preToolUses = append(p.preToolUses, preToolEvent{
					entityID:  entityID,
					sessionID: session,
					agentName: agentName,
					ts:        ts,
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return p, nil
}

// displayEntityRe extracts entity_id from display markup like
// "Updating task __entity-id__ → __status__"
var displayEntityRe = regexp.MustCompile(`__([^_]+)__`)

func extractEntityIDFromDisplay(display string) string {
	m := displayEntityRe.FindStringSubmatch(display)
	if m != nil {
		return m[1]
	}
	return ""
}

// v3IDToV1 maps a v3 entity ID (out-<id>, wu-<id>) to the v1 ID and entity type.
// Returns ("", "") if the ID doesn't follow the v1→v3 naming convention.
func v3IDToV1(entityType, entityID string) (v1Type string, v1ID string) {
	switch entityType {
	case "outcome":
		if strings.HasPrefix(entityID, "out-") {
			return "outcome", strings.TrimPrefix(entityID, "out-")
		}
	case "workunit":
		if strings.HasPrefix(entityID, "wu-") {
			return "workunit", strings.TrimPrefix(entityID, "wu-")
		}
	}
	return "", ""
}

// v1EntityTypes lists the v1 types that map to each v3 type. Projects and goals
// both map to outcomes; tasks and workitems both map to workunits.
var v1EntityTypes = map[string][]string{
	"outcome":  {"project", "goal"},
	"workunit": {"task", "workitem"},
}

func buildBackfillPlan(orphans []store.Interval, p *parsedEvents) []backfillAction {
	sort.Slice(p.statusChanges, func(i, j int) bool {
		return p.statusChanges[i].ts.Before(p.statusChanges[j].ts)
	})
	sort.Slice(p.preToolUses, func(i, j int) bool {
		return p.preToolUses[i].ts.Before(p.preToolUses[j].ts)
	})
	sort.Slice(p.creates, func(i, j int) bool {
		return p.creates[i].ts.Before(p.creates[j].ts)
	})

	type entityKey struct {
		entityType string
		entityID   string
	}

	statusByEntity := make(map[entityKey][]statusChangeEvent)
	for _, e := range p.statusChanges {
		k := entityKey{e.entityType, e.entityID}
		statusByEntity[k] = append(statusByEntity[k], e)
	}

	preToolByEntity := make(map[string][]preToolEvent)
	for _, e := range p.preToolUses {
		preToolByEntity[e.entityID] = append(preToolByEntity[e.entityID], e)
	}

	createByEntity := make(map[entityKey][]createEvent)
	for _, e := range p.creates {
		k := entityKey{e.entityType, e.entityID}
		createByEntity[k] = append(createByEntity[k], e)
	}

	var plan []backfillAction

	// lookupKeys returns the entity keys to search for an orphaned interval:
	// the v3 key first, then v1 equivalents (for migration-backfilled entities
	// whose v3 ID is out-<v1id> or wu-<v1id>).
	lookupKeys := func(o store.Interval) []entityKey {
		keys := []entityKey{{o.EntityType, o.EntityID}}
		if _, v1ID := v3IDToV1(o.EntityType, o.EntityID); v1ID != "" {
			for _, v1Type := range v1EntityTypes[o.EntityType] {
				keys = append(keys, entityKey{v1Type, v1ID})
			}
		}
		return keys
	}

	for _, o := range orphans {
		action := backfillAction{orphan: o}
		keys := lookupKeys(o)

		// Collect all status change events across v3 and v1 keys.
		var events []statusChangeEvent
		for _, k := range keys {
			events = append(events, statusByEntity[k]...)
		}
		sort.Slice(events, func(i, j int) bool {
			return events[i].ts.Before(events[j].ts)
		})

		// Phase 1: try to match a WMSStatusChange event where new_status
		// equals the interval's state, near the interval's started_at.
		openIdx := -1
		bestDelta := time.Duration(math.MaxInt64)
		for i, e := range events {
			if e.newStatus != o.State {
				continue
			}
			delta := absDuration(e.ts.Sub(o.StartedAt))
			if delta < 10*time.Second && delta < bestDelta {
				openIdx = i
				bestDelta = delta
			}
		}

		if openIdx >= 0 {
			openEvent := events[openIdx]
			if openEvent.sessionID != "" {
				action.sessionID = openEvent.sessionID
				action.agentName = openEvent.agentName
			}
			upgradeFromPreTool(&action, preToolByEntity, o.EntityID, openEvent.ts)
		}

		// Phase 2: if no WMSStatusChange matched, try a creation event.
		// First try timestamp proximity; then fall back to entity_id-only
		// matching (for intervals created by the v17/v23 migration backfill
		// with a uniform started_at that doesn't match the original event time).
		if openIdx == -1 {
			for _, k := range keys {
				for _, c := range createByEntity[k] {
					delta := absDuration(c.ts.Sub(o.StartedAt))
					if delta < 10*time.Second {
						if c.sessionID != "" {
							action.sessionID = c.sessionID
						}
						if c.agentName != "" {
							action.agentName = c.agentName
						}
						break
					}
				}
				if action.sessionID != "" {
					break
				}
			}
		}

		// Phase 3: entity_id-only fallback. Migration-backfilled intervals
		// have a uniform started_at that won't match any event timestamp.
		// Use the first creation event or first status change event for
		// the entity to recover session_id.
		if action.sessionID == "" && openIdx == -1 {
			for _, k := range keys {
				if creates := createByEntity[k]; len(creates) > 0 {
					c := creates[0]
					if c.sessionID != "" {
						action.sessionID = c.sessionID
					}
					if c.agentName != "" {
						action.agentName = c.agentName
					}
				}
				if action.sessionID != "" {
					break
				}
			}
		}
		if action.sessionID == "" && openIdx == -1 && len(events) > 0 {
			e := events[0]
			if e.sessionID != "" {
				action.sessionID = e.sessionID
				action.agentName = e.agentName
			}
		}

		if action.sessionID == "" {
			action.skip = true
			if openIdx == -1 {
				action.skipReason = fmt.Sprintf("no matching event for entity %s",
					o.EntityID)
			} else {
				action.skipReason = "matched event but no session_id available"
			}
			plan = append(plan, action)
			continue
		}

		// Find the close time for open intervals.
		if o.EndedAt == nil {
			closeFound := false
			searchStart := 0
			if openIdx >= 0 {
				searchStart = openIdx + 1
			}
			for i := searchStart; i < len(events); i++ {
				if events[i].oldStatus == o.State && events[i].ts.After(o.StartedAt) {
					action.endedAt = events[i].ts
					action.closeSource = "next WMSStatusChange"
					closeFound = true
					break
				}
			}

			if !closeFound {
				if lastTS, ok := p.lastEventBySession[action.sessionID]; ok {
					action.endedAt = lastTS
					action.closeSource = "session last event"
				}
			}
		}

		plan = append(plan, action)
	}

	return plan
}

func upgradeFromPreTool(action *backfillAction, preToolByEntity map[string][]preToolEvent, entityID string, refTime time.Time) {
	pts, ok := preToolByEntity[entityID]
	if !ok {
		return
	}
	for _, pt := range pts {
		delta := refTime.Sub(pt.ts)
		if delta >= 0 && delta <= 3*time.Second {
			if pt.sessionID != "" {
				action.sessionID = pt.sessionID
			}
			if pt.agentName != "" {
				action.agentName = pt.agentName
			}
			return
		}
	}
}

func applyBackfill(ctx context.Context, s store.MaintenanceStore, action backfillAction) error {
	if action.endedAt.IsZero() {
		// Session_id/agent_name update only — either no close time was found,
		// or the interval already had an ended_at that doesn't need changing.
		return s.BackfillInterval(ctx, action.orphan.ID, action.sessionID, action.agentName, nil, nil)
	}

	endedAt := action.endedAt
	durationMs := endedAt.Sub(action.orphan.StartedAt).Milliseconds()
	if durationMs < 0 {
		durationMs = 0
	}

	// Try the update; if unique constraint collides, offset by 1µs increments.
	for attempt := 0; attempt < 100; attempt++ {
		err := s.BackfillInterval(ctx, action.orphan.ID, action.sessionID, action.agentName, &endedAt, &durationMs)
		if err == nil {
			return nil
		}
		if errors.Is(err, store.ErrConflict) {
			endedAt = endedAt.Add(time.Microsecond)
			durationMs = endedAt.Sub(action.orphan.StartedAt).Milliseconds()
			if durationMs < 0 {
				durationMs = 0
			}
			continue
		}
		return err
	}
	return fmt.Errorf("unique constraint collision after 100 attempts")
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
