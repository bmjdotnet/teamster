package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/bmjdotnet/teamster/internal/wms"
)

func runWMSClose(args []string) int {
	fs := flag.NewFlagSet("teamster wms close", flag.ContinueOnError)
	resolution := fs.String("resolution", "abandoned", "resolution: abandoned (status=abandoned) or achieved (status=done)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster wms close <entity-id> [--resolution abandoned|achieved]")
		return 2
	}
	entityID := fs.Arg(0)

	// Reject an unrecognized --resolution before touching the store at all —
	// same fail-fast order the CLI has always had. closeEntity below also
	// validates internally (via the same resolveCloseTarget, not a second
	// hardcoded check), so this is defense-in-depth for the CLI's UX, not a
	// second independent source of truth.
	if _, _, err := resolveCloseTarget(*resolution); err != nil {
		fmt.Fprintf(os.Stderr, "wms close: %v\n", err)
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

	targetStatus, wroteTag, err := closeEntity(ctx, s, entityType, entityID, *resolution, hostForJournal(), agentIDForClose())
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms close: %v\n", err)
		return 1
	}

	n, err := s.CloseIntervalsOnTerminalEntities(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wms close: drain intervals: %v\n", err)
		return 1
	}

	if wroteTag {
		fmt.Printf("closed %s %s as %s (resolution=%s)", entityType, entityID, targetStatus, *resolution)
	} else {
		fmt.Printf("closed %s %s as %s", entityType, entityID, targetStatus)
	}
	if n > 0 {
		fmt.Printf(", drained %d interval(s)", n)
	}
	fmt.Println()
	return 0
}

// resolveCloseTarget maps a --resolution value to the status wms_close
// writes and whether the resolution tag should also be written. Returns an
// error for anything other than "abandoned"/"achieved" — a caller can never
// silently close an entity under an unrecognized value.
func resolveCloseTarget(resolution string) (targetStatus string, writeResolutionTag bool, err error) {
	switch resolution {
	case "abandoned":
		return wms.StatusAbandoned, false, nil
	case "achieved":
		return wms.StatusDone, true, nil
	default:
		return "", false, fmt.Errorf("--resolution must be 'abandoned' or 'achieved', got %q", resolution)
	}
}

