package statusui

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// TestRenderWMSPanel_OnHoldNonzero is the "before/after" AC's after-capture:
// both entity types carry a nonzero on_hold count and both must get their own
// line, without disturbing the existing Open/Done/Abandoned line's values
// (WP3-DESIGN.md §3a: on_hold is an additive breakdown, Open's own value and
// wording are unchanged). Also pins two things a plain substring search
// cannot: the containment marker (touchstone round 1 MAJOR — bare "N
// on_hold" reads as a fourth, disjoint member of the open/done/abandoned
// partition on the line above it, when it is actually a subset of Open) and
// the line's position (round 1 MINOR-2 — every prior assertion here used
// strings.Contains over the whole panel, so a regression that moved both
// on_hold lines to the end of the panel would still have passed).
func TestRenderWMSPanel_OnHoldNonzero(t *testing.T) {
	summary := store.StatusSummary{
		OutcomesOpen: 12, OutcomesDone: 5, OutcomesAbandoned: 2, OutcomesOnHold: 3,
		WorkUnitsOpen: 34, WorkUnitsDone: 10, WorkUnitsAbandoned: 4, WorkUnitsOnHold: 7,
	}
	got := renderWMSPanel(summary, true, 60)

	for _, want := range []string{
		"Outcomes", "12 open", "5 done", "2 abandoned", "3 on_hold (of open)",
		"Work Units", "34 open", "10 done", "4 abandoned", "7 on_hold (of open)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered panel missing %q; got:\n%s", want, got)
		}
	}

	// renderPanel wraps content in a title line plus a lipgloss border, so
	// got's raw line indices are border-dependent (border top, "WMS" title,
	// then content, then border bottom) — filter down to the entity-labeled
	// content lines rather than assume where they start.
	lines := entityLines(got)
	if len(lines) != 4 {
		t.Fatalf("expected 4 entity-labeled lines (summary+on_hold per entity type), got %d:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "Outcomes") || !strings.Contains(lines[0], "open") {
		t.Errorf("line 0 = %q, want the Outcomes open/done/abandoned line", lines[0])
	}
	if !strings.Contains(lines[1], "Outcomes") || !strings.Contains(lines[1], "on_hold (of open)") {
		t.Errorf("line 1 = %q, want the Outcomes on_hold line directly under line 0", lines[1])
	}
	if !strings.Contains(lines[2], "Work Units") || !strings.Contains(lines[2], "open") {
		t.Errorf("line 2 = %q, want the Work Units open/done/abandoned line", lines[2])
	}
	if !strings.Contains(lines[3], "Work Units") || !strings.Contains(lines[3], "on_hold (of open)") {
		t.Errorf("line 3 = %q, want the Work Units on_hold line directly under line 2", lines[3])
	}
}

// entityLines returns the lines of a rendered panel that mention an entity
// label ("Outcomes" or "Work Units"), in order — filtering out the border
// and title lines renderPanel adds around the content this package's own
// renderers produce.
func entityLines(rendered string) []string {
	var out []string
	for _, l := range strings.Split(rendered, "\n") {
		if strings.Contains(l, "Outcomes") || strings.Contains(l, "Work Units") {
			out = append(out, l)
		}
	}
	return out
}

// TestRenderWMSPanel_OnHoldZero_LineOmitted is the "before" capture: with both
// counts at zero, no on_hold line appears at all, and the existing
// Open/Done/Abandoned line is unchanged from today's rendering.
func TestRenderWMSPanel_OnHoldZero_LineOmitted(t *testing.T) {
	summary := store.StatusSummary{
		OutcomesOpen: 12, OutcomesDone: 5, OutcomesAbandoned: 2, OutcomesOnHold: 0,
		WorkUnitsOpen: 34, WorkUnitsDone: 10, WorkUnitsAbandoned: 4, WorkUnitsOnHold: 0,
	}
	got := renderWMSPanel(summary, true, 60)

	if strings.Contains(got, "on_hold") {
		t.Errorf("expected no on_hold line when both counts are zero; got:\n%s", got)
	}
	for _, want := range []string{
		"Outcomes", "12 open", "5 done", "2 abandoned",
		"Work Units", "34 open", "10 done", "4 abandoned",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered panel missing %q; got:\n%s", want, got)
		}
	}
}

// TestRenderWMSPanel_OnHoldGatesIndependently proves the two lines are gated
// separately, not on "either is nonzero": Outcomes has a backlog, WorkUnits
// does not, and only the Outcomes on_hold line may appear.
func TestRenderWMSPanel_OnHoldGatesIndependently(t *testing.T) {
	summary := store.StatusSummary{
		OutcomesOpen: 1, OutcomesOnHold: 3,
		WorkUnitsOpen: 1, WorkUnitsOnHold: 0,
	}
	got := renderWMSPanel(summary, true, 60)

	if !strings.Contains(got, "3 on_hold (of open)") {
		t.Errorf("expected the Outcomes on_hold line; got:\n%s", got)
	}
	// renderPanel's border prefixes every content line with a box-drawing
	// character, not whitespace, so a HasPrefix-after-TrimSpace check against
	// the raw split would never match "Work Units" and would pass vacuously
	// regardless of content — use entityLines, which matches by Contains.
	for _, line := range entityLines(got) {
		if strings.Contains(line, "Work Units") && strings.Contains(line, "on_hold") {
			t.Errorf("Work Units on_hold line present when WorkUnitsOnHold=0: %q", line)
		}
	}
	if got := len(entityLines(renderWMSPanel(summary, true, 60))); got != 3 {
		t.Errorf("expected 3 entity-labeled lines (2 summaries + 1 on_hold), got %d", got)
	}
}

// TestRenderWMSPanel_NoStore is a regression guard: the pre-existing
// "Store unavailable" branch is untouched by this change.
func TestRenderWMSPanel_NoStore(t *testing.T) {
	got := renderWMSPanel(store.StatusSummary{}, false, 60)
	if !strings.Contains(got, "Store unavailable") {
		t.Errorf("expected the no-store message; got:\n%s", got)
	}
	if strings.Contains(got, "on_hold") {
		t.Errorf("no-store panel must not mention on_hold; got:\n%s", got)
	}
}
