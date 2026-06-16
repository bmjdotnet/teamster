package wms

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// resolutionTagKey is the lifecycle key that records WHY an outcome closed
// (resolution:achieved | resolution:abandoned). A solo close-out that omits it
// leaves the outcome's disposition ambiguous in cost rollups and the dashboard.
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
// It only inspects on the terminal transition (newStatus == done); any other
// transition returns nil. Store-read failures degrade silently (best-effort
// advisory, never a blocker) — a warning that can't be computed is simply not
// emitted.
func CloseoutWarnings(ctx context.Context, r Reader, outcomeID, newStatus string) []string {
	if newStatus != StatusDone {
		return nil
	}

	var warnings []string

	// (a)/(c) Non-terminal child work units. A child still pending/active/review
	// when its outcome is marked done means its work-type/phase attribution and
	// its still-open cost interval are about to be orphaned. A `pending` child
	// (never advanced off its initial state) is the frozen-focus signature.
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
				"Outcome %s marked done but %d work unit(s) are not done: %s — advance or close them, or this work's cost attribution is lost.",
				outcomeID, len(open), strings.Join(open, ", ")))
		}
	}

	// (b) Missing resolution tag. An outcome closed without resolution:achieved
	// or resolution:abandoned leaves its disposition unrecorded.
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
				"Outcome %s closed without a resolution tag — set resolution:achieved (or resolution:abandoned) so its disposition is recorded.",
				outcomeID))
		}
	}

	return warnings
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