// closeEntity performs the actual close: rejects an unrecognized resolution
// before any store write (lead ruling on redteam MAJOR 1), then fetches the
// entity's prior status and rejects a prior→target edge the engine's own
// transition table (wms.ValidTransition) does not permit — an already-
// terminal entity (abandoned→*, done→abandoned, done→done) — before writing
// anything (LF-CLI-1). Only then does it write the target status, the
// resolution tag when applicable, and the audit-trail journal entry (WP10
// path 2). Takes a narrow interface — same testability pattern as
// closeStaleEntities in wms_gc.go — so the whole close decision can be
// exercised against a fake store instead of a live MySQL fixture (lead
// ruling on redteam MINOR 3).
func closeEntity(ctx context.Context, s interface {
	GetOutcome(ctx context.Context, id string) (*wms.Outcome, error)
	GetWorkUnit(ctx context.Context, id string) (*wms.WorkUnit, error)
	UpdateOutcomeStatus(ctx context.Context, id, status string) error
	UpdateWorkUnitStatus(ctx context.Context, id, status string) error
	TagEntity(ctx context.Context, entityType, entityID, tagKey, tagValue, source, description string) error
	WriteJournalEntry(ctx context.Context, entry wms.JournalEntry) error
}, entityType, entityID, resolution, host, agentID string) (targetStatus string, wroteTag bool, err error) {
	targetStatus, wroteTag, err = resolveCloseTarget(resolution)
	if err != nil {
		return "", false, err
	}

	var priorStatus string
	switch entityType {
	case "outcome":
		o, gerr := s.GetOutcome(ctx, entityID)
		if gerr != nil {
			return "", false, gerr
		}
		priorStatus = o.Status
	case "workunit":
		wu, gerr := s.GetWorkUnit(ctx, entityID)
		if gerr != nil {
			return "", false, gerr
		}
		priorStatus = wu.Status
	}

	// Reject an edge the engine's own transition table doesn't have — the
	// same predicate the MCP status-update tools use (wms.ValidTransition),
	// not a hand-written list, so this can't drift from the table (kit's
	// incomplete-predicate failure pattern). Covers already-terminal
	// entities (abandoned→*, done→done) and specifically done→abandoned,
	// which must go through the review reopen edge first (R2). It also
	// covers pending/on_hold→done: neither has a direct edge to done (only
	// active/review/blocked do), an asymmetry the table already had before
	// this fix — the CLI just never checked it (redteam review finding: the
	// rejection must name a route the CLI-only user can actually take, not
	// just state the table fact).
	if !wms.ValidTransition(entityType, priorStatus, targetStatus) {
		statusTool := "wms_updateOutcomeStatus"
		if entityType == wms.EntityWorkUnit {
			statusTool = "wms_updateWorkUnitStatus"
		}
		msg := fmt.Sprintf("%s %s is %s; no %s→%s transition", entityType, entityID, priorStatus, priorStatus, targetStatus)
		switch {
		case priorStatus == wms.StatusDone:
			msg = fmt.Sprintf("%s (reopen it first: %s → review)", msg, statusTool)
		case priorStatus != wms.StatusAbandoned:
			msg = fmt.Sprintf("%s (activate it first: %s → active, then close)", msg, statusTool)
		}
		return "", false, fmt.Errorf("%s", msg)
	}

	switch entityType {
	case "outcome":
		if uerr := s.UpdateOutcomeStatus(ctx, entityID, targetStatus); uerr != nil {
			return "", false, fmt.Errorf("update outcome status: %w", uerr)
		}
	case "workunit":
		if uerr := s.UpdateWorkUnitStatus(ctx, entityID, targetStatus); uerr != nil {
			return "", false, fmt.Errorf("update workunit status: %w", uerr)
		}
	}

	// The status now carries the abandoned/achieved distinction by itself
	// (R3); writing resolution:abandoned on top would just record the same
	// fact twice. Only "achieved" still needs the tag.
	if wroteTag {
		if terr := s.TagEntity(ctx, entityType, entityID, "resolution", resolution, "manual", ""); terr != nil {
			return "", false, fmt.Errorf("tag resolution: %w", terr)
		}
	}

	notes := fmt.Sprintf("closed via `teamster wms close --resolution %s`", resolution)
	entry := wms.JournalEntry{
		EntityType: entityType, EntityID: entityID,
		Field: "status", OldValue: priorStatus, NewValue: targetStatus,
		Notes: notes, Host: host, AgentID: agentID,
	}
	if rerr := wms.RecordMutation(ctx, s, entry); rerr != nil {
		fmt.Fprintf(os.Stderr, "wms close: record mutation: %v\n", rerr)
		// do not fail the command over an audit-write failure — the status
		// change already succeeded; matches the existing degrade-don't-block
		// convention used by TransitionEventRecord failures elsewhere.
	}

	return targetStatus, wroteTag, nil
}

// hostForJournal returns the host identity for a journal entry written by a
// standalone CLI command (wms close, wms gc — neither has an MCP request's
// p.Meta.Host to draw from). Reproduces config.Load().Host's own derivation
// exactly — a TEAMSTER_HOST override, else os.Hostname(), else "localhost"
// (config.go's Default()/Load()) — without calling config.Load() itself,
// which also parses teamster.yaml and TEAMSTER_STORE_DSN and creates
// DataDir as a side effect (os.MkdirAll), none of which these two commands
// need just to name a host. Honoring TEAMSTER_HOST (CLAUDE.md: "carries the
// short hostname so the hub can attribute events") is what makes these rows
// group with every other row from the same machine instead of recording a
// second spelling of the hostname (redteam finding).
func hostForJournal() string {
	if v := os.Getenv("TEAMSTER_HOST"); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

// agentIDForClose names the operator running `teamster wms close` on the
// journal row's AgentID field — this command has no MCP session/agent
// identity to draw from. Includes the OS user when cheaply available
// (os/user.Current() is a local syscall, no network or file I/O), unlike
// `wms gc`, whose journal rows always carry the fixed identity `wms-gc`
// regardless of which operator typed the interactive "yes" that ran it
// (lead ruling) — there is no scheduled `wms gc` timer to attribute to
// instead; only `wms review-sweep` has one.
func agentIDForClose() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return fmt.Sprintf("wms-close (%s)", u.Username)
	}
	return "wms-close"
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
