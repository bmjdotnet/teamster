package wms

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// resolutionTagKey is the lifecycle key that records a successful close-out
// (resolution:achieved). Post-R3, abandonment is carried by the `abandoned`
// status itself, not by this tag — a solo close-out that reaches `done`
// without it leaves the outcome's disposition ambiguous in cost rollups and
// the dashboard.
const resolutionTagKey = "resolution"

// CloseoutWarnings inspects an outcome being transitioned to `done` and returns
// advisory warnings about close-out discipline the transition itself does NOT
// enforce. It is the engine's backstop for close-out bookkeeping that the lead
// might otherwise skip.
//
// The transition is NEVER blocked by these warnings — the caller appends them
// to the success response so a solo agent reads them inline. CloseoutWarnings
// returns nil for a clean close-out (no children pending, resolution tagged).
//
// It only inspects on a terminal transition (newStatus == done or abandoned);
// any other transition returns nil. Store-read failures degrade silently
// (best-effort advisory, never a blocker) — a warning that can't be computed
// is simply not emitted.
func CloseoutWarnings(ctx context.Context, r Reader, outcomeID, newStatus string) []string {
	if newStatus != StatusDone && newStatus != StatusAbandoned {
		return nil
	}

	var warnings []string

	// (a)/(c) Non-terminal child work units — applies to BOTH terminal
	// statuses. An abandoned outcome with open children orphans their cost
	// attribution exactly as a done one does. A `pending` child (never
	// advanced off its initial state) is the frozen-focus signature.
	if units, err := r.ListWorkUnits(ctx, outcomeID); err == nil {
		var open []string
		for _, u := range units {
			if u == nil || IsTerminal(EntityWorkUnit, u.Status) {
				continue
			}
			open = append(open, fmt.Sprintf("%s (%s)", u.ID, u.Status))
		}
		if len(open) > 0 {
			sort.Strings(open)
			warnings = append(warnings, fmt.Sprintf(
				"Outcome %s marked %s but %d work unit(s) are not done: %s — advance or close them, or this work's cost attribution is lost.",
				outcomeID, newStatus, len(open), strings.Join(open, ", ")))
		}
	}

	// (b) Missing resolution tag — DONE ONLY. An outcome reaching `abandoned`
	// no longer needs a resolution:abandoned tag: the status itself now
	// carries that meaning (R3). resolution:achieved still applies to `done`.
	if newStatus == StatusDone {
		if tags, err := r.GetEntityTags(ctx, EntityOutcome, outcomeID); err == nil {
			hasResolution := false
			for _, t := range tags {
				if t.TagKey == resolutionTagKey && t.TagValue != "" {
					hasResolution = true
					break
				}
			}
			if !hasResolution {
				warnings = append(warnings, fmt.Sprintf(
					"Outcome %s closed without a resolution tag — set resolution:achieved so its disposition is recorded.",
					outcomeID))
			}
		}
	}

	return warnings
}

// clearResolutionOnReopen removes a stale `resolution` tag when an entity
// reopens `done → review` — the sole edge leaving `done` (R2). Once an
// entity leaves `done` its resolution no longer holds; left in place, a
// later `review → abandoned` arrives at `abandoned` while still asserting
// `resolution:achieved` (LF-gfx-1). `abandoned` is reachable only from a
// non-terminal status, and `review` is the only non-terminal status
// reachable from `done`, so every path through `done` passes through this
// one edge — clearing here is sufficient; no other transition needs it.
//
// A no-op, with no journal row, when the entity carries no `resolution`
// tag — reopening a close-out that was never tagged is not a mutation.
// Store failures degrade silently, the same swallow-and-log posture as the
// rest of RecordMutation's callers (see evaluateUnblock): a failed clear is
// logged, never a rejected reopen, and nothing is journaled for a clear
// that didn't happen.
func clearResolutionOnReopen(ctx context.Context, store Store, change StatusChange) {
	if change.OldStatus != StatusDone || change.NewStatus != StatusReview {
		return
	}

	tags, err := store.GetEntityTags(ctx, change.EntityType, change.EntityID)
	if err != nil {
		slog.Warn("wms engine: get entity tags for reopen clear", "entity", change.EntityID, "err", err)
		return
	}
	var value string
	for _, t := range tags {
		if t.TagKey == resolutionTagKey && t.TagValue != "" {
			value = t.TagValue
			break
		}
	}
	if value == "" {
		return // no resolution tag bound — nothing to clear, no journal noise
	}

	if err := store.DeleteEntityTag(ctx, change.EntityType, change.EntityID, resolutionTagKey, value); err != nil {
		slog.Warn("wms engine: clear resolution tag on reopen", "entity", change.EntityID, "err", err)
		return
	}

	entry := JournalEntry{
		EntityType: change.EntityType, EntityID: change.EntityID,
		Field: resolutionTagKey, OldValue: value, NewValue: "",
		Notes:     fmt.Sprintf("%s:%s cleared — %s reopened (done → review)", resolutionTagKey, value, change.EntityType),
		SessionID: change.SessionID, AgentID: change.AgentName, Host: change.Host,
	}
	if err := RecordMutation(ctx, store, entry); err != nil {
		slog.Warn("wms engine: record mutation for reopen tag clear", "entity", change.EntityID, "err", err)
	}
}

// FormatCloseoutWarnings renders warnings as a block appended to a tool's
// success message. Returns "" when there are no warnings, so the clean
// close-out response is byte-identical to today's.
func FormatCloseoutWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nclose-out warnings (advisory — the transition succeeded):")
	for _, w := range warnings {
		b.WriteString("\n  - ")
		b.WriteString(w)
	}
	return b.String()
}
